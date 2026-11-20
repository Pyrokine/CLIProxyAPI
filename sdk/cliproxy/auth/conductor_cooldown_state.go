package auth

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const cooldownStatePersistenceTimeout = 5 * time.Second

func authAutoRefreshRestricted(auth *Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled ||
		hasUnauthorizedAuthFailure(auth) || hasQuotaCooldownState(auth.Quota, auth.LastError) {
		return true
	}
	if auth.LastError == nil {
		return false
	}
	scope := auth.StateEffectScope
	if scope == "" {
		scope = auth.LastError.executionState().EffectScope
	}
	if scope != cliproxyexecutor.StateEffectScopeCredential && (scope != "" || len(auth.ModelStates) > 0) {
		return false
	}
	return auth.LastError.HTTPStatus == http.StatusPaymentRequired ||
		auth.LastError.HTTPStatus == http.StatusForbidden
}

// SetCooldownStateStore swaps the independent runtime cooldown state store.
func (m *Manager) SetCooldownStateStore(store CooldownStateStore) {
	if m == nil {
		return
	}
	m.configCooldownMu.Lock()
	m.mu.Lock()
	m.cooldownStore = store
	m.mu.Unlock()
	m.configCooldownMu.Unlock()
}

// SwapCooldownStateStore 保存待清理的旧冷却快照，再发布替换存储
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (m *Manager) SwapCooldownStateStore(ctx context.Context, store CooldownStateStore, persistOld bool) bool {
	if m == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.Lock()
	storeToPersist := m.pendingCooldownStateStore
	if storeToPersist == nil && persistOld {
		storeToPersist = m.cooldownStore
		m.pendingCooldownStateStore = storeToPersist
	}
	m.mu.Unlock()
	if !m.persistCooldownStatesToLocked(ctx, storeToPersist) || ctx.Err() != nil {
		return false
	}
	m.mu.Lock()
	m.cooldownStore = store
	m.mu.Unlock()
	return true
}

// ApplyConfigWithCooldownStateStore serializes a config update with its cooldown
// store transition and persists cleared state to the old store first.
func (m *Manager) ApplyConfigWithCooldownStateStore(
	ctx context.Context,
	cfg *internalconfig.Config,
	store CooldownStateStore,
) bool {
	if m == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return false
	}
	snapshot, err := internalconfig.Clone(cfg)
	if err != nil {
		logEntryWithRequestID(ctx).Warnf("failed to copy runtime config snapshot: %v", err)
		return false
	}
	cfg = snapshot

	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	m.mu.RLock()
	oldStore := m.cooldownStore
	pendingStore := m.pendingCooldownStateStore
	m.mu.RUnlock()
	if pendingStore != nil && pendingStore != oldStore && !m.persistCooldownStatesToLocked(ctx, pendingStore) {
		return false
	}
	if oldStore != nil {
		records := m.cooldownStateRecordsSnapshotForConfig(cfg)
		if store == nil {
			records = nil
		}
		if errSave := oldStore.Save(ctx, records); errSave != nil {
			logEntryWithRequestID(
				ctx,
			).Warnf("failed to persist cooldown state before config transition: %v", errSave)
			return false
		}
	}
	if oldStore == nil && ctx.Err() != nil {
		return false
	}
	m.setConfigSnapshotLocked(cfg)
	m.mu.Lock()
	if m.pendingCooldownStateStore == oldStore {
		m.pendingCooldownStateStore = nil
	}
	m.cooldownStore = store
	m.mu.Unlock()
	return true
}

// PersistCooldownStates writes the current cooldown snapshot using ctx.
func (m *Manager) PersistCooldownStates(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	m.persistCooldownStatesToLocked(ctx, store)
}

func (m *Manager) persistCooldownStatesAfterMutation(ctx context.Context) {
	persistCtx, cancel := newCooldownStatePersistenceContext(ctx)
	defer cancel()
	m.PersistCooldownStates(persistCtx)
}

func newCooldownStatePersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(ctx, cooldownStatePersistenceTimeout)
}

func (m *Manager) persistCooldownStatesToLocked(ctx context.Context, store CooldownStateStore) bool {
	if store == nil {
		return true
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return false
	}
	records := m.cooldownStateRecordsSnapshot()
	if errSave := store.Save(ctx, records); errSave != nil {
		logEntryWithRequestID(ctx).Warnf("failed to persist cooldown state: %v", errSave)
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	m.mu.Lock()
	if m.pendingCooldownStateStore == store {
		m.pendingCooldownStateStore = nil
	}
	m.mu.Unlock()
	return true
}

// RestoreCooldownStates restores unexpired persisted cooldown records into registered auths.
func (m *Manager) RestoreCooldownStates(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}

	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	records, errLoad := store.Load(ctx)
	if errLoad != nil {
		return errLoad
	}

	now := time.Now()
	authLevelRecords := make([]CooldownStateRecord, 0)
	restoredAuthIDs := make(map[string]struct{})
	m.mu.Lock()
	for _, record := range records {
		if strings.TrimSpace(record.Model) == "" {
			authLevelRecords = append(authLevelRecords, record)
			continue
		}
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				restoredAuthIDs[auth.ID] = struct{}{}
			}
		}
	}
	for _, record := range authLevelRecords {
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				restoredAuthIDs[auth.ID] = struct{}{}
			}
		}
	}
	snapshotsByID := make(map[string]*Auth, len(restoredAuthIDs))
	projectionGenerations := make(map[string]uint64, len(restoredAuthIDs))
	for authID := range restoredAuthIDs {
		if auth := m.auths[authID]; auth != nil {
			snapshotsByID[authID] = auth.Clone()
			projectionGenerations[authID] = m.nextAuthGenerationLocked()
		}
	}
	m.mu.Unlock()

	for authID, snapshot := range snapshotsByID {
		m.publishClientModelProjections(snapshot, projectionGenerations[authID], now)
		if m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	if !m.persistCooldownStatesToLocked(ctx, store) {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
		return fmt.Errorf("persist normalized cooldown state")
	}
	return nil
}

func (m *Manager) restoreCooldownRecordLocked(record CooldownStateRecord, now time.Time) bool {
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" || (!record.NextRetryAfter.IsZero() && !record.NextRetryAfter.After(now)) {
		return false
	}
	auth := m.auths[authID]
	terminalRecord := strings.TrimSpace(record.Model) == "" && record.NextRetryAfter.IsZero() &&
		record.LastError != nil && record.LastError.StatusCode() == http.StatusUnauthorized &&
		isInvalidGrantResultError(record.LastError)
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) ||
		((auth.Disabled || auth.Status == StatusDisabled || m.cooldownDisabledForAuth(auth)) && !terminalRecord) {
		return false
	}
	if binding := strings.TrimSpace(record.BindingSHA256); binding != "" &&
		!strings.EqualFold(binding, credentialBindingSHA256(auth)) {
		return false
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	reason := strings.TrimSpace(record.Reason)
	model := strings.TrimSpace(record.Model)
	quota := quotaForCooldownRecord(auth.Provider, record.Quota)
	if quota.Exceeded && quota.NextRecoverAt.IsZero() && !record.NextRetryAfter.IsZero() {
		quota.NextRecoverAt = record.NextRetryAfter
	}
	resetAuthority := record.ResetAuthority
	if resetAuthority == "" {
		resetAuthority = StateResetAuthoritySuccess
	}
	stateGeneration := normalizedStateGeneration(record.StateGeneration)

	if model == "" {
		if normalizedStateGeneration(auth.StateGeneration) > stateGeneration {
			return false
		}
		effectScope := record.EffectScope
		if effectScope == "" {
			effectScope = cliproxyexecutor.StateEffectScopeCredential
		}
		effectKey := strings.TrimSpace(record.EffectKey)
		if effectKey == "" {
			effectKey = auth.ID
		}
		auth.Unavailable = true
		if !auth.Disabled && auth.Status != StatusDisabled {
			auth.Status = StatusError
		}
		auth.NextRetryAfter = record.NextRetryAfter
		applyCooldownFields(&auth.Quota, quota)
		if quota.ObservedAt.After(auth.Quota.ObservedAt) {
			auth.Quota.ObservedAt = quota.ObservedAt
			auth.Quota.Signals = quota.Signals
		}
		auth.StateEffectScope = effectScope
		auth.StateEffectKey = effectKey
		auth.ResetAuthority = resetAuthority
		auth.StateGeneration = max(normalizedStateGeneration(auth.StateGeneration), stateGeneration)
		auth.UpdatedAt = updatedAt
		if reason != "" {
			auth.StatusMessage = reason
		}
		auth.LastError = cloneError(record.LastError)
		if auth.LastError != nil {
			auth.LastError.executionState().StateGeneration = auth.StateGeneration
		}
		return true
	}

	effectScope := record.EffectScope
	if effectScope == "" {
		if strings.TrimSpace(quota.Family) != "" {
			effectScope = cliproxyexecutor.StateEffectScopeQuotaFamily
		} else {
			effectScope = cliproxyexecutor.StateEffectScopeExactModel
		}
	}
	effectKey := strings.TrimSpace(record.EffectKey)
	if effectKey == "" {
		if effectScope == cliproxyexecutor.StateEffectScopeQuotaFamily {
			effectKey = strings.TrimSpace(quota.Family)
		}
		if effectKey == "" {
			effectKey = canonicalModelKey(model)
		}
	}
	if effectScope == cliproxyexecutor.StateEffectScopeExactModel &&
		canonicalModelKey(effectKey) == canonicalModelKey(model) {
		effectKey = canonicalModelKey(model)
	}
	if quota.Exceeded {
		if quota.Generation == 0 {
			quota.Generation = stateGeneration
		}
		if quota.ResetAuthority == "" {
			quota.ResetAuthority = resetAuthority
		}
	}
	current, modelKey := findModelState(auth, model)
	if current != nil {
		newerQuota := quota.Exceeded && (quota.Generation > current.Quota.Generation ||
			(quota.Generation == current.Quota.Generation && quota.ObservedAt.After(current.Quota.ObservedAt)))
		if (normalizedStateGeneration(current.Generation) > stateGeneration ||
			(normalizedStateGeneration(current.Generation) ==
				stateGeneration &&
				current.UpdatedAt.After(updatedAt))) &&
			!newerQuota {
			return false
		}
	}
	state := ensureModelState(auth, model)
	candidate := &ModelState{
		Status: StatusError, StatusMessage: reason, Unavailable: true, NextRetryAfter: record.NextRetryAfter,
		Quota: quota, EffectScope: effectScope, EffectKey: effectKey, ResetAuthority: resetAuthority,
		Generation: stateGeneration, UpdatedAt: updatedAt, LastError: cloneError(record.LastError),
		QuotaEpoch: state.QuotaEpoch,
	}
	if !candidate.Quota.ObservedAt.After(state.Quota.ObservedAt) {
		candidate.Quota.ObservedAt = state.Quota.ObservedAt
		candidate.Quota.Signals = state.Quota.Clone().Signals
	}
	if quota.Exceeded {
		candidate.QuotaEpoch++
	}
	if reason == "" {
		candidate.StatusMessage = state.StatusMessage
	}
	if candidate.LastError != nil {
		candidate.LastError.executionState().StateGeneration = stateGeneration
	}
	auth.ModelStates[modelKey] = mergeModelStates(state, candidate, model == modelKey)
	updateAggregatedAvailability(auth, now)
	return true
}

func (m *Manager) cooldownDisabledForAuth(auth *Auth) bool {
	if m == nil {
		return quotaCooldownDisabledForAuth(auth)
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return quotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

// QuotaCooldownDisabledForAuthWithConfig 按凭据、Provider 配置和全局设置读取冷却策略
func QuotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func quotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
		if override, ok := providerCoolingDisabledForAuth(auth, cfg); ok {
			return override
		}
	}
	if cfg != nil && cfg.DisableCooling {
		return true
	}
	return quotaCooldownDisabled.Load()
}

func providerCoolingDisabledForAuth(auth *Auth, cfg *internalconfig.Config) (bool, bool) {
	if auth == nil || cfg == nil {
		return false, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return false, false
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if providerKey == "" && compatName == "" && provider != "openai-compatibility" {
		return false, false
	}
	if providerKey == "" {
		providerKey = provider
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, provider, auth)
	if entry == nil || entry.DisableCooling == nil {
		return false, false
	}
	return *entry.DisableCooling, true
}

func (m *Manager) clearDisabledCooldownStates(cfg *internalconfig.Config) bool {
	if m == nil {
		return false
	}
	now := time.Now()
	snapshots := make([]*Auth, 0)
	m.mu.Lock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if !quotaCooldownDisabledForAuthWithConfig(auth, cfg) && !auth.Disabled &&
			auth.Status != StatusDisabled {
			continue
		}
		if clearCooldownStateForAuth(auth, now) {
			snapshots = append(snapshots, auth.Clone())
		}
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		for _, snapshot := range snapshots {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	return len(snapshots) > 0
}

func clearCooldownStateForAuth(auth *Auth, now time.Time) bool {
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) {
		return false
	}
	changed := false
	if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded ||
		!auth.Quota.NextRecoverAt.IsZero() {
		changed = clearAuthState(auth, now, StateResetAuthorityOperator)
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded ||
			!state.Quota.NextRecoverAt.IsZero() {
			if state.Quota.Exceeded {
				state.QuotaEpoch++
			}
			changed = clearModelState(state, now, StateResetAuthorityOperator) || changed
		}
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	return changed
}

func (m *Manager) cooldownStateRecordsSnapshot() []CooldownStateRecord {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return m.cooldownStateRecordsSnapshotForConfig(cfg)
}

func (m *Manager) cooldownStateRecordsSnapshotForConfig(cfg *internalconfig.Config) []CooldownStateRecord {
	now := time.Now()
	records := make([]CooldownStateRecord, 0)
	m.mu.RLock()
	for _, auth := range m.auths {
		records = append(records, m.cooldownStateRecordsForAuthWithConfigLocked(auth, now, cfg)...)
	}
	m.mu.RUnlock()
	sort.Slice(
		records, func(i, j int) bool {
			if records[i].Provider != records[j].Provider {
				return records[i].Provider < records[j].Provider
			}
			if records[i].AuthID != records[j].AuthID {
				return records[i].AuthID < records[j].AuthID
			}
			return records[i].Model < records[j].Model
		},
	)
	return records
}

func (m *Manager) cooldownStateRecordsForAuthLocked(auth *Auth, now time.Time) []CooldownStateRecord {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return m.cooldownStateRecordsForAuthWithConfigLocked(auth, now, cfg)
}

func (m *Manager) cooldownStateRecordsForAuthWithConfigLocked(
	auth *Auth,
	now time.Time,
	cfg *internalconfig.Config,
) []CooldownStateRecord {
	if auth == nil || auth.ID == "" {
		return nil
	}
	terminal := hasTerminalUnauthorizedAuthFailure(auth)
	skipCooldown := auth.Disabled || auth.Status == StatusDisabled ||
		quotaCooldownDisabledForAuthWithConfig(auth, cfg)
	if skipCooldown && !terminal {
		return nil
	}
	records := make([]CooldownStateRecord, 0, 1+len(auth.ModelStates))
	if record, ok := authCooldownStateRecord(auth, now); ok {
		records = append(records, record)
	}
	if skipCooldown {
		return records
	}
	for model, state := range auth.ModelStates {
		if record, ok := modelCooldownStateRecord(auth, model, state, now); ok {
			records = append(records, record)
		}
	}
	sort.Slice(
		records, func(i, j int) bool {
			return records[i].Model < records[j].Model
		},
	)
	return records
}

func cooldownStateRecordsEqual(a, b []CooldownStateRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !cooldownStateRecordEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func cooldownStateRecordEqual(a, b CooldownStateRecord) bool {
	if a.Provider != b.Provider || a.AuthID != b.AuthID || a.AuthFile != b.AuthFile || a.Model != b.Model ||
		a.Status != b.Status || a.Reason != b.Reason || !a.NextRetryAfter.Equal(b.NextRetryAfter) ||
		a.EffectScope != b.EffectScope || a.EffectKey != b.EffectKey ||
		a.StateGeneration != b.StateGeneration || a.ResetAuthority != b.ResetAuthority ||
		a.BindingSHA256 != b.BindingSHA256 || !a.UpdatedAt.Equal(b.UpdatedAt) ||
		!cooldownQuotaEqual(a.Quota, b.Quota) {
		return false
	}
	return cooldownErrorEqual(a.LastError, b.LastError)
}

func cooldownQuotaEqual(a, b QuotaState) bool {
	return a.Exceeded == b.Exceeded && a.Reason == b.Reason && a.Family == b.Family &&
		a.BackoffLevel == b.BackoffLevel && a.Generation == b.Generation &&
		a.ResetAuthority == b.ResetAuthority && a.NextRecoverAt.Equal(b.NextRecoverAt)
}

func cooldownErrorEqual(a, b *Error) bool {
	if a == nil {
		return b == nil
	}
	if b == nil {
		return false
	}
	return a.Code == b.Code && a.Message == b.Message && a.Retryable == b.Retryable &&
		a.HTTPStatus ==
			b.HTTPStatus &&
		a.executionState().DispatchPhase ==
			b.executionState().DispatchPhase &&
		a.executionState().Outcome ==
			b.executionState().Outcome &&
		a.executionState().RetryDirective ==
			b.executionState().RetryDirective &&
		a.executionState().EffectScope ==
			b.executionState().EffectScope &&
		a.executionState().EffectKey ==
			b.executionState().EffectKey &&
		a.executionState().StateGeneration ==
			b.executionState().StateGeneration
}

func authCooldownStateRecord(auth *Auth, now time.Time) (CooldownStateRecord, bool) {
	if auth == nil || !persistableUnavailableState(auth.Unavailable, auth.NextRetryAfter, now) {
		return CooldownStateRecord{}, false
	}
	return CooldownStateRecord{
		Provider:        strings.TrimSpace(auth.Provider),
		AuthID:          auth.ID,
		AuthFile:        cooldownAuthFile(auth),
		Status:          persistedCooldownStatus(auth.NextRetryAfter),
		NextRetryAfter:  auth.NextRetryAfter,
		Reason:          cooldownReason(auth.StatusMessage, auth.Quota, auth.LastError),
		Quota:           quotaForCooldownRecord(auth.Provider, auth.Quota),
		LastError:       cloneError(auth.LastError),
		EffectScope:     auth.StateEffectScope,
		EffectKey:       auth.StateEffectKey,
		StateGeneration: normalizedStateGeneration(auth.StateGeneration),
		ResetAuthority:  auth.ResetAuthority,
		BindingSHA256:   credentialBindingSHA256(auth),
		UpdatedAt:       auth.UpdatedAt,
	}, true
}

func modelCooldownStateRecord(
	auth *Auth,
	model string,
	state *ModelState,
	now time.Time,
) (CooldownStateRecord, bool) {
	model = strings.TrimSpace(model)
	if auth == nil || state == nil || model == "" ||
		!persistableUnavailableState(state.Unavailable, state.NextRetryAfter, now) {
		return CooldownStateRecord{}, false
	}
	stateGeneration := normalizedStateGeneration(state.Generation)
	if state.EffectScope == cliproxyexecutor.StateEffectScopeQuotaFamily {
		stateGeneration = normalizedStateGeneration(state.Quota.Generation)
	}
	return CooldownStateRecord{
		Provider:        strings.TrimSpace(auth.Provider),
		AuthID:          auth.ID,
		AuthFile:        cooldownAuthFile(auth),
		Model:           model,
		Status:          persistedCooldownStatus(state.NextRetryAfter),
		NextRetryAfter:  state.NextRetryAfter,
		Reason:          cooldownReason(state.StatusMessage, state.Quota, state.LastError),
		Quota:           quotaForCooldownRecord(auth.Provider, state.Quota),
		LastError:       cloneError(state.LastError),
		EffectScope:     state.EffectScope,
		EffectKey:       state.EffectKey,
		StateGeneration: stateGeneration,
		ResetAuthority:  state.ResetAuthority,
		BindingSHA256:   credentialBindingSHA256(auth),
		UpdatedAt:       state.UpdatedAt,
	}, true
}

func persistableUnavailableState(unavailable bool, nextRetryAfter, now time.Time) bool {
	return unavailable && (nextRetryAfter.IsZero() || nextRetryAfter.After(now))
}

func persistedCooldownStatus(nextRetryAfter time.Time) string {
	if nextRetryAfter.IsZero() {
		return "blocked"
	}
	return "cooling"
}

func cooldownReason(statusMessage string, quota QuotaState, lastErr *Error) string {
	if reason := strings.TrimSpace(quota.Reason); reason != "" {
		return reason
	}
	if statusMessage = strings.TrimSpace(statusMessage); statusMessage != "" {
		return statusMessage
	}
	if lastErr != nil {
		if code := strings.TrimSpace(lastErr.Code); code != "" {
			return code
		}
		if message := strings.TrimSpace(lastErr.Message); message != "" {
			return message
		}
	}
	return ""
}
