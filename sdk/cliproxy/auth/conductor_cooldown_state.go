package auth

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const cooldownStatePersistenceTimeout = 5 * time.Second

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

	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.RLock()
	oldStore := m.cooldownStore
	m.mu.RUnlock()
	if oldStore != nil {
		records := m.cooldownStateRecordsSnapshotForConfig(cfg)
		if store == nil {
			records = nil
		}
		if errSave := oldStore.Save(ctx, records); errSave != nil {
			logEntryWithRequestID(ctx).Warnf("failed to persist cooldown state before config transition: %v", errSave)
			return false
		}
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return false
	}
	m.setConfigSnapshotLocked(cfg)
	m.mu.Lock()
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
	return ctx.Err() == nil
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
	snapshotsByID := make(map[string]*Auth)
	m.mu.Lock()
	for _, record := range records {
		if strings.TrimSpace(record.Model) == "" {
			authLevelRecords = append(authLevelRecords, record)
			continue
		}
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				snapshotsByID[auth.ID] = auth.Clone()
			}
		}
	}
	for _, record := range authLevelRecords {
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				snapshotsByID[auth.ID] = auth.Clone()
			}
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, snapshot := range snapshotsByID {
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
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || m.cooldownDisabledForAuth(auth) {
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
	quota := record.Quota
	if quota.Exceeded && quota.NextRecoverAt.IsZero() && !record.NextRetryAfter.IsZero() {
		quota.NextRecoverAt = record.NextRetryAfter
	}
	resetAuthority := record.ResetAuthority
	if resetAuthority == "" {
		resetAuthority = StateResetAuthoritySuccess
	}
	stateGeneration := normalizedStateGeneration(record.StateGeneration)

	if model == "" {
		effectScope := record.EffectScope
		if effectScope == "" {
			effectScope = cliproxyexecutor.StateEffectScopeCredential
		}
		effectKey := strings.TrimSpace(record.EffectKey)
		if effectKey == "" {
			effectKey = auth.ID
		}
		auth.Unavailable = true
		auth.Status = StatusError
		auth.NextRetryAfter = record.NextRetryAfter
		auth.Quota = quota
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
			auth.LastError.StateGeneration = auth.StateGeneration
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
	if quota.Exceeded {
		stateGeneration = max(stateGeneration, normalizedStateGeneration(quota.Generation))
		quota.Generation = stateGeneration
		if quota.ResetAuthority == "" {
			quota.ResetAuthority = resetAuthority
		}
	}
	state := ensureModelState(auth, model)
	if quota.Exceeded {
		state.QuotaEpoch++
	}
	state.Unavailable = true
	state.Status = StatusError
	state.NextRetryAfter = record.NextRetryAfter
	state.Quota = quota
	state.EffectScope = effectScope
	state.EffectKey = effectKey
	state.ResetAuthority = resetAuthority
	state.Generation = max(normalizedStateGeneration(state.Generation), stateGeneration)
	state.UpdatedAt = updatedAt
	if reason != "" {
		state.StatusMessage = reason
	}
	state.LastError = cloneError(record.LastError)
	if state.LastError != nil {
		state.LastError.StateGeneration = stateGeneration
	}
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

func quotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
		if providerCoolingDisabledForAuth(auth, cfg) {
			return true
		}
	}
	if cfg != nil && cfg.DisableCooling {
		return true
	}
	return quotaCooldownDisabled.Load()
}

func providerCoolingDisabledForAuth(auth *Auth, cfg *internalconfig.Config) bool {
	if auth == nil || cfg == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return false
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if providerKey == "" && compatName == "" && provider != "openai-compatibility" {
		return false
	}
	if providerKey == "" {
		providerKey = provider
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, provider)
	return entry != nil && entry.DisableCooling
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
		if !quotaCooldownDisabledForAuthWithConfig(auth, cfg) && !auth.Disabled && auth.Status != StatusDisabled {
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
	if auth == nil {
		return false
	}
	changed := false
	if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero() {
		changed = clearAuthState(auth, now, StateResetAuthorityOperator)
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
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
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == StatusDisabled ||
		quotaCooldownDisabledForAuthWithConfig(auth, cfg) {
		return nil
	}
	records := make([]CooldownStateRecord, 0, 1+len(auth.ModelStates))
	if record, ok := authCooldownStateRecord(auth, now); ok {
		records = append(records, record)
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
		a.HTTPStatus == b.HTTPStatus && a.DispatchPhase == b.DispatchPhase && a.Outcome == b.Outcome &&
		a.RetryDirective == b.RetryDirective && a.EffectScope == b.EffectScope &&
		a.EffectKey == b.EffectKey && a.StateGeneration == b.StateGeneration
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
		Quota:           auth.Quota,
		LastError:       cloneError(auth.LastError),
		EffectScope:     auth.StateEffectScope,
		EffectKey:       auth.StateEffectKey,
		StateGeneration: normalizedStateGeneration(auth.StateGeneration),
		ResetAuthority:  auth.ResetAuthority,
		BindingSHA256:   credentialBindingSHA256(auth),
		UpdatedAt:       auth.UpdatedAt,
	}, true
}

func modelCooldownStateRecord(auth *Auth, model string, state *ModelState, now time.Time) (CooldownStateRecord, bool) {
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
		Quota:           state.Quota,
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
