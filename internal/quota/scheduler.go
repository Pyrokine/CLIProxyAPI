package quota

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// FetchFunc is the compatibility form of a quota fetcher without cancellation.
type FetchFunc func(entry *Entry) ([]byte, error)

// ContextFetchFunc refreshes quota data for one immutable credential snapshot.
type ContextFetchFunc func(ctx context.Context, entry *Entry) ([]byte, error)

// ResultFetchFunc refreshes quota data and reports source, completeness, and empty-result authority.
type ResultFetchFunc func(ctx context.Context, entry *Entry) (FetchResult, error)

// RefreshAttempt describes one successful provider fetch before its state is published.
type RefreshAttempt struct {
	EntryKey       string
	FileName       string
	Type           CredentialType
	AuthIndex      string
	PreviousStatus Status
	StartedAt      time.Time
	Data           []byte
}

// refreshFinalizer is the compatibility form of a refresh finalizer without cancellation.
type refreshFinalizer func(attempt RefreshAttempt) error

// refreshContextFinalizer applies provider-specific recovery before refresh success is published.
type refreshContextFinalizer func(ctx context.Context, attempt RefreshAttempt) error

type persistFunc func(entry EntryState)

type deleteFunc func(entry EntryState)

var errRefreshTargetChanged = errors.New("quota refresh target changed")

type refreshFlight struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}
	waiters int
	result  RefreshResult
}

// Scheduler manages periodic quota refresh for all registered credentials.
// Periodic work uses a min-heap dispatcher; manual work may refresh unrelated
// targets concurrently while overlapping target calls share one fetch.
type Scheduler struct {
	mu               sync.RWMutex
	entries          map[string]*Entry
	history          map[string]EntryState
	cfg              Config
	fetchFn          ResultFetchFunc
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	wake             chan struct{} // signal dispatcher to re-check heap
	updatedAt        time.Time
	refreshFinalizer refreshContextFinalizer
	persistFn        persistFunc
	deleteFn         deleteFunc
	flightMu         sync.Mutex
	flights          map[string]*refreshFlight
}

// NewScheduler creates a quota scheduler for a compatibility fetcher.
func NewScheduler(cfg Config, fetchFn FetchFunc) *Scheduler {
	var contextFetchFn ContextFetchFunc
	if fetchFn != nil {
		contextFetchFn = func(_ context.Context, entry *Entry) ([]byte, error) {
			return fetchFn(entry)
		}
	}
	return NewSchedulerWithContext(cfg, contextFetchFn)
}

// NewSchedulerWithContext creates a quota scheduler whose fetches support cancellation.
func NewSchedulerWithContext(cfg Config, fetchFn ContextFetchFunc) *Scheduler {
	var resultFetchFn ResultFetchFunc
	if fetchFn != nil {
		resultFetchFn = func(ctx context.Context, entry *Entry) (FetchResult, error) {
			data, err := fetchFn(ctx, entry)
			return FetchResult{
				Data:               data,
				Completeness:       FetchComplete,
				AuthoritativeEmpty: err == nil && len(data) == 0,
			}, err
		}
	}
	return NewSchedulerWithResult(cfg, resultFetchFn)
}

// NewSchedulerWithResult creates a quota scheduler with explicit freshness semantics.
func NewSchedulerWithResult(cfg Config, fetchFn ResultFetchFunc) *Scheduler {
	return &Scheduler{
		entries: make(map[string]*Entry),
		history: make(map[string]EntryState),
		cfg:     cfg,
		fetchFn: fetchFn,
		wake:    make(chan struct{}, 1),
		flights: make(map[string]*refreshFlight),
	}
}

func (s *Scheduler) SetPersistence(persist persistFunc, deleteFn deleteFunc) {
	s.mu.Lock()
	s.persistFn = persist
	s.deleteFn = deleteFn
	s.mu.Unlock()
}

// setRefreshFinalizer configures a compatibility finalizer for every refresh path.
func (s *Scheduler) setRefreshFinalizer(finalizer refreshFinalizer) {
	if finalizer == nil {
		s.SetRefreshContextFinalizer(nil)
		return
	}
	s.SetRefreshContextFinalizer(
		func(_ context.Context, attempt RefreshAttempt) error {
			return finalizer(attempt)
		},
	)
}

// SetRefreshContextFinalizer configures the context-aware finalizer for every refresh path.
func (s *Scheduler) SetRefreshContextFinalizer(finalizer refreshContextFinalizer) {
	s.mu.Lock()
	s.refreshFinalizer = finalizer
	s.mu.Unlock()
}

func (s *Scheduler) Restore(entries map[string]EntryState) {
	if len(entries) == 0 {
		return
	}
	changed := make([]EntryState, 0, len(entries))
	now := time.Now().UTC()
	s.mu.Lock()
	for name, rawState := range entries {
		state := normalizeRestoredEntryState(rawState)
		key := strings.TrimSpace(name)
		if key == "" {
			key = BindingEntryKey(state.Type, state.FileName, state.AuthIndex)
		}
		if key == "" {
			continue
		}
		if current, exists := s.history[key]; exists && entryGenerationPrecedes(state, current) {
			continue
		}
		if state.Status == statusLoading {
			state.Status = StatusIdle
			state.Error = "quota refresh interrupted by server restart"
			state.LastError = state.Error
			state.LastErrorAt = cloneTime(&now)
			state.StateRevision++
			if len(state.Data) > 0 || state.SuccessfulEmpty {
				state.Freshness = freshnessStale
			} else {
				state.Freshness = FreshnessOutdated
			}
		}
		s.history[key] = cloneEntryState(state)
	}
	for key, state := range s.history {
		entry := s.entries[key]
		if entry == nil {
			continue
		}
		entry.mu.Lock()
		disabledConfigured := entry.disabledConfigured
		disabled := entry.Disabled
		stateRevision := entry.StateRevision
		switch {
		case state.Tombstone:
			generation := max(entry.Generation, state.Generation+1)
			resetEntryForBindingLocked(entry, generation)
		case !entryBindingMatchesStateLocked(entry, state):
			generation := max(entry.Generation, state.Generation+1)
			resetEntryForBindingLocked(entry, generation)
		default:
			restoreEntryStateLocked(entry, state)
		}
		if entry.Generation == state.Generation && entry.StateRevision < stateRevision {
			entry.StateRevision = stateRevision
		}
		entry.disabledConfigured = disabledConfigured
		if disabledConfigured {
			entry.Disabled = disabled
		}
		current := entry.snapshotLocked()
		s.history[key] = cloneEntryState(current)
		changed = append(changed, current)
		entry.mu.Unlock()
	}
	if len(changed) > 0 {
		s.updatedAt = now
	}
	s.mu.Unlock()
	for _, state := range changed {
		s.persist(state)
	}
}

// Start begins the scheduler's dispatch loop. No-op if not enabled.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Enabled {
		return
	}
	if s.cancel != nil {
		return // already running
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.dispatchLoop(ctx)
}

// Stop gracefully shuts down the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		s.wg.Wait()
	}
}

// Register adds or updates a credential in the scheduler.
func (s *Scheduler) Register(fileName string, credType CredentialType, authIndex string) {
	s.RegisterBinding(fileName, credType, authIndex, authIndex)
}

// RegisterBinding adds or updates a credential with a non-secret persisted revision identity.
func (s *Scheduler) RegisterBinding(
	fileName string,
	credType CredentialType,
	authIndex,
	credentialRevision string,
	equivalentRevisions ...string,
) {
	fileName = strings.TrimSpace(fileName)
	authIndex = strings.TrimSpace(authIndex)
	credentialRevision = strings.TrimSpace(credentialRevision)
	revisionMatches := func(revision string) bool {
		revision = strings.TrimSpace(revision)
		if revision == credentialRevision {
			return true
		}
		for _, equivalent := range equivalentRevisions {
			if revision != "" && revision == equivalent {
				return true
			}
		}
		return false
	}
	key := BindingEntryKey(credType, fileName, authIndex)
	if key == "" {
		return
	}

	var state EntryState
	tombstones := make([]EntryState, 0)
	now := time.Now().UTC()
	s.mu.Lock()
	for existingKey, existing := range s.entries {
		if existingKey == key {
			continue
		}
		existing.mu.RLock()
		replaceSameAuth := existing.FileName == fileName && existing.AuthIndex == authIndex && authIndex != ""
		existing.mu.RUnlock()
		if !replaceSameAuth {
			continue
		}
		existing.mu.Lock()
		tombstone := tombstoneEntryLocked(existing)
		existing.mu.Unlock()
		delete(s.entries, existingKey)
		s.history[existingKey] = cloneEntryState(tombstone)
		tombstones = append(tombstones, tombstone)
	}

	if existing := s.entries[key]; existing != nil {
		existing.mu.Lock()
		if existing.AuthIndex != authIndex || !revisionMatches(existing.credentialRevision) {
			generation := existing.Generation + 1
			if previous := s.history[key]; previous.Generation >= generation {
				generation = previous.Generation + 1
			}
			existing.AuthIndex = authIndex
			existing.credentialRevision = credentialRevision
			resetEntryForBindingLocked(existing, generation)
		} else if existing.credentialRevision != credentialRevision {
			existing.credentialRevision = credentialRevision
			existing.StateRevision++
		}
		state = existing.snapshotLocked()
		s.history[key] = cloneEntryState(state)
		existing.mu.Unlock()
		s.updatedAt = now
		s.poke()
		s.mu.Unlock()
		for _, tombstone := range tombstones {
			s.deletePersisted(tombstone)
		}
		s.persist(state)
		return
	}

	entry := newEntryForBinding(fileName, credType, authIndex, credentialRevision, now)
	if previous, exists := s.history[key]; exists {
		matches := bindingMatches(previous, authIndex, credentialRevision)
		if len(equivalentRevisions) > 0 {
			matches = previous.AuthIndex == authIndex && revisionMatches(previous.CredentialRevision)
		}
		if !previous.Tombstone && matches {
			if previous.CredentialRevision != credentialRevision {
				previous.CredentialRevision = credentialRevision
				previous.StateRevision++
			}
			restoreEntryStateLocked(entry, previous)
		} else {
			resetEntryForBindingLocked(entry, previous.Generation+1)
		}
	}
	next := now.Add(time.Duration(len(s.entries)) * time.Second * 2) // stagger
	if entry.NextRefresh == nil {
		entry.NextRefresh = &next
	}
	s.entries[key] = entry
	state = entry.snapshot()
	s.history[key] = cloneEntryState(state)
	s.updatedAt = now
	s.poke()
	s.mu.Unlock()
	for _, tombstone := range tombstones {
		s.deletePersisted(tombstone)
	}
	s.persist(state)
}

// Unregister removes credentials for a file from the scheduler and persists tombstones.
func (s *Scheduler) Unregister(fileName string) {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return
	}
	tombstones := make([]EntryState, 0)
	s.mu.Lock()
	for key, entry := range s.entries {
		entry.mu.RLock()
		matches := entry.FileName == fileName
		entry.mu.RUnlock()
		if !matches {
			continue
		}
		entry.mu.Lock()
		tombstone := tombstoneEntryLocked(entry)
		entry.mu.Unlock()
		delete(s.entries, key)
		s.history[key] = cloneEntryState(tombstone)
		tombstones = append(tombstones, tombstone)
	}
	if len(tombstones) > 0 {
		s.updatedAt = time.Now().UTC()
		s.poke()
	}
	s.mu.Unlock()
	for _, tombstone := range tombstones {
		s.deletePersisted(tombstone)
	}
}

// UnregisterBinding removes one exact credential identity while preserving siblings from the same file.
func (s *Scheduler) UnregisterBinding(fileName string, credType CredentialType, authIndex string) {
	key := BindingEntryKey(credType, fileName, authIndex)
	if key == "" {
		return
	}
	var tombstone EntryState
	s.mu.Lock()
	if entry := s.entries[key]; entry != nil {
		entry.mu.Lock()
		tombstone = tombstoneEntryLocked(entry)
		entry.mu.Unlock()
		delete(s.entries, key)
		s.history[key] = cloneEntryState(tombstone)
		s.updatedAt = time.Now().UTC()
		s.poke()
	}
	s.mu.Unlock()
	if tombstone.FileName != "" {
		s.deletePersisted(tombstone)
	}
}

// updateAuthIndex sets the auth_index for a registered entry.
func (s *Scheduler) updateAuthIndex(fileName string, credType CredentialType, authIndex string) {
	key := EntryKey(credType, fileName)
	if key == "" {
		return
	}
	var state EntryState
	s.mu.Lock()
	if entry := s.entries[key]; entry != nil {
		entry.mu.Lock()
		if entry.AuthIndex != authIndex {
			entry.AuthIndex = strings.TrimSpace(authIndex)
			entry.credentialRevision = entry.AuthIndex
			resetEntryForBindingLocked(entry, entry.Generation+1)
		}
		state = entry.snapshotLocked()
		s.history[key] = cloneEntryState(state)
		entry.mu.Unlock()
		s.updatedAt = time.Now().UTC()
	}
	s.mu.Unlock()
	if state.FileName != "" {
		s.persist(state)
	}
}

// SetDisabled updates the disabled state for a legacy single-identity binding.
func (s *Scheduler) SetDisabled(fileName string, credType CredentialType, disabled bool) {
	s.SetBindingDisabled(fileName, credType, "", disabled)
}

// SetBindingDisabled updates one exact credential binding and wakes the dispatcher.
func (s *Scheduler) SetBindingDisabled(
	fileName string,
	credType CredentialType,
	authIndex string,
	disabled bool,
) {
	key := BindingEntryKey(credType, fileName, authIndex)
	if key == "" {
		return
	}
	var state EntryState
	s.mu.Lock()
	if entry, ok := s.entries[key]; ok {
		entry.mu.Lock()
		if entry.Disabled != disabled || !entry.disabledConfigured {
			entry.Disabled = disabled
			entry.disabledConfigured = true
			entry.StateRevision++
		}
		state = entry.snapshotLocked()
		s.history[key] = cloneEntryState(state)
		entry.mu.Unlock()
		s.updatedAt = time.Now().UTC()
		s.poke()
	}
	s.mu.Unlock()
	if state.FileName != "" {
		s.persist(state)
	}
}

// RefreshNow triggers an immediate refresh for specific credentials or all eligible credentials.
func (s *Scheduler) RefreshNow(fileNames []string) {
	s.refreshNow(context.Background(), fileNames, nil)
}

// refreshNowWithFinalizer refreshes named credentials with a compatibility finalizer.
func (s *Scheduler) refreshNowWithFinalizer(fileNames []string, finalizer refreshFinalizer) {
	var contextFinalizer refreshContextFinalizer
	if finalizer != nil {
		contextFinalizer = func(_ context.Context, attempt RefreshAttempt) error {
			return finalizer(attempt)
		}
	}
	s.refreshNow(context.Background(), fileNames, contextFinalizer)
}

func (s *Scheduler) refreshNow(
	ctx context.Context,
	fileNames []string,
	finalizer refreshContextFinalizer,
) {
	targets, _ := s.ResolveRefreshTargets(fileNames)
	isManual := len(fileNames) > 0
	for _, target := range targets {
		if !isManual && shouldSkipAutoRefreshTarget(target) {
			continue
		}
		if finalizer == nil {
			_ = s.RefreshTarget(ctx, target)
			continue
		}
		_ = s.refreshTarget(ctx, target, finalizer, false)
	}
}

// ResolveRefreshTargets resolves provider-aware keys and legacy file names to stable targets.
func (s *Scheduler) ResolveRefreshTargets(identifiers []string) ([]RefreshTarget, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	targets := make([]RefreshTarget, 0, len(s.entries))
	seen := make(map[string]struct{}, len(s.entries))
	unresolved := make([]string, 0)
	if len(identifiers) == 0 {
		for key, entry := range s.entries {
			targets = append(targets, refreshTargetSnapshot(key, entry))
		}
	} else {
		for _, rawIdentifier := range identifiers {
			identifier := strings.TrimSpace(rawIdentifier)
			if identifier == "" {
				continue
			}
			matched := false
			if entry, ok := s.entries[identifier]; ok {
				matched = true
				if _, exists := seen[identifier]; !exists {
					seen[identifier] = struct{}{}
					targets = append(targets, refreshTargetSnapshot(identifier, entry))
				}
			} else {
				for key, entry := range s.entries {
					entry.mu.RLock()
					matches := entry.FileName == identifier
					entry.mu.RUnlock()
					if !matches {
						continue
					}
					matched = true
					if _, exists := seen[key]; exists {
						continue
					}
					seen[key] = struct{}{}
					targets = append(targets, refreshTargetSnapshot(key, entry))
				}
			}
			if !matched {
				unresolved = append(unresolved, identifier)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].EntryKey < targets[j].EntryKey })
	return targets, unresolved
}

func refreshTargetSnapshot(key string, entry *Entry) RefreshTarget {
	if entry == nil {
		return RefreshTarget{EntryKey: key}
	}
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if key == "" {
		key = BindingEntryKey(entry.Type, entry.FileName, entry.AuthIndex)
	}
	return RefreshTarget{
		EntryKey:   key,
		FileName:   entry.FileName,
		Type:       entry.Type,
		AuthIndex:  entry.AuthIndex,
		Generation: entry.Generation,
		Status:     entry.Status,
		Disabled:   entry.Disabled,
	}
}

func shouldSkipAutoRefreshTarget(target RefreshTarget) bool {
	return target.Disabled || target.Status == StatusBanned || target.Status == StatusQuotaExceeded
}

// ProbeTarget calls the configured provider quota endpoint without publishing
// quota state or invoking the refresh finalizer.
func (s *Scheduler) ProbeTarget(ctx context.Context, target RefreshTarget) ProbeResult {
	startedAt := time.Now().UTC()
	result := ProbeResult{Target: target, StartedAt: startedAt}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = err
		}
		result.Outcome = ProbeOutcomeInconclusive
		result.Error = cause.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}

	s.mu.RLock()
	entry := s.entries[target.EntryKey]
	if entry == nil {
		s.mu.RUnlock()
		result.Outcome = ProbeOutcomeOutdated
		result.Error = errRefreshTargetChanged.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	entry.mu.RLock()
	if !refreshTargetMatchesEntryLocked(target, entry) {
		entry.mu.RUnlock()
		s.mu.RUnlock()
		result.Outcome = ProbeOutcomeOutdated
		result.Error = errRefreshTargetChanged.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	fetchEntry := &Entry{
		Generation: target.Generation,
		FileName:   target.FileName,
		Type:       target.Type,
		AuthIndex:  target.AuthIndex,
		Status:     entry.Status,
		Disabled:   entry.Disabled,
	}
	entry.mu.RUnlock()
	s.mu.RUnlock()

	var fetchResult FetchResult
	var err error
	if s.fetchFn == nil {
		err = errors.New("quota fetcher is not configured")
	} else {
		fetchResult, err = s.fetchFn(ctx, fetchEntry)
	}

	s.mu.RLock()
	current := s.entries[target.EntryKey]
	bindingCurrent := current == entry
	if bindingCurrent {
		entry.mu.RLock()
		bindingCurrent = refreshTargetMatchesEntryLocked(target, entry)
		entry.mu.RUnlock()
	}
	s.mu.RUnlock()
	result.CompletedAt = time.Now().UTC()
	if !bindingCurrent {
		result.Outcome = ProbeOutcomeOutdated
		result.Error = errRefreshTargetChanged.Error()
		return result
	}
	if errContext := ctx.Err(); errContext != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = errContext
		}
		result.Outcome = ProbeOutcomeInconclusive
		result.Error = cause.Error()
		return result
	}
	if err != nil {
		result.Outcome = ProbeOutcomeFailed
		result.Error = err.Error()
		return result
	}
	result.Outcome = ProbeOutcomePassed
	result.Data = append(result.Data, fetchResult.Data...)
	return result
}

// RefreshTarget refreshes one exact provider credential with per-target singleflight.
func (s *Scheduler) RefreshTarget(ctx context.Context, target RefreshTarget) RefreshResult {
	return s.refreshTarget(ctx, target, nil, true)
}

func (s *Scheduler) refreshTarget(
	ctx context.Context,
	target RefreshTarget,
	finalizer refreshContextFinalizer,
	share bool,
) RefreshResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if finalizer == nil {
		finalizer = s.selectRefreshFinalizer()
	}
	if !share {
		return s.executeRefresh(ctx, target, finalizer)
	}

	flightKey := target.EntryKey + "\x00" + strconv.FormatUint(target.Generation, 10)
	s.flightMu.Lock()
	flight, exists := s.flights[flightKey]
	if exists {
		flight.waiters++
	} else {
		flightCtx, cancel := context.WithCancelCause(context.Background())
		flight = &refreshFlight{
			ctx:     flightCtx,
			cancel:  cancel,
			done:    make(chan struct{}),
			waiters: 1,
		}
		s.flights[flightKey] = flight
		go s.runRefreshFlight(flightKey, flight, target, finalizer)
	}
	s.flightMu.Unlock()

	select {
	case <-flight.done:
		result := flight.result
		result.Shared = exists
		return result
	case <-ctx.Done():
		if s.leaveRefreshFlight(flightKey, flight) {
			<-flight.done
			result := flight.result
			result.Shared = exists
			return result
		}
		return RefreshResult{
			Target:      target,
			Outcome:     RefreshOutcomeCancelled,
			Error:       context.Cause(ctx).Error(),
			StartedAt:   time.Now().UTC(),
			CompletedAt: time.Now().UTC(),
			Shared:      exists,
		}
	}
}

func (s *Scheduler) leaveRefreshFlight(flightKey string, flight *refreshFlight) bool {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	current, ok := s.flights[flightKey]
	if !ok || current != flight {
		return false
	}
	if flight.waiters > 0 {
		flight.waiters--
	}
	if flight.waiters != 0 {
		return false
	}
	flight.cancel(context.Canceled)
	return true
}

func (s *Scheduler) runRefreshFlight(
	flightKey string,
	flight *refreshFlight,
	target RefreshTarget,
	finalizer refreshContextFinalizer,
) {
	result := s.executeRefresh(flight.ctx, target, finalizer)
	s.flightMu.Lock()
	flight.result = result
	if current, ok := s.flights[flightKey]; ok && current == flight {
		delete(s.flights, flightKey)
	}
	close(flight.done)
	s.flightMu.Unlock()
}

func (s *Scheduler) selectRefreshFinalizer() refreshContextFinalizer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.refreshFinalizer
}

// GetStatus returns the current status of all credentials.
func (s *Scheduler) GetStatus() StatusResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	credentials := make(map[string]*Entry, len(s.entries))
	for name, e := range s.entries {
		e.mu.RLock()
		cp := Entry{
			FileName:        e.FileName,
			Type:            e.Type,
			AuthIndex:       e.AuthIndex,
			Generation:      e.Generation,
			StateRevision:   e.StateRevision,
			Status:          e.Status,
			Freshness:       e.Freshness,
			Source:          e.Source,
			ObservedAt:      cloneTime(e.ObservedAt),
			FetchedAt:       cloneTime(e.FetchedAt),
			LastSuccessAt:   cloneTime(e.LastSuccessAt),
			LastError:       e.LastError,
			LastErrorAt:     cloneTime(e.LastErrorAt),
			RetryAt:         cloneTime(e.RetryAt),
			SuccessfulEmpty: e.SuccessfulEmpty,
			LastRefresh:     cloneTime(e.LastRefresh),
			NextRefresh:     cloneTime(e.NextRefresh),
			Error:           e.Error,
			FailureCount:    e.FailureCount,
			Data:            append(json.RawMessage(nil), e.Data...),
			Disabled:        e.Disabled,
		}
		e.mu.RUnlock()
		credentials[name] = &cp
	}
	return StatusResponse{
		Enabled:         s.cfg.Enabled,
		IntervalSeconds: s.cfg.Interval,
		Credentials:     credentials,
		ByProvider:      summarizeEntriesByProvider(credentials),
		UpdatedAt:       s.updatedAt,
	}
}

func summarizeEntriesByProvider(entries map[string]*Entry) map[string]ProviderStatusSummary {
	out := make(map[string]ProviderStatusSummary)
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(string(entry.Type)))
		if provider == "" {
			provider = "unknown"
		}
		summary := out[provider]
		summary.Provider = provider
		summary.CredentialCount++
		summary.FailureCount += entry.FailureCount
		if entry.Disabled {
			summary.Disabled++
		}
		switch entry.Freshness {
		case FreshnessFresh:
			summary.Fresh++
		case freshnessStale:
			summary.Stale++
		case freshnessPartial:
			summary.Partial++
		case FreshnessOutdated:
			summary.Outdated++
		}
		if entry.SuccessfulEmpty {
			summary.SuccessfulEmpty++
		}
		switch entry.Status {
		case StatusIdle:
			summary.Idle++
		case statusLoading:
			summary.Loading++
		case StatusSuccess:
			summary.Success++
		case StatusError:
			summary.Error++
		case StatusBanned:
			summary.Banned++
		case StatusQuotaExceeded:
			summary.QuotaExceeded++
		}
		if entry.LastRefresh != nil && (summary.LatestRefreshAt == nil || entry.LastRefresh.After(*summary.LatestRefreshAt)) {
			latest := *entry.LastRefresh
			summary.LatestRefreshAt = &latest
		}
		if entry.NextRefresh != nil && (summary.NextRefreshAt == nil || entry.NextRefresh.Before(*summary.NextRefreshAt)) {
			next := *entry.NextRefresh
			summary.NextRefreshAt = &next
		}
		out[provider] = summary
	}
	return out
}

// GetConfig returns the current configuration.
func (s *Scheduler) GetConfig() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// UpdateConfig updates the scheduler configuration.
func (s *Scheduler) UpdateConfig(cfg Config) {
	s.mu.Lock()
	wasEnabled := s.cfg.Enabled
	s.cfg = cfg

	if cfg.Enabled && !wasEnabled {
		s.mu.Unlock()
		s.Start()
	} else if !cfg.Enabled && wasEnabled {
		s.mu.Unlock()
		s.Stop()
	} else {
		s.mu.Unlock()
	}
}

func (s *Scheduler) persist(state EntryState) {
	s.mu.RLock()
	persistFn := s.persistFn
	s.mu.RUnlock()
	if persistFn != nil {
		persistFn(state)
	}
}

func (s *Scheduler) deletePersisted(state EntryState) {
	s.mu.RLock()
	deleteFn := s.deleteFn
	s.mu.RUnlock()
	if deleteFn != nil {
		deleteFn(state)
		return
	}
	s.persist(state)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// poke signals the dispatcher to re-check the heap. Must be called with s.mu held.
func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// dispatchLoop is the main scheduler goroutine.
func (s *Scheduler) dispatchLoop(ctx context.Context) {
	defer s.wg.Done()

	for {
		s.mu.RLock()
		h := s.buildHeap()
		s.mu.RUnlock()

		if h.Len() == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}

		next := h[0]
		next.mu.RLock()
		nextRefresh := cloneTime(next.NextRefresh)
		next.mu.RUnlock()
		if nextRefresh == nil {
			continue
		}
		target := refreshTargetSnapshot("", next)
		delay := time.Until(*nextRefresh)
		if delay <= 0 {
			_ = s.RefreshTarget(ctx, target)
			continue
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
			_ = s.RefreshTarget(ctx, target)
		}
	}
}

const refreshFinalizerErrorMessage = "quota refreshed, but routing state could not be restored"

func (s *Scheduler) executeRefresh(
	ctx context.Context,
	target RefreshTarget,
	finalizer refreshContextFinalizer,
) RefreshResult {
	startedAt := time.Now().UTC()
	result := RefreshResult{Target: target, StartedAt: startedAt}
	if err := ctx.Err(); err != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = err
		}
		result.Outcome = RefreshOutcomeCancelled
		result.Error = cause.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}

	s.mu.Lock()
	entry := s.entries[target.EntryKey]
	if entry == nil {
		s.mu.Unlock()
		result.Outcome = RefreshOutcomeInconclusive
		result.Error = errRefreshTargetChanged.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	entry.mu.Lock()
	if !refreshTargetMatchesEntryLocked(target, entry) {
		entry.mu.Unlock()
		s.mu.Unlock()
		result.Outcome = RefreshOutcomeInconclusive
		result.Error = errRefreshTargetChanged.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	previousState := entry.snapshotLocked()
	attempt := RefreshAttempt{
		EntryKey:       target.EntryKey,
		FileName:       target.FileName,
		Type:           target.Type,
		AuthIndex:      target.AuthIndex,
		PreviousStatus: entry.Status,
		StartedAt:      startedAt,
	}
	fetchEntry := &Entry{
		FileName:      target.FileName,
		Type:          target.Type,
		AuthIndex:     target.AuthIndex,
		Generation:    target.Generation,
		StateRevision: entry.StateRevision,
		Status:        entry.Status,
		Freshness:     entry.Freshness,
		Disabled:      entry.Disabled,
	}
	entry.Status = statusLoading
	entry.StateRevision++
	loadingState := entry.snapshotLocked()
	s.history[target.EntryKey] = cloneEntryState(loadingState)
	entry.mu.Unlock()
	s.mu.Unlock()
	s.persist(loadingState)

	var fetchResult FetchResult
	var err error
	if s.fetchFn == nil {
		err = errors.New("quota fetcher is not configured")
	} else {
		fetchResult, err = s.fetchFn(ctx, fetchEntry)
	}
	completedAt := time.Now().UTC()
	if err == nil {
		fetchResult, err = normalizeFetchResult(fetchResult, completedAt)
	}
	refreshCommitted := false
	if err == nil && ctx.Err() == nil {
		if !s.refreshTargetIsCurrent(target, entry) {
			result.Outcome = RefreshOutcomeInconclusive
			result.Error = errRefreshTargetChanged.Error()
			result.CompletedAt = time.Now().UTC()
			return result
		}
		if finalizer != nil {
			attempt.Data = append([]byte(nil), fetchResult.Data...)
			if errFinalize := finalizer(ctx, attempt); errFinalize != nil {
				log.WithError(errFinalize).Warnf("quota: finalize refresh for %s", target.FileName)
				err = errors.New(refreshFinalizerErrorMessage)
			}
		}
		refreshCommitted = err == nil
	}

	s.mu.Lock()
	current := s.entries[target.EntryKey]
	if current != entry {
		s.mu.Unlock()
		result.Outcome = RefreshOutcomeInconclusive
		result.Error = errRefreshTargetChanged.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	entry.mu.Lock()
	if !refreshTargetMatchesEntryLocked(target, entry) {
		entry.mu.Unlock()
		s.mu.Unlock()
		result.Outcome = RefreshOutcomeInconclusive
		result.Error = errRefreshTargetChanged.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}
	if errContext := ctx.Err(); errContext != nil && !refreshCommitted {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = errContext
		}
		disabled := entry.Disabled
		disabledConfigured := entry.disabledConfigured
		previousState.StateRevision = entry.StateRevision + 1
		restoreEntryStateLocked(entry, previousState)
		entry.Disabled = disabled
		entry.disabledConfigured = disabledConfigured
		state := entry.snapshotLocked()
		s.history[target.EntryKey] = cloneEntryState(state)
		entry.mu.Unlock()
		s.mu.Unlock()
		s.persist(state)
		result.Outcome = RefreshOutcomeCancelled
		result.Freshness = state.Freshness
		result.SuccessfulEmpty = state.SuccessfulEmpty
		result.Error = cause.Error()
		result.CompletedAt = time.Now().UTC()
		return result
	}

	entry.StateRevision++
	entry.FetchedAt = cloneTime(&completedAt)
	entry.LastRefresh = cloneTime(&completedAt)
	s.updatedAt = completedAt
	if err != nil {
		applyRefreshFailureLocked(entry, err, completedAt, s.cfg)
		log.Warnf(
			"quota: refresh failed for %s [%s]: %v (next retry at %v)",
			entry.FileName, entry.Status, err, entry.RetryAt,
		)
		result.Outcome = RefreshOutcomeFailed
		result.Error = err.Error()
	} else {
		applyRefreshSuccessLocked(entry, fetchResult, completedAt, s.cfg)
		result.Outcome = RefreshOutcomeSucceeded
	}
	state := entry.snapshotLocked()
	s.history[target.EntryKey] = cloneEntryState(state)
	entry.mu.Unlock()
	s.mu.Unlock()
	s.persist(state)
	result.Freshness = state.Freshness
	result.SuccessfulEmpty = state.SuccessfulEmpty
	result.CompletedAt = completedAt
	if result.Outcome == RefreshOutcomeSucceeded && fetchResult.Enrich != nil {
		result = s.enrichRefresh(ctx, target, entry, state.StateRevision, fetchResult, result)
	}
	return result
}

func (s *Scheduler) enrichRefresh(
	ctx context.Context,
	target RefreshTarget,
	entry *Entry,
	revision uint64,
	fetched FetchResult,
	result RefreshResult,
) RefreshResult {
	if ctx.Err() != nil || !s.refreshTargetIsCurrent(target, entry) {
		return result
	}
	enriched, err := fetched.Enrich(ctx)
	now := time.Now().UTC()
	if err == nil && len(enriched.Data) == 0 {
		err = errors.New("optional quota details returned no data")
	}
	if err == nil {
		enriched, err = normalizeFetchResult(enriched, now)
	}
	if err != nil {
		log.WithError(err).Warnf("quota: optional details refresh failed for %s", target.FileName)
		enriched = fetched
		enriched.Completeness = FetchPartial
		if enriched.PartialError != "" {
			enriched.PartialError += "; "
		}
		enriched.PartialError += "quota refreshed, but optional details could not be updated"
	}

	s.mu.Lock()
	if s.entries[target.EntryKey] != entry {
		s.mu.Unlock()
		return result
	}
	entry.mu.Lock()
	if ctx.Err() != nil || !refreshTargetMatchesEntryLocked(target, entry) || entry.StateRevision != revision {
		entry.mu.Unlock()
		s.mu.Unlock()
		return result
	}
	entry.StateRevision++
	applyRefreshSuccessLocked(entry, enriched, now, s.cfg)
	state := entry.snapshotLocked()
	s.history[target.EntryKey] = cloneEntryState(state)
	s.updatedAt = now
	entry.mu.Unlock()
	s.mu.Unlock()
	s.persist(state)
	result.Freshness = state.Freshness
	result.CompletedAt = now
	return result
}

func normalizeFetchResult(result FetchResult, completedAt time.Time) (FetchResult, error) {
	result.Source = strings.TrimSpace(result.Source)
	result.PartialError = strings.TrimSpace(result.PartialError)
	if result.Completeness == "" {
		result.Completeness = FetchComplete
	}
	if result.PartialError != "" && result.Completeness == FetchComplete {
		result.Completeness = FetchPartial
	}
	if result.FetchedAt.IsZero() {
		result.FetchedAt = completedAt
	} else {
		result.FetchedAt = result.FetchedAt.UTC()
	}
	if result.ObservedAt.IsZero() {
		result.ObservedAt = result.FetchedAt
	} else {
		result.ObservedAt = result.ObservedAt.UTC()
	}
	switch result.Completeness {
	case FetchComplete:
	case FetchPartial:
		if len(result.Data) == 0 {
			return FetchResult{}, errors.New("partial quota fetch returned no usable data")
		}
	default:
		return FetchResult{}, fmt.Errorf("invalid quota fetch completeness %q", result.Completeness)
	}
	if len(result.Data) == 0 {
		if !result.AuthoritativeEmpty {
			return FetchResult{}, errors.New("quota fetch returned empty data without authority")
		}
		return result, nil
	}
	if result.AuthoritativeEmpty {
		return FetchResult{}, errors.New("quota fetch marked non-empty data as authoritative empty")
	}
	if !json.Valid(result.Data) {
		return FetchResult{}, errors.New("quota fetch returned invalid JSON")
	}
	return result, nil
}

func applyRefreshFailureLocked(entry *Entry, err error, now time.Time, cfg Config) {
	errMsg := err.Error()
	status, backoffMultiplier := classifyError(errMsg)
	entry.Status = status
	entry.Error = errMsg
	entry.LastError = errMsg
	entry.LastErrorAt = cloneTime(&now)
	entry.FailureCount++
	if len(entry.Data) > 0 || entry.SuccessfulEmpty {
		entry.Freshness = freshnessStale
	} else {
		entry.Freshness = FreshnessOutdated
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultConfig().Interval
	}
	backoff := time.Duration(interval) * time.Second *
		time.Duration(backoffMultiplier) * (1 << min(entry.FailureCount, 5))
	maxInterval := time.Duration(cfg.MaxInterval) * time.Second
	if maxInterval <= 0 {
		maxInterval = time.Duration(DefaultConfig().MaxInterval) * time.Second
	}
	if backoff > maxInterval {
		backoff = maxInterval
	}
	retryAt := now.Add(backoff)
	entry.RetryAt = cloneTime(&retryAt)
	entry.NextRefresh = cloneTime(&retryAt)
}

func applyRefreshSuccessLocked(entry *Entry, fetched FetchResult, now time.Time, cfg Config) {
	entry.Status = StatusSuccess
	entry.Error = ""
	entry.FailureCount = 0
	entry.Source = fetched.Source
	entry.ObservedAt = cloneTime(&fetched.ObservedAt)
	entry.FetchedAt = cloneTime(&fetched.FetchedAt)
	entry.LastRefresh = cloneTime(&fetched.FetchedAt)
	entry.LastSuccessAt = cloneTime(&fetched.FetchedAt)
	entry.RetryAt = nil
	if len(fetched.Data) > 0 {
		entry.Data = append(entry.Data[:0], fetched.Data...)
		entry.SuccessfulEmpty = false
	} else {
		entry.Data = nil
		entry.SuccessfulEmpty = true
	}
	if fetched.Completeness == FetchPartial {
		entry.Freshness = freshnessPartial
		if fetched.PartialError != "" {
			entry.LastError = fetched.PartialError
			entry.LastErrorAt = cloneTime(&now)
		}
	} else {
		entry.Freshness = FreshnessFresh
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultConfig().Interval
	}
	nextRefresh := now.Add(time.Duration(interval) * time.Second)
	entry.NextRefresh = cloneTime(&nextRefresh)
}

func normalizeRestoredEntryState(state EntryState) EntryState {
	if state.Generation == 0 {
		state.Generation = 1
	}
	if state.StateRevision == 0 {
		state.StateRevision = 1
	}
	if state.FetchedAt == nil {
		state.FetchedAt = cloneTime(state.LastRefresh)
	}
	if state.LastRefresh == nil {
		state.LastRefresh = cloneTime(state.FetchedAt)
	}
	if state.LastSuccessAt == nil && state.Status == StatusSuccess {
		state.LastSuccessAt = cloneTime(state.FetchedAt)
	}
	if state.ObservedAt == nil && state.Status == StatusSuccess {
		state.ObservedAt = cloneTime(state.LastSuccessAt)
	}
	if state.LastError == "" {
		state.LastError = state.Error
	}
	if state.LastErrorAt == nil && state.LastError != "" {
		state.LastErrorAt = cloneTime(state.FetchedAt)
	}
	if state.RetryAt == nil && state.Status != StatusSuccess {
		state.RetryAt = cloneTime(state.NextRefresh)
	}
	if state.Freshness == "" {
		switch {
		case state.Tombstone:
			state.Freshness = FreshnessOutdated
		case state.Status == StatusSuccess && (len(state.Data) > 0 || state.SuccessfulEmpty):
			state.Freshness = FreshnessFresh
		case len(state.Data) > 0 || state.SuccessfulEmpty:
			state.Freshness = freshnessStale
		default:
			state.Freshness = FreshnessOutdated
		}
	}
	return state
}

func newEntryForBinding(
	fileName string,
	credType CredentialType,
	authIndex,
	credentialRevision string,
	now time.Time,
) *Entry {
	return &Entry{
		credentialRevision: credentialRevision,
		FileName:           fileName,
		Type:               credType,
		AuthIndex:          authIndex,
		Generation:         1,
		StateRevision:      1,
		Status:             StatusIdle,
		Freshness:          FreshnessOutdated,
		LastRefresh:        nil,
		NextRefresh:        cloneTime(&now),
	}
}

func resetEntryForBindingLocked(entry *Entry, generation uint64) {
	if generation == 0 {
		generation = 1
	}
	entry.Generation = generation
	entry.StateRevision = 1
	entry.Status = StatusIdle
	entry.Freshness = FreshnessOutdated
	entry.Source = ""
	entry.ObservedAt = nil
	entry.FetchedAt = nil
	entry.LastSuccessAt = nil
	entry.LastError = ""
	entry.LastErrorAt = nil
	entry.RetryAt = nil
	entry.SuccessfulEmpty = false
	entry.LastRefresh = nil
	now := time.Now().UTC()
	entry.NextRefresh = cloneTime(&now)
	entry.Error = ""
	entry.FailureCount = 0
	entry.Data = nil
}

func restoreEntryStateLocked(entry *Entry, state EntryState) {
	state = normalizeRestoredEntryState(state)
	entry.Generation = state.Generation
	entry.StateRevision = state.StateRevision
	entry.Status = state.Status
	entry.Freshness = state.Freshness
	entry.Source = state.Source
	entry.ObservedAt = cloneTime(state.ObservedAt)
	entry.FetchedAt = cloneTime(state.FetchedAt)
	entry.LastSuccessAt = cloneTime(state.LastSuccessAt)
	entry.LastError = state.LastError
	entry.LastErrorAt = cloneTime(state.LastErrorAt)
	entry.RetryAt = cloneTime(state.RetryAt)
	entry.SuccessfulEmpty = state.SuccessfulEmpty
	entry.LastRefresh = cloneTime(state.LastRefresh)
	entry.NextRefresh = cloneTime(state.NextRefresh)
	entry.Error = state.Error
	entry.FailureCount = state.FailureCount
	entry.Data = append(entry.Data[:0], state.Data...)
	entry.Disabled = state.Disabled
	if state.CredentialRevision != "" {
		entry.credentialRevision = state.CredentialRevision
	}
}

func tombstoneEntryLocked(entry *Entry) EntryState {
	return EntryState{
		FileName:           entry.FileName,
		Type:               entry.Type,
		AuthIndex:          entry.AuthIndex,
		CredentialRevision: entry.credentialRevision,
		Generation:         entry.Generation + 1,
		StateRevision:      1,
		Tombstone:          true,
		Status:             StatusIdle,
		Freshness:          FreshnessOutdated,
		Disabled:           entry.Disabled,
	}
}

func entryBindingMatchesStateLocked(entry *Entry, state EntryState) bool {
	return entry != nil && bindingMatches(state, entry.AuthIndex, entry.credentialRevision)
}

func bindingMatches(state EntryState, authIndex, credentialRevision string) bool {
	if state.AuthIndex != strings.TrimSpace(authIndex) {
		return false
	}
	stateRevision := strings.TrimSpace(state.CredentialRevision)
	credentialRevision = strings.TrimSpace(credentialRevision)
	return stateRevision == "" || credentialRevision != "" && stateRevision == credentialRevision
}

func entryGenerationPrecedes(candidate, current EntryState) bool {
	return candidate.Generation < current.Generation
}

func cloneEntryState(state EntryState) EntryState {
	state.ObservedAt = cloneTime(state.ObservedAt)
	state.FetchedAt = cloneTime(state.FetchedAt)
	state.LastSuccessAt = cloneTime(state.LastSuccessAt)
	state.LastErrorAt = cloneTime(state.LastErrorAt)
	state.RetryAt = cloneTime(state.RetryAt)
	state.LastRefresh = cloneTime(state.LastRefresh)
	state.NextRefresh = cloneTime(state.NextRefresh)
	state.Data = append(json.RawMessage(nil), state.Data...)
	return state
}

func (s *Scheduler) refreshTargetIsCurrent(target RefreshTarget, expected *Entry) bool {
	s.mu.RLock()
	entry := s.entries[target.EntryKey]
	if entry == nil || entry != expected {
		s.mu.RUnlock()
		return false
	}
	entry.mu.RLock()
	current := refreshTargetMatchesEntryLocked(target, entry)
	entry.mu.RUnlock()
	s.mu.RUnlock()
	return current
}

func refreshTargetMatchesEntryLocked(target RefreshTarget, entry *Entry) bool {
	return entry != nil && target.EntryKey == BindingEntryKey(entry.Type, entry.FileName, entry.AuthIndex) &&
		target.FileName == entry.FileName && target.Type == entry.Type &&
		target.AuthIndex == entry.AuthIndex && target.Generation == entry.Generation
}

// classifyError determines the error status and backoff multiplier from the error message.
// Returns (status, backoffMultiplier) where multiplier scales the base exponential backoff.
func classifyError(errMsg string) (Status, int) {
	if strings.Contains(errMsg, "status 429") || strings.Contains(errMsg, "status 403") {
		return StatusBanned, 4
	}
	if strings.Contains(errMsg, "status 402") {
		return StatusQuotaExceeded, 1
	}
	return StatusError, 1
}

// buildHeap constructs a min-heap of entries sorted by NextRefresh. Must be called with s.mu held.
// Entries that shouldSkipAutoRefresh reports as ineligible (disabled, banned, quota-exceeded)
// are excluded so the dispatcher never wakes for them; manual RefreshNow still reaches them directly.
func (s *Scheduler) buildHeap() entryHeap {
	h := make(entryHeap, 0, len(s.entries))
	for _, e := range s.entries {
		if e.NextRefresh == nil {
			continue
		}
		if e.shouldSkipAutoRefresh() {
			continue
		}
		h = append(h, e)
	}
	heap.Init(&h)
	return h
}

// entryHeap implements heap.Interface for Entry pointers, ordered by NextRefresh.
type entryHeap []*Entry

func (h *entryHeap) Len() int { return len(*h) }
func (h *entryHeap) Less(i, j int) bool {
	entries := *h
	if entries[i].NextRefresh == nil {
		return false
	}
	if entries[j].NextRefresh == nil {
		return true
	}
	return entries[i].NextRefresh.Before(*entries[j].NextRefresh)
}
func (h *entryHeap) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }

func (h *entryHeap) Push(x any) {
	*h = append(*h, x.(*Entry))
}

func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
