package auth

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const quotaAwareInFlightLeaseTTL = 24 * time.Hour

// QuotaSchedulingStatus describes whether a quota snapshot can influence routing.
type QuotaSchedulingStatus string

const (
	QuotaSchedulingStatusFresh   QuotaSchedulingStatus = "fresh"
	QuotaSchedulingStatusStale   QuotaSchedulingStatus = "stale"
	QuotaSchedulingStatusUnknown QuotaSchedulingStatus = "unknown"
)

// QuotaSchedulingSnapshot contains one provider-neutral quota signal.
type QuotaSchedulingSnapshot struct {
	Status   QuotaSchedulingStatus
	Headroom float64
	ResetAt  *time.Time
}

// QuotaSchedulingProvider returns immutable snapshots keyed by auth index.
type QuotaSchedulingProvider interface {
	Snapshots(provider, model string, authIndexes []string) map[string]QuotaSchedulingSnapshot
}

// QuotaSchedulingProviderFunc adapts a function to QuotaSchedulingProvider.
type QuotaSchedulingProviderFunc func(
	provider,
	model string,
	authIndexes []string,
) map[string]QuotaSchedulingSnapshot

func (f QuotaSchedulingProviderFunc) Snapshots(
	provider,
	model string,
	authIndexes []string,
) map[string]QuotaSchedulingSnapshot {
	if f == nil {
		return nil
	}
	return f(provider, model, authIndexes)
}

type quotaAwareCandidateSignal struct {
	status       QuotaSchedulingStatus
	headroom     float64
	resetAt      *time.Time
	resetAfterMS *int64
	inFlight     int64
	poolKey      string
}

type quotaAwareViewState struct {
	lastPickedPool string
	poolStates     map[string]smoothWeightedState
}

func cloneQuotaAwareViewState(state quotaAwareViewState) quotaAwareViewState {
	cloned := quotaAwareViewState{lastPickedPool: state.lastPickedPool}
	if len(state.poolStates) > 0 {
		cloned.poolStates = make(map[string]smoothWeightedState, len(state.poolStates))
		for poolKey, poolState := range state.poolStates {
			cloned.poolStates[poolKey] = cloneSmoothWeightedState(poolState)
		}
	}
	return cloned
}

func cloneQuotaSchedulingTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type quotaInFlightRecord struct {
	poolKey   string
	expiresAt time.Time
}

type quotaInFlightLease struct {
	scheduler *authScheduler
	id        uint64
	once      sync.Once
}

func (l *quotaInFlightLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(
		func() {
			if l.scheduler != nil {
				l.scheduler.releaseQuotaInFlight(l.id)
			}
		},
	)
}

type quotaInFlightRequest struct {
	mu    sync.Mutex
	lease *quotaInFlightLease
}

func (r *quotaInFlightRequest) set(lease *quotaInFlightLease) {
	if r == nil || lease == nil {
		return
	}
	r.mu.Lock()
	if r.lease == nil {
		r.lease = lease
		lease = nil
	}
	r.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (r *quotaInFlightRequest) get() *quotaInFlightLease {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease
}

func (r *quotaInFlightRequest) take() *quotaInFlightLease {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	lease := r.lease
	r.lease = nil
	r.mu.Unlock()
	return lease
}

func (r *quotaInFlightRequest) release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	lease := r.lease
	r.lease = nil
	r.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

type quotaInFlightRequestContextKey struct{}

type quotaInFlightTransfer struct {
	lease   *quotaInFlightLease
	claimed atomic.Bool
}

type quotaInFlightTransferContextKey struct{}

func withQuotaInFlightRequest(ctx context.Context) (context.Context, *quotaInFlightRequest) {
	if ctx == nil {
		ctx = context.Background()
	}
	request := &quotaInFlightRequest{}
	return context.WithValue(ctx, quotaInFlightRequestContextKey{}, request), request
}

func quotaInFlightRequestFromContext(ctx context.Context) *quotaInFlightRequest {
	if ctx == nil {
		return nil
	}
	request, _ := ctx.Value(quotaInFlightRequestContextKey{}).(*quotaInFlightRequest)
	return request
}

func withQuotaInFlightTransfer(ctx context.Context, lease *quotaInFlightLease) (
	context.Context,
	*quotaInFlightTransfer,
) {
	if lease == nil {
		return ctx, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	transfer := &quotaInFlightTransfer{lease: lease}
	return context.WithValue(ctx, quotaInFlightTransferContextKey{}, transfer), transfer
}

func takeQuotaInFlightLease(ctx context.Context) *quotaInFlightLease {
	if ctx == nil {
		return nil
	}
	transfer, _ := ctx.Value(quotaInFlightTransferContextKey{}).(*quotaInFlightTransfer)
	if transfer == nil || transfer.lease == nil || !transfer.claimed.CompareAndSwap(false, true) {
		return nil
	}
	return transfer.lease
}

func quotaPoolKey(auth *Auth, provider string) string {
	if auth == nil {
		return ""
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	}
	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		auth.EnsureIndex()
		authIndex = strings.TrimSpace(auth.Index)
	}
	if provider == "" || authIndex == "" {
		return ""
	}
	return provider + "\x00" + authIndex
}

func (s *authScheduler) setQuotaSchedulingProvider(provider QuotaSchedulingProvider) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.quotaProvider = provider
	s.mu.Unlock()
}

func (s *authScheduler) quotaSignalsLocked(
	entries []*scheduledAuth,
	model string,
	now time.Time,
) (map[string]quotaAwareCandidateSignal, bool) {
	signals := make(map[string]quotaAwareCandidateSignal, len(entries))
	if len(entries) == 0 {
		return signals, false
	}
	s.expireQuotaInFlightLocked(now)
	byProvider := make(map[string]map[string]struct{})
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || entry.meta == nil {
			continue
		}
		provider := entry.meta.providerKey
		authIndex := strings.TrimSpace(entry.auth.Index)
		if authIndex == "" {
			entry.auth.EnsureIndex()
			authIndex = strings.TrimSpace(entry.auth.Index)
		}
		if provider == "" || authIndex == "" {
			continue
		}
		indexes := byProvider[provider]
		if indexes == nil {
			indexes = make(map[string]struct{})
			byProvider[provider] = indexes
		}
		indexes[authIndex] = struct{}{}
	}

	providerSnapshots := make(map[string]map[string]QuotaSchedulingSnapshot, len(byProvider))
	if s.quotaProvider != nil {
		for provider, indexSet := range byProvider {
			indexes := make([]string, 0, len(indexSet))
			for authIndex := range indexSet {
				indexes = append(indexes, authIndex)
			}
			sort.Strings(indexes)
			providerSnapshots[provider] = s.quotaProvider.Snapshots(provider, model, indexes)
		}
	}

	allFresh := true
	freshPools := make(map[string]bool)
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || entry.meta == nil {
			allFresh = false
			continue
		}
		provider := entry.meta.providerKey
		authIndex := strings.TrimSpace(entry.auth.Index)
		poolKey := quotaPoolKey(entry.auth, provider)
		snapshot, found := providerSnapshots[provider][authIndex]
		status := QuotaSchedulingStatusUnknown
		if found {
			status = snapshot.Status
		}
		if status != QuotaSchedulingStatusFresh || math.IsNaN(snapshot.Headroom) ||
			math.IsInf(snapshot.Headroom, 0) || snapshot.Headroom < 0 || snapshot.Headroom > 1 {
			if status == QuotaSchedulingStatusFresh {
				status = QuotaSchedulingStatusStale
			}
			allFresh = false
		}
		signal := quotaAwareCandidateSignal{
			status:   status,
			headroom: snapshot.Headroom,
			resetAt:  cloneQuotaSchedulingTime(snapshot.ResetAt),
			inFlight: s.quotaInFlightByPool[poolKey],
			poolKey:  poolKey,
		}
		if signal.resetAt != nil {
			resetAfter := max(signal.resetAt.Sub(now).Milliseconds(), 0)
			signal.resetAfterMS = &resetAfter
		}
		signals[entry.auth.ID] = signal
		if status == QuotaSchedulingStatusFresh {
			freshPools[poolKey] = true
		}
	}
	if len(freshPools) != len(uniqueQuotaPools(entries)) {
		allFresh = false
	}
	return signals, allFresh
}

func uniqueQuotaPools(entries []*scheduledAuth) map[string]struct{} {
	pools := make(map[string]struct{})
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || entry.meta == nil {
			continue
		}
		if poolKey := quotaPoolKey(entry.auth, entry.meta.providerKey); poolKey != "" {
			pools[poolKey] = struct{}{}
		}
	}
	return pools
}

func (s *authScheduler) pickQuotaAwareLocked(
	entries []*scheduledAuth,
	predicate func(*scheduledAuth) bool,
	state *quotaAwareViewState,
	signals map[string]quotaAwareCandidateSignal,
	allFresh bool,
) (*scheduledAuth, []string) {
	if len(entries) == 0 || state == nil {
		return nil, nil
	}
	if !allFresh {
		fallback := &smoothWeightedState{}
		if state.poolStates == nil {
			state.poolStates = make(map[string]smoothWeightedState)
		}
		if saved, ok := state.poolStates[""]; ok {
			*fallback = saved
		}
		fallback.prepare(scheduledWeightVectorMatching(entries, predicate))
		picked := pickSmoothWeightedScheduled(entries, fallback, predicate)
		state.poolStates[""] = *fallback
		return picked, []string{"quota_weighted_fallback"}
	}

	type poolCandidate struct {
		key      string
		headroom float64
		inFlight int64
		resetAt  *time.Time
		entries  []*scheduledAuth
		score    float64
	}
	byPool := make(map[string]*poolCandidate)
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || (predicate != nil && !predicate(entry)) {
			continue
		}
		signal, ok := signals[entry.auth.ID]
		if !ok || signal.poolKey == "" || signal.status != QuotaSchedulingStatusFresh {
			return nil, []string{"quota_weighted_fallback"}
		}
		pool := byPool[signal.poolKey]
		if pool == nil {
			pool = &poolCandidate{
				key:      signal.poolKey,
				headroom: signal.headroom,
				inFlight: signal.inFlight,
				resetAt:  cloneQuotaSchedulingTime(signal.resetAt),
			}
			pool.score = pool.headroom / float64(pool.inFlight+1)
			byPool[signal.poolKey] = pool
		}
		pool.entries = append(pool.entries, entry)
	}
	if len(byPool) == 0 {
		return nil, nil
	}
	pools := make([]*poolCandidate, 0, len(byPool))
	for _, pool := range byPool {
		pools = append(pools, pool)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].key < pools[j].key })

	bestScore := pools[0].score
	scoreTied := []*poolCandidate{pools[0]}
	for _, pool := range pools[1:] {
		switch {
		case pool.score > bestScore:
			bestScore = pool.score
			scoreTied = []*poolCandidate{pool}
		case pool.score == bestScore:
			scoreTied = append(scoreTied, pool)
		}
	}
	rules := []string{"quota_available_score"}
	resetTied := scoreTied
	if len(scoreTied) > 1 {
		var earliest *time.Time
		for _, pool := range scoreTied {
			if pool.resetAt != nil && (earliest == nil || pool.resetAt.Before(*earliest)) {
				earliest = pool.resetAt
			}
		}
		if earliest != nil {
			resetTied = resetTied[:0]
			for _, pool := range scoreTied {
				if pool.resetAt != nil && pool.resetAt.Equal(*earliest) {
					resetTied = append(resetTied, pool)
				}
			}
			rules = append(rules, "quota_reset_proximity")
		}
	}
	selectedPool := resetTied[0]
	if len(resetTied) > 1 {
		poolKeys := make([]string, 0, len(resetTied))
		byKey := make(map[string]*poolCandidate, len(resetTied))
		for _, pool := range resetTied {
			poolKeys = append(poolKeys, pool.key)
			byKey[pool.key] = pool
		}
		sort.Strings(poolKeys)
		selectedPool = byKey[poolKeys[nextSortedStringIndex(poolKeys, state.lastPickedPool)]]
		rules = append(rules, "quota_pool_cursor")
	}
	state.lastPickedPool = selectedPool.key
	if state.poolStates == nil {
		state.poolStates = make(map[string]smoothWeightedState)
	}
	poolState := state.poolStates[selectedPool.key]
	sort.Slice(
		selectedPool.entries, func(i, j int) bool {
			return selectedPool.entries[i].auth.ID < selectedPool.entries[j].auth.ID
		},
	)
	poolState.prepare(scheduledWeightVectorMatching(selectedPool.entries, predicate))
	picked := pickSmoothWeightedScheduled(selectedPool.entries, &poolState, predicate)
	state.poolStates[selectedPool.key] = poolState
	return picked, rules
}

func (s *authScheduler) captureQuotaInFlightLeaseLocked(
	ctx context.Context,
	auth *Auth,
	provider string,
	now time.Time,
) {
	request := quotaInFlightRequestFromContext(ctx)
	if request == nil {
		return
	}
	request.set(s.acquireQuotaInFlightLocked(auth, provider, now))
}

func (s *authScheduler) captureQuotaInFlightLease(ctx context.Context, auth *Auth, provider string) {
	if s == nil || quotaInFlightRequestFromContext(ctx) == nil {
		return
	}
	s.mu.Lock()
	s.captureQuotaInFlightLeaseLocked(ctx, auth, provider, time.Now())
	s.mu.Unlock()
}

func (s *authScheduler) acquireQuotaInFlightLocked(auth *Auth, provider string, now time.Time) *quotaInFlightLease {
	if s == nil || auth == nil || s.strategy != schedulerStrategyQuotaAware {
		return nil
	}
	poolKey := quotaPoolKey(auth, provider)
	if poolKey == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.expireQuotaInFlightLocked(now)
	s.quotaInFlightSeq++
	if s.quotaInFlightSeq == 0 {
		s.quotaInFlightSeq++
	}
	id := s.quotaInFlightSeq
	if s.quotaInFlight == nil {
		s.quotaInFlight = make(map[uint64]quotaInFlightRecord)
	}
	if s.quotaInFlightByPool == nil {
		s.quotaInFlightByPool = make(map[string]int64)
	}
	s.quotaInFlight[id] = quotaInFlightRecord{poolKey: poolKey, expiresAt: now.Add(quotaAwareInFlightLeaseTTL)}
	s.quotaInFlightByPool[poolKey]++
	return &quotaInFlightLease{scheduler: s, id: id}
}

func (s *authScheduler) releaseQuotaInFlight(id uint64) {
	if s == nil || id == 0 {
		return
	}
	s.mu.Lock()
	s.releaseQuotaInFlightLocked(id)
	s.mu.Unlock()
}

func (s *authScheduler) releaseQuotaInFlightLocked(id uint64) {
	record, ok := s.quotaInFlight[id]
	if !ok {
		return
	}
	delete(s.quotaInFlight, id)
	if count := s.quotaInFlightByPool[record.poolKey]; count <= 1 {
		delete(s.quotaInFlightByPool, record.poolKey)
	} else {
		s.quotaInFlightByPool[record.poolKey] = count - 1
	}
}

func (s *authScheduler) expireQuotaInFlightLocked(now time.Time) {
	for id, record := range s.quotaInFlight {
		if !record.expiresAt.IsZero() && !record.expiresAt.After(now) {
			s.releaseQuotaInFlightLocked(id)
		}
	}
}
