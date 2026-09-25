package auth

import (
	"context"
	"errors"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// schedulerStrategy identifies which built-in routing semantics the scheduler should apply.
type schedulerStrategy int

const (
	schedulerStrategyCustom schedulerStrategy = iota
	schedulerStrategyRoundRobin
	schedulerStrategyFillFirst
	schedulerStrategyWeightedRoundRobin
	schedulerStrategyQuotaAware
)

// scheduledState describes how an auth currently participates in a model shard.
type scheduledState int

const (
	scheduledStateReady scheduledState = iota
	scheduledStateCooldown
	scheduledStateBlocked
	scheduledStateDisabled
)

// scheduledAuthVersion orders live snapshots and removal tombstones for one auth ID.
type scheduledAuthVersion struct {
	generation uint64
	updatedAt  time.Time
	tombstone  bool
}

// authScheduler keeps the incremental provider/model scheduling state used by Manager.
type authScheduler struct {
	mu                  sync.Mutex
	strategy            schedulerStrategy
	providers           map[string]*providerScheduler
	authProviders       map[string]string
	authVersions        map[string]scheduledAuthVersion
	mixedCursors        map[string]string
	mixedWeightedStates map[string]*smoothWeightedState
	mixedQuotaStates    map[string]*quotaAwareViewState
	quotaProvider       QuotaSchedulingProvider
	quotaInFlight       map[uint64]quotaInFlightRecord
	quotaInFlightByPool map[string]int64
	quotaInFlightSeq    uint64
	checkpoint          *schedulerCheckpointRuntime
}

// providerScheduler stores auth metadata and model shards for a single provider.
type providerScheduler struct {
	providerKey string
	auths       map[string]*scheduledAuthMeta
	modelShards map[string]*modelScheduler
}

// scheduledAuthMeta stores the immutable scheduling fields derived from an auth snapshot.
type scheduledAuthMeta struct {
	auth              *Auth
	providerKey       string
	priority          int
	weight            int64
	virtualParent     string
	websocketEnabled  bool
	supportedModelSet map[string]struct{}
	registryEpoch     uint64
}

// modelScheduler tracks ready and blocked auths for one provider/model combination.
type modelScheduler struct {
	modelKey               string
	entries                map[string]*scheduledAuth
	priorityOrder          []int
	readyByPriority        map[int]*readyBucket
	cursorStatesByPriority map[int]readyBucketCursorState
	blocked                cooldownQueue
}

// scheduledAuth stores the runtime scheduling state for a single auth inside a model shard.
type scheduledAuth struct {
	meta        *scheduledAuthMeta
	auth        *Auth
	state       scheduledState
	nextRetryAt time.Time
}

// readyBucket keeps the ready views for one priority level.
type readyBucket struct {
	all readyView
	ws  readyView
}

// readyView holds the selection order for flat or grouped round-robin traversal.
type readyView struct {
	flat             []*scheduledAuth
	lastPickedAuth   string
	parentOrder      []string
	lastPickedParent string
	children         map[string]*childBucket
	weightedState    smoothWeightedState
	quotaState       quotaAwareViewState
}

// childBucket keeps the per-parent rotation state for grouped Gemini virtual auths.
type childBucket struct {
	items          []*scheduledAuth
	lastPickedAuth string
}

// cooldownQueue is the blocked auth collection ordered by next retry time during rebuilds.
type cooldownQueue []*scheduledAuth

type readyViewCursorState struct {
	lastPickedAuth   string
	lastPickedParent string
	childLastPicked  map[string]string
	weightedState    smoothWeightedState
	quotaState       quotaAwareViewState
}

type readyBucketCursorState struct {
	all readyViewCursorState
	ws  readyViewCursorState
}

func snapshotReadyViewCursors(view readyView) readyViewCursorState {
	state := readyViewCursorState{
		lastPickedAuth:   view.lastPickedAuth,
		lastPickedParent: view.lastPickedParent,
		weightedState:    cloneSmoothWeightedState(view.weightedState),
		quotaState:       cloneQuotaAwareViewState(view.quotaState),
	}
	if len(view.children) == 0 {
		return state
	}
	state.childLastPicked = make(map[string]string, len(view.children))
	for parent, child := range view.children {
		if child == nil || child.lastPickedAuth == "" {
			continue
		}
		state.childLastPicked[parent] = child.lastPickedAuth
	}
	return state
}

func restoreReadyViewCursors(view *readyView, state readyViewCursorState) {
	if view == nil {
		return
	}
	view.lastPickedAuth = state.lastPickedAuth
	view.lastPickedParent = state.lastPickedParent
	weights := scheduledWeightVector(view.flat)
	view.weightedState.lastPicked = state.weightedState.lastPicked
	if len(state.weightedState.current) > 0 && !weightsConfigChanged(state.weightedState.weights, weights) {
		current := make(map[string]int64, len(view.flat))
		for _, entry := range view.flat {
			if entry == nil || entry.auth == nil {
				continue
			}
			if value, ok := state.weightedState.current[entry.auth.ID]; ok {
				current[entry.auth.ID] = value
			}
		}
		view.weightedState.current = current
		view.weightedState.weights = weights
	}
	view.quotaState = cloneQuotaAwareViewState(state.quotaState)
	if len(view.children) == 0 || len(state.childLastPicked) == 0 {
		return
	}
	for parent, child := range view.children {
		if child == nil {
			continue
		}
		child.lastPickedAuth = state.childLastPicked[parent]
	}
}

func cloneSmoothWeightedState(state smoothWeightedState) smoothWeightedState {
	cloned := smoothWeightedState{lastPicked: state.lastPicked}
	if len(state.current) > 0 {
		cloned.current = make(map[string]int64, len(state.current))
		maps.Copy(cloned.current, state.current)
	}
	if len(state.weights) > 0 {
		cloned.weights = make(map[string]int64, len(state.weights))
		maps.Copy(cloned.weights, state.weights)
	}
	return cloned
}

func cloneReadyViewCursorState(state readyViewCursorState) readyViewCursorState {
	cloned := readyViewCursorState{
		lastPickedAuth:   state.lastPickedAuth,
		lastPickedParent: state.lastPickedParent,
		weightedState:    cloneSmoothWeightedState(state.weightedState),
		quotaState:       cloneQuotaAwareViewState(state.quotaState),
	}
	if len(state.childLastPicked) > 0 {
		cloned.childLastPicked = make(map[string]string, len(state.childLastPicked))
		maps.Copy(cloned.childLastPicked, state.childLastPicked)
	}
	return cloned
}

func cloneReadyBucketCursorState(state readyBucketCursorState) readyBucketCursorState {
	return readyBucketCursorState{
		all: cloneReadyViewCursorState(state.all),
		ws:  cloneReadyViewCursorState(state.ws),
	}
}

func (m *modelScheduler) snapshotCursorStatesLocked() map[int]readyBucketCursorState {
	states := make(map[int]readyBucketCursorState, len(m.cursorStatesByPriority)+len(m.readyByPriority))
	for priority, state := range m.cursorStatesByPriority {
		states[priority] = cloneReadyBucketCursorState(state)
	}
	for priority, bucket := range m.readyByPriority {
		if bucket == nil {
			continue
		}
		states[priority] = readyBucketCursorState{
			all: snapshotReadyViewCursors(bucket.all),
			ws:  snapshotReadyViewCursors(bucket.ws),
		}
	}
	return states
}

func (m *modelScheduler) hasPriorityLocked(priority int) bool {
	if m == nil {
		return false
	}
	for _, entry := range m.entries {
		if entry != nil && entry.meta != nil && entry.meta.priority == priority {
			return true
		}
	}
	return false
}

// newAuthScheduler constructs an empty scheduler configured for the supplied selector strategy.
func newAuthScheduler(selector Selector) *authScheduler {
	return &authScheduler{
		strategy:            selectorStrategy(selector),
		providers:           make(map[string]*providerScheduler),
		authProviders:       make(map[string]string),
		authVersions:        make(map[string]scheduledAuthVersion),
		mixedCursors:        make(map[string]string),
		mixedWeightedStates: make(map[string]*smoothWeightedState),
		mixedQuotaStates:    make(map[string]*quotaAwareViewState),
		quotaInFlight:       make(map[uint64]quotaInFlightRecord),
		quotaInFlightByPool: make(map[string]int64),
	}
}

// selectorStrategy maps a selector implementation to the scheduler semantics it should emulate.
func unwrapSchedulerSelector(selector Selector) Selector {
	for {
		sessionAffinitySelector, ok := selector.(*SessionAffinitySelector)
		if !ok || sessionAffinitySelector == nil {
			return selector
		}
		fallback := sessionAffinitySelector.FallbackSelector()
		if fallback == nil || fallback == selector {
			return selector
		}
		selector = fallback
	}
}

func schedulerStrategyUsesWeights(strategy schedulerStrategy) bool {
	return strategy == schedulerStrategyWeightedRoundRobin || strategy == schedulerStrategyQuotaAware
}

func selectorStrategy(selector Selector) schedulerStrategy {
	switch unwrapSchedulerSelector(selector).(type) {
	case *FillFirstSelector:
		return schedulerStrategyFillFirst
	case *WeightedRoundRobinSelector:
		return schedulerStrategyWeightedRoundRobin
	case *QuotaAwareSelector:
		return schedulerStrategyQuotaAware
	case nil, *RoundRobinSelector:
		return schedulerStrategyRoundRobin
	default:
		return schedulerStrategyCustom
	}
}

func (s *authScheduler) setSelector(selector Selector) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strategy = selectorStrategy(selector)
	clear(s.mixedCursors)
	clear(s.mixedWeightedStates)
	clear(s.mixedQuotaStates)
	s.markCheckpointDirtyLocked()
}

func (s *authScheduler) upsertAuth(auth *Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertAuthLocked(auth, time.Now())
}

// upsertAuthResult updates only the model shards affected by a runtime result.
func (s *authScheduler) upsertAuthResult(auth *Auth, targetModels []string, fullClient bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertAuthResultLocked(auth, targetModels, fullClient, time.Now())
}

// recordRemovalTombstone removes an auth only when the supplied publication generation
// is not older than the scheduler's current snapshot. An authoritative removal in the
// same generation wins even when its timestamp is behind the observed live snapshot.
func (s *authScheduler) recordRemovalTombstone(authID string, generation uint64, removedAt time.Time) bool {
	if s == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	if removedAt.IsZero() {
		removedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordRemovalTombstoneLocked(authID, scheduledAuthVersion{
		generation: generation,
		updatedAt:  removedAt,
		tombstone:  true,
	})
}

// rebuild recreates the complete scheduler state from an auth snapshot.
func (s *authScheduler) rebuild(auths []*Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.snapshotStateLocked()
	s.providers = make(map[string]*providerScheduler)
	s.authProviders = make(map[string]string)
	if s.authVersions == nil {
		s.authVersions = make(map[string]scheduledAuthVersion)
	}
	s.mixedCursors = make(map[string]string)
	s.mixedWeightedStates = make(map[string]*smoothWeightedState)
	s.mixedQuotaStates = make(map[string]*quotaAwareViewState)
	now := time.Now()
	for _, auth := range auths {
		s.upsertAuthLocked(auth, now)
	}
	s.restoreStateLocked(state, now)
}

// pickSingle returns the next auth for a single provider/model request using scheduler state.
func (s *authScheduler) pickSingle(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
) (*Auth, error) {
	return s.pickSingleWithPredicate(ctx, provider, model, opts, tried, nil)
}

func providerPrefersWebsocketTransport(providerKey string) bool {
	switch strings.ToLower(strings.TrimSpace(providerKey)) {
	case "codex", "xai":
		return true
	default:
		return false
	}
}

func appendScheduledStateExclusions(
	events *[]cliproxyexecutor.RouteTraceEvent,
	opts cliproxyexecutor.Options,
	shard *modelScheduler,
	provider,
	model string,
) {
	if routeTraceCallback(opts.Metadata) == nil || shard == nil {
		return
	}
	now := time.Now()
	for _, entry := range shard.entries {
		if entry == nil || entry.auth == nil || entry.state == scheduledStateReady {
			continue
		}
		reason := "unavailable"
		// The loop precondition excludes scheduledStateReady.
		// noinspection GoSwitchMissingCasesForIotaConsts
		switch entry.state {
		case scheduledStateCooldown:
			reason = "cooldown"
		case scheduledStateDisabled:
			reason = "disabled"
		case scheduledStateBlocked:
			reason = "blocked"
		}
		retryAfterMS := int64(0)
		if !entry.nextRetryAt.IsZero() && entry.nextRetryAt.After(now) {
			retryAfterMS = entry.nextRetryAt.Sub(now).Milliseconds()
		}
		appendAuthRouteTraceEvent(
			events, opts, entry.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:     provider,
				Model:        model,
				Action:       "candidate.excluded",
				Reason:       reason,
				RetryAfterMS: retryAfterMS,
			},
		)
	}
}

func appendSchedulerSelection(
	events *[]cliproxyexecutor.RouteTraceEvent,
	opts cliproxyexecutor.Options,
	decision *routeDecisionDraft,
	auth *Auth,
	provider,
	model string,
	strategy schedulerStrategy,
	matchedRules ...string,
) {
	reason := schedulerRouteTraceReason(strategy)
	decision.selectCandidate(auth, model, reason)
	decision.addSelectedRules(auth, matchedRules...)
	appendAuthRouteTraceEvent(
		events, opts, auth, cliproxyexecutor.RouteTraceEvent{
			Provider: provider,
			Model:    model,
			Action:   "candidate.selected",
			Reason:   reason,
		},
	)
}

func (s *authScheduler) pickSingleWithPredicate(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
	requestPredicate func(*Auth) bool,
) (*Auth, error) {
	if s == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	modelKey := canonicalModelKey(model)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	preferWebsocket := cliproxyexecutor.DownstreamWebsocket(ctx) && providerPrefersWebsocketTransport(providerKey) && pinnedAuthID == ""

	var traceEvents []cliproxyexecutor.RouteTraceEvent
	s.mu.Lock()
	decision := newRouteDecisionDraft(opts, []string{providerKey}, model, s.strategy)
	defer func() {
		appendRouteDecisionEvent(&traceEvents, opts, decision)
		s.mu.Unlock()
		flushRouteTraceEvents(opts.Metadata, traceEvents)
	}()
	providerState := s.providers[providerKey]
	if providerState == nil {
		attempt, previousAttempt := routeTraceAttempt(opts)
		appendRouteTraceEvent(
			&traceEvents, opts.Metadata, cliproxyexecutor.RouteTraceEvent{
				Attempt:         attempt,
				PreviousAttempt: previousAttempt,
				Provider:        providerKey,
				Model:           model,
				Action:          "provider.excluded",
				Reason:          "provider_not_registered",
			},
		)
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	shard := providerState.ensureModelLocked(modelKey, time.Now())
	if shard == nil {
		attempt, previousAttempt := routeTraceAttempt(opts)
		appendRouteTraceEvent(
			&traceEvents, opts.Metadata, cliproxyexecutor.RouteTraceEvent{
				Attempt:         attempt,
				PreviousAttempt: previousAttempt,
				Provider:        providerKey,
				Model:           model,
				Action:          "provider.excluded",
				Reason:          "model_shard_missing",
			},
		)
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	shard.promoteExpiredLocked(time.Now())
	decision.mergeShard(shard, model)
	appendScheduledStateExclusions(&traceEvents, opts, shard, providerKey, model)
	tracedExclusions := make(map[string]struct{})
	traceExclusion := func(entry *scheduledAuth, reason string) {
		if routeTraceCallback(opts.Metadata) == nil || entry == nil || entry.auth == nil {
			return
		}
		decision.exclude(entry.auth, model, reason)
		key := entry.auth.ID + "\x00" + reason
		if _, exists := tracedExclusions[key]; exists {
			return
		}
		tracedExclusions[key] = struct{}{}
		appendAuthRouteTraceEvent(
			&traceEvents, opts, entry.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: providerKey,
				Model:    model,
				Action:   "candidate.excluded",
				Reason:   reason,
			},
		)
	}
	predicate := func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if schedulerStrategyUsesWeights(s.strategy) && (entry.meta == nil || entry.meta.weight <= 0) {
			traceExclusion(entry, "weight_non_positive")
			return false
		}
		if pinnedAuthID != "" && entry.auth.ID != pinnedAuthID {
			traceExclusion(entry, "pinned_auth_mismatch")
			return false
		}
		if requestPredicate != nil && !requestPredicate(entry.auth) {
			traceExclusion(entry, "request_filter")
			return false
		}
		if len(tried) > 0 {
			if _, ok := tried[entry.auth.ID]; ok {
				traceExclusion(entry, "already_tried")
				return false
			}
		}
		return true
	}
	if s.strategy == schedulerStrategyQuotaAware {
		priority, okPriority := shard.highestReadyPriorityLocked(preferWebsocket, predicate)
		if okPriority {
			entries := shard.readyEntriesAtPriorityLocked(preferWebsocket, priority, predicate)
			signals, allFresh := s.quotaSignalsLocked(entries, model, time.Now())
			decision.mergeQuotaSignals(entries, signals)
			view := shard.readyViewAtPriorityLocked(preferWebsocket, priority, predicate)
			if view != nil {
				pickedEntry, quotaRules := s.pickQuotaAwareLocked(
					entries, predicate, &view.quotaState, signals, allFresh,
				)
				if pickedEntry != nil && pickedEntry.auth != nil {
					picked := pickedEntry.auth
					s.captureQuotaInFlightLeaseLocked(ctx, picked, providerKey, time.Now())
					s.markCheckpointDirtyLocked()
					matchedRules := shard.routeDecisionSelectionRulesLocked(
						picked, preferWebsocket, s.strategy, true,
					)
					matchedRules = append(matchedRules, quotaRules...)
					appendSchedulerSelection(
						&traceEvents, opts, decision, picked, providerKey, model, s.strategy, matchedRules...,
					)
					return picked, nil
				}
			}
		}
	} else if picked := shard.pickReadyLocked(preferWebsocket, s.strategy, predicate); picked != nil {
		s.markCheckpointDirtyLocked()
		appendSchedulerSelection(
			&traceEvents,
			opts,
			decision,
			picked,
			providerKey,
			model,
			s.strategy,
			shard.routeDecisionSelectionRulesLocked(picked, preferWebsocket, s.strategy, true)...,
		)
		return picked, nil
	}
	return nil, shard.unavailableErrorLocked(provider, model, predicate)
}

// pickMixed returns the next auth and provider for a mixed-provider request.
func (s *authScheduler) pickMixed(
	ctx context.Context,
	providers []string,
	model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
) (*Auth, string, error) {
	return s.pickMixedWithPredicate(ctx, providers, model, opts, tried, nil)
}

func (s *authScheduler) pickMixedWithPredicate(
	ctx context.Context,
	providers []string,
	model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
	requestPredicate func(*Auth) bool,
) (*Auth, string, error) {
	if s == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	normalized := normalizeProviderKeys(providers)
	if len(normalized) == 0 {
		return nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if len(normalized) == 1 {
		picked, errPick := s.pickSingleWithPredicate(
			ctx, normalized[0], model, opts, tried, requestPredicate,
		)
		return picked, normalized[0], errPick
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	modelKey := canonicalModelKey(model)

	var traceEvents []cliproxyexecutor.RouteTraceEvent
	s.mu.Lock()
	decision := newRouteDecisionDraft(opts, normalized, model, s.strategy)
	defer func() {
		appendRouteDecisionEvent(&traceEvents, opts, decision)
		s.mu.Unlock()
		flushRouteTraceEvents(opts.Metadata, traceEvents)
	}()
	if pinnedAuthID != "" {
		providerKey := s.authProviders[pinnedAuthID]
		if providerKey == "" || !containsProvider(normalized, providerKey) {
			attempt, previousAttempt := routeTraceAttempt(opts)
			appendRouteTraceEvent(
				&traceEvents, opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attempt,
					PreviousAttempt: previousAttempt,
					Provider:        "mixed",
					Model:           model,
					Action:          "provider.excluded",
					Reason:          "pinned_auth_provider_unavailable",
				},
			)
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		providerState := s.providers[providerKey]
		if providerState == nil {
			attempt, previousAttempt := routeTraceAttempt(opts)
			appendRouteTraceEvent(
				&traceEvents, opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attempt,
					PreviousAttempt: previousAttempt,
					Provider:        providerKey,
					Model:           model,
					Action:          "provider.excluded",
					Reason:          "provider_not_registered",
				},
			)
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		shard := providerState.ensureModelLocked(modelKey, time.Now())
		shard.promoteExpiredLocked(time.Now())
		decision.mergeShard(shard, model)
		appendScheduledStateExclusions(&traceEvents, opts, shard, providerKey, model)
		tracedExclusions := make(map[string]struct{})
		traceExclusion := func(entry *scheduledAuth, reason string) {
			if routeTraceCallback(opts.Metadata) == nil || entry == nil || entry.auth == nil {
				return
			}
			key := entry.auth.ID + "\x00" + reason
			if _, exists := tracedExclusions[key]; exists {
				return
			}
			tracedExclusions[key] = struct{}{}
			appendAuthRouteTraceEvent(
				&traceEvents, opts, entry.auth, cliproxyexecutor.RouteTraceEvent{
					Provider: providerKey,
					Model:    model,
					Action:   "candidate.excluded",
					Reason:   reason,
				},
			)
		}
		predicate := func(entry *scheduledAuth) bool {
			if entry == nil || entry.auth == nil || entry.auth.ID != pinnedAuthID {
				return false
			}
			if schedulerStrategyUsesWeights(s.strategy) && (entry.meta == nil || entry.meta.weight <= 0) {
				traceExclusion(entry, "weight_non_positive")
				return false
			}
			if requestPredicate != nil && !requestPredicate(entry.auth) {
				traceExclusion(entry, "request_filter")
				return false
			}
			if len(tried) == 0 {
				return true
			}
			_, ok := tried[pinnedAuthID]
			if ok {
				traceExclusion(entry, "already_tried")
			}
			return !ok
		}
		if s.strategy == schedulerStrategyQuotaAware {
			priority, okPriority := shard.highestReadyPriorityLocked(false, predicate)
			if okPriority {
				entries := shard.readyEntriesAtPriorityLocked(false, priority, predicate)
				signals, allFresh := s.quotaSignalsLocked(entries, model, time.Now())
				decision.mergeQuotaSignals(entries, signals)
				view := shard.readyViewAtPriorityLocked(false, priority, predicate)
				if view != nil {
					pickedEntry, quotaRules := s.pickQuotaAwareLocked(
						entries, predicate, &view.quotaState, signals, allFresh,
					)
					if pickedEntry != nil && pickedEntry.auth != nil {
						picked := pickedEntry.auth
						s.captureQuotaInFlightLeaseLocked(ctx, picked, providerKey, time.Now())
						s.markCheckpointDirtyLocked()
						matchedRules := shard.routeDecisionSelectionRulesLocked(
							picked, false, s.strategy, false,
						)
						matchedRules = append(matchedRules, quotaRules...)
						appendSchedulerSelection(
							&traceEvents, opts, decision, picked, providerKey, model, s.strategy, matchedRules...,
						)
						return picked, providerKey, nil
					}
				}
			}
		} else if picked := shard.pickReadyLocked(false, s.strategy, predicate); picked != nil {
			s.markCheckpointDirtyLocked()
			appendSchedulerSelection(
				&traceEvents,
				opts,
				decision,
				picked,
				providerKey,
				model,
				s.strategy,
				shard.routeDecisionSelectionRulesLocked(picked, false, s.strategy, false)...,
			)
			return picked, providerKey, nil
		}
		return nil, "", shard.unavailableErrorLocked("mixed", model, predicate)
	}

	tracedExclusions := make(map[string]struct{})
	traceExclusion := func(entry *scheduledAuth, reason string) {
		if routeTraceCallback(opts.Metadata) == nil || entry == nil || entry.auth == nil {
			return
		}
		decision.exclude(entry.auth, model, reason)
		key := entry.auth.ID + "\x00" + reason
		if _, exists := tracedExclusions[key]; exists {
			return
		}
		tracedExclusions[key] = struct{}{}
		providerKey := ""
		if entry.meta != nil {
			providerKey = entry.meta.providerKey
		}
		appendAuthRouteTraceEvent(
			&traceEvents, opts, entry.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: providerKey,
				Model:    model,
				Action:   "candidate.excluded",
				Reason:   reason,
			},
		)
	}
	predicate := func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if schedulerStrategyUsesWeights(s.strategy) && (entry.meta == nil || entry.meta.weight <= 0) {
			traceExclusion(entry, "weight_non_positive")
			return false
		}
		if requestPredicate != nil && !requestPredicate(entry.auth) {
			traceExclusion(entry, "request_filter")
			return false
		}
		if len(tried) == 0 {
			return true
		}
		_, ok := tried[entry.auth.ID]
		if ok {
			traceExclusion(entry, "already_tried")
		}
		return !ok
	}
	candidateShards := make([]*modelScheduler, len(normalized))
	preferWebsocketByProvider := make([]bool, len(normalized))
	bestPriority := 0
	hasCandidate := false
	now := time.Now()
	for providerIndex, providerKey := range normalized {
		providerState := s.providers[providerKey]
		if providerState == nil {
			attempt, previousAttempt := routeTraceAttempt(opts)
			appendRouteTraceEvent(
				&traceEvents, opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attempt,
					PreviousAttempt: previousAttempt,
					Provider:        providerKey,
					Model:           model,
					Action:          "provider.excluded",
					Reason:          "provider_not_registered",
				},
			)
			continue
		}
		shard := providerState.ensureModelLocked(modelKey, now)
		candidateShards[providerIndex] = shard
		preferWebsocket := cliproxyexecutor.DownstreamWebsocket(ctx) && providerPrefersWebsocketTransport(providerKey)
		preferWebsocketByProvider[providerIndex] = preferWebsocket
		if shard == nil {
			continue
		}
		shard.promoteExpiredLocked(now)
		decision.mergeShard(shard, model)
		appendScheduledStateExclusions(&traceEvents, opts, shard, providerKey, model)
		priorityReady, okPriority := shard.highestReadyPriorityLocked(preferWebsocket, predicate)
		if !okPriority {
			continue
		}
		if !hasCandidate || priorityReady > bestPriority {
			bestPriority = priorityReady
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	if s.strategy == schedulerStrategyFillFirst {
		for providerIndex, providerKey := range normalized {
			shard := candidateShards[providerIndex]
			if shard == nil {
				continue
			}
			picked := shard.pickReadyAtPriorityLocked(
				preferWebsocketByProvider[providerIndex], bestPriority, s.strategy, predicate,
			)
			if picked != nil {
				appendSchedulerSelection(
					&traceEvents,
					opts,
					decision,
					picked,
					providerKey,
					model,
					s.strategy,
					shard.routeDecisionSelectionRulesLocked(
						picked, preferWebsocketByProvider[providerIndex], s.strategy, false,
					)...,
				)
				return picked, providerKey, nil
			}
		}
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	cursorKey := mixedSchedulerCursorKey(normalized, modelKey)
	if s.strategy == schedulerStrategyQuotaAware {
		entries := make([]*scheduledAuth, 0)
		for providerIndex, shard := range candidateShards {
			if shard == nil {
				continue
			}
			entries = append(
				entries, shard.readyEntriesAtPriorityLocked(
					preferWebsocketByProvider[providerIndex], bestPriority, predicate,
				)...,
			)
		}
		sort.Slice(
			entries, func(i, j int) bool {
				if entries[i] == nil || entries[i].auth == nil {
					return false
				}
				if entries[j] == nil || entries[j].auth == nil {
					return true
				}
				return entries[i].auth.ID < entries[j].auth.ID
			},
		)
		signals, allFresh := s.quotaSignalsLocked(entries, model, now)
		decision.mergeQuotaSignals(entries, signals)
		if s.mixedQuotaStates == nil {
			s.mixedQuotaStates = make(map[string]*quotaAwareViewState)
		}
		state := s.mixedQuotaStates[cursorKey]
		if state == nil {
			state = &quotaAwareViewState{}
			s.mixedQuotaStates[cursorKey] = state
		}
		picked, quotaRules := s.pickQuotaAwareLocked(entries, predicate, state, signals, allFresh)
		if picked != nil && picked.auth != nil && picked.meta != nil {
			providerIndex := providerKeyIndex(normalized, picked.meta.providerKey)
			var matchedRules []string
			if providerIndex >= 0 && candidateShards[providerIndex] != nil {
				matchedRules = candidateShards[providerIndex].routeDecisionSelectionRulesLocked(
					picked.auth,
					preferWebsocketByProvider[providerIndex],
					s.strategy,
					false,
				)
			}
			matchedRules = append(matchedRules, quotaRules...)
			s.captureQuotaInFlightLeaseLocked(ctx, picked.auth, picked.meta.providerKey, now)
			s.markCheckpointDirtyLocked()
			appendSchedulerSelection(
				&traceEvents,
				opts,
				decision,
				picked.auth,
				picked.meta.providerKey,
				model,
				s.strategy,
				matchedRules...,
			)
			return picked.auth, picked.meta.providerKey, nil
		}
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}
	if s.strategy == schedulerStrategyWeightedRoundRobin {
		entries := make([]*scheduledAuth, 0)
		for providerIndex, shard := range candidateShards {
			if shard == nil {
				continue
			}
			entries = append(
				entries,
				shard.readyEntriesAtPriorityLocked(
					preferWebsocketByProvider[providerIndex], bestPriority, predicate,
				)...,
			)
		}
		sort.Slice(
			entries, func(i, j int) bool {
				if entries[i] == nil || entries[i].auth == nil {
					return false
				}
				if entries[j] == nil || entries[j].auth == nil {
					return true
				}
				return entries[i].auth.ID < entries[j].auth.ID
			},
		)
		if s.mixedWeightedStates == nil {
			s.mixedWeightedStates = make(map[string]*smoothWeightedState)
		}
		state := s.mixedWeightedStates[cursorKey]
		if state == nil {
			state = &smoothWeightedState{}
			s.mixedWeightedStates[cursorKey] = state
		}
		state.prepare(scheduledWeightVectorMatching(entries, predicate))
		picked := pickSmoothWeightedScheduled(entries, state, predicate)
		if picked != nil && picked.auth != nil && picked.meta != nil {
			providerIndex := providerKeyIndex(normalized, picked.meta.providerKey)
			var matchedRules []string
			if providerIndex >= 0 && candidateShards[providerIndex] != nil {
				matchedRules = candidateShards[providerIndex].routeDecisionSelectionRulesLocked(
					picked.auth,
					preferWebsocketByProvider[providerIndex],
					s.strategy,
					false,
				)
			}
			s.markCheckpointDirtyLocked()
			appendSchedulerSelection(
				&traceEvents,
				opts,
				decision,
				picked.auth,
				picked.meta.providerKey,
				model,
				s.strategy,
				matchedRules...,
			)
			return picked.auth, picked.meta.providerKey, nil
		}
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	entries := make([]*scheduledAuth, 0)
	for providerIndex, shard := range candidateShards {
		if shard == nil {
			continue
		}
		entries = append(
			entries, shard.readyEntriesAtPriorityLocked(
				preferWebsocketByProvider[providerIndex], bestPriority, predicate,
			)...,
		)
	}
	picked := pickStableMixedScheduled(entries, s.mixedCursors[cursorKey])
	if picked == nil || picked.auth == nil || picked.meta == nil {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}
	providerKey := picked.meta.providerKey
	providerIndex := providerKeyIndex(normalized, providerKey)
	if providerIndex < 0 || candidateShards[providerIndex] == nil {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}
	selectedID := picked.auth.ID
	selected := candidateShards[providerIndex].pickReadyAtPriorityLocked(
		preferWebsocketByProvider[providerIndex],
		bestPriority,
		schedulerStrategyRoundRobin,
		func(entry *scheduledAuth) bool {
			return entry != nil && entry.auth != nil && entry.auth.ID == selectedID
		},
	)
	if selected == nil {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}
	s.mixedCursors[cursorKey] = selected.ID
	s.markCheckpointDirtyLocked()
	appendSchedulerSelection(
		&traceEvents,
		opts,
		decision,
		selected,
		providerKey,
		model,
		s.strategy,
		candidateShards[providerIndex].routeDecisionSelectionRulesLocked(
			selected, preferWebsocketByProvider[providerIndex], s.strategy, false,
		)...,
	)
	return selected, providerKey, nil
}

// mixedUnavailableErrorLocked synthesizes the mixed-provider cooldown or unavailable error.
func (s *authScheduler) mixedUnavailableErrorLocked(
	providers []string,
	model string,
	predicate func(*scheduledAuth) bool,
) error {
	now := time.Now()
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	var (
		latestModelTime   time.Time
		latestModelAuthID string
		latestModelErr    error
		latestAuthTime    time.Time
		latestAuthID      string
		latestAuthErr     error
	)
	for _, providerKey := range providers {
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		shard := providerState.ensureModelLocked(canonicalModelKey(model), now)
		if shard == nil {
			continue
		}
		localTotal, localCooldownCount, localEarliest := shard.availabilitySummaryLocked(predicate)
		total += localTotal
		cooldownCount += localCooldownCount
		if !localEarliest.IsZero() && (earliest.IsZero() || localEarliest.Before(earliest)) {
			earliest = localEarliest
		}
		modelCandidate, authCandidate := shard.candidateErrorsLocked(model, predicate)
		if modelCandidate.err != nil &&
			(latestModelErr == nil || modelCandidate.observedAt.After(latestModelTime) ||
				modelCandidate.observedAt.Equal(latestModelTime) && modelCandidate.authID > latestModelAuthID) {
			latestModelTime = modelCandidate.observedAt
			latestModelAuthID = modelCandidate.authID
			latestModelErr = modelCandidate.err
		}
		if authCandidate.err != nil &&
			(latestAuthErr == nil || authCandidate.observedAt.After(latestAuthTime) ||
				authCandidate.observedAt.Equal(latestAuthTime) && authCandidate.authID > latestAuthID) {
			latestAuthTime = authCandidate.observedAt
			latestAuthID = authCandidate.authID
			latestAuthErr = authCandidate.err
		}
	}
	lastCandidateErr := latestModelErr
	if lastCandidateErr == nil {
		lastCandidateErr = latestAuthErr
	}
	if total == 0 {
		return WithCause(&Error{Code: "auth_not_found", Message: "no auth available"}, lastCandidateErr)
	}
	if cooldownCount == total && !earliest.IsZero() {
		resetIn := max(earliest.Sub(now), 0)
		return newModelCooldownErrorWithCause(model, "", resetIn, lastCandidateErr)
	}
	return WithCause(&Error{Code: "auth_unavailable", Message: "no auth available"}, lastCandidateErr)
}

// normalizeProviderKeys lowercases, trims, and de-duplicates provider keys while preserving order.
func normalizeProviderKeys(providers []string) []string {
	seen := make(map[string]struct{}, len(providers))
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" {
			continue
		}
		if _, ok := seen[providerKey]; ok {
			continue
		}
		seen[providerKey] = struct{}{}
		out = append(out, providerKey)
	}
	return out
}

// containsProvider reports whether provider is present in the normalized provider list.
func containsProvider(providers []string, provider string) bool {
	return providerKeyIndex(providers, provider) >= 0
}

func providerKeyIndex(providers []string, provider string) int {
	for index, candidate := range providers {
		if candidate == provider {
			return index
		}
	}
	return -1
}

func mixedSchedulerCursorKey(providers []string, model string) string {
	providerSet := append([]string(nil), providers...)
	sort.Strings(providerSet)
	return strings.Join(providerSet, ",") + ":" + model
}

func pickStableMixedScheduled(entries []*scheduledAuth, lastPicked string) *scheduledAuth {
	var initial *scheduledAuth
	var first *scheduledAuth
	var successor *scheduledAuth
	for _, entry := range entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		if initial == nil {
			initial = entry
		}
		if first == nil || entry.auth.ID < first.auth.ID {
			first = entry
		}
		if entry.auth.ID <= lastPicked {
			continue
		}
		if successor == nil || entry.auth.ID < successor.auth.ID {
			successor = entry
		}
	}
	if lastPicked == "" {
		return initial
	}
	if successor != nil {
		return successor
	}
	return first
}

func compareScheduledAuthVersion(left, right scheduledAuthVersion) int {
	if left.generation < right.generation {
		return -1
	}
	if left.generation > right.generation {
		return 1
	}
	if left.updatedAt.Before(right.updatedAt) {
		return -1
	}
	if left.updatedAt.After(right.updatedAt) {
		return 1
	}
	if left.tombstone == right.tombstone {
		return 0
	}
	if left.tombstone {
		return 1
	}
	return -1
}

func scheduledVersionForAuth(auth *Auth) scheduledAuthVersion {
	if auth == nil {
		return scheduledAuthVersion{}
	}
	return scheduledAuthVersion{
		generation: auth.Generation,
		updatedAt:  auth.UpdatedAt,
	}
}

func (s *authScheduler) acceptsAuthVersionLocked(authID string, incoming scheduledAuthVersion) bool {
	if s.authVersions == nil {
		s.authVersions = make(map[string]scheduledAuthVersion)
	}
	existing, exists := s.authVersions[authID]
	return !exists || compareScheduledAuthVersion(incoming, existing) >= 0
}

// upsertAuthLocked synchronizes one lifecycle snapshot and every existing model shard.
func (s *authScheduler) upsertAuthLocked(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	version := scheduledVersionForAuth(auth)
	if !s.acceptsAuthVersionLocked(authID, version) {
		s.refreshAuthModelRegistrationLocked(authID, now)
		return
	}
	s.authVersions[authID] = version

	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" || auth.Disabled {
		s.removeAuthFromProvidersLocked(authID)
		return
	}
	if previousProvider := s.authProviders[authID]; previousProvider != "" && previousProvider != providerKey {
		if previousState := s.providers[previousProvider]; previousState != nil {
			previousState.removeAuthLocked(authID)
		}
	}
	modelSet, registryEpoch := registeredModelSetForAuth(authID)
	meta := buildScheduledAuthMetaWithModelSet(auth, modelSet, registryEpoch)
	s.authProviders[authID] = providerKey
	s.ensureProviderLocked(providerKey).upsertAuthForModelsLocked(meta, nil, true, now)
}

func (s *authScheduler) upsertAuthResultLocked(
	auth *Auth,
	targetModels []string,
	fullClient bool,
	now time.Time,
) {
	if auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	version := scheduledVersionForAuth(auth)
	if !s.acceptsAuthVersionLocked(authID, version) {
		if s.refreshAuthModelRegistrationLocked(authID, now) {
			return
		}
		providerState, latestMeta := s.scheduledAuthMetaLocked(authID)
		if providerState != nil && latestMeta != nil {
			providerState.upsertAuthForModelsLocked(latestMeta, targetModels, fullClient, now)
		}
		return
	}
	s.authVersions[authID] = version

	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" || auth.Disabled {
		s.removeAuthFromProvidersLocked(authID)
		return
	}
	if previousProvider := s.authProviders[authID]; previousProvider != "" && previousProvider != providerKey {
		if previousState := s.providers[previousProvider]; previousState != nil {
			previousState.removeAuthLocked(authID)
		}
	}

	providerState := s.ensureProviderLocked(providerKey)
	existingMeta := providerState.auths[authID]
	registryEpoch := registry.GetGlobalRegistry().ClientRegistrationEpoch(authID)
	modelSetChanged := existingMeta == nil || existingMeta.registryEpoch != registryEpoch
	var modelSet map[string]struct{}
	if modelSetChanged {
		modelSet, registryEpoch = registeredModelSetForAuth(authID)
	} else {
		modelSet = existingMeta.supportedModelSet
	}
	meta := buildScheduledAuthMetaWithModelSet(auth, modelSet, registryEpoch)
	s.authProviders[authID] = providerKey

	if fullClient || len(targetModels) == 0 || modelSetChanged ||
		scheduledAuthMetadataChanged(existingMeta, meta) {
		providerState.upsertAuthForModelsLocked(meta, nil, true, now)
		return
	}
	providerState.upsertAuthForModelsLocked(meta, targetModels, false, now)
}

func (s *authScheduler) scheduledAuthMetaLocked(authID string) (*providerScheduler, *scheduledAuthMeta) {
	providerKey := s.authProviders[authID]
	if providerKey == "" {
		return nil, nil
	}
	providerState := s.providers[providerKey]
	if providerState == nil {
		return nil, nil
	}
	return providerState, providerState.auths[authID]
}

func (s *authScheduler) refreshAuthModelRegistrationLocked(authID string, now time.Time) bool {
	providerState, existingMeta := s.scheduledAuthMetaLocked(authID)
	if providerState == nil || existingMeta == nil || existingMeta.auth == nil {
		return false
	}
	registryEpoch := registry.GetGlobalRegistry().ClientRegistrationEpoch(authID)
	if existingMeta.registryEpoch == registryEpoch {
		return false
	}
	modelSet, registryEpoch := registeredModelSetForAuth(authID)
	meta := buildScheduledAuthMetaWithModelSet(existingMeta.auth, modelSet, registryEpoch)
	providerState.upsertAuthForModelsLocked(meta, nil, true, now)
	return true
}

func scheduledAuthMetadataChanged(previous, current *scheduledAuthMeta) bool {
	if previous == nil || current == nil {
		return true
	}
	return previous.providerKey != current.providerKey ||
		previous.priority != current.priority ||
		previous.weight != current.weight ||
		previous.virtualParent != current.virtualParent ||
		previous.websocketEnabled != current.websocketEnabled ||
		(previous.auth.Status == StatusDisabled) != (current.auth.Status == StatusDisabled)
}

func (s *authScheduler) recordRemovalTombstoneLocked(authID string, tombstone scheduledAuthVersion) bool {
	if authID == "" {
		return false
	}
	if s.authVersions == nil {
		s.authVersions = make(map[string]scheduledAuthVersion)
	}
	if existing, exists := s.authVersions[authID]; exists {
		if existing.generation > tombstone.generation {
			return false
		}
		if existing.generation == tombstone.generation && existing.updatedAt.After(tombstone.updatedAt) {
			tombstone.updatedAt = existing.updatedAt
		}
	}
	tombstone.tombstone = true
	s.authVersions[authID] = tombstone
	s.removeAuthFromProvidersLocked(authID)
	return true
}

func (s *authScheduler) removeAuthFromProvidersLocked(authID string) {
	if providerKey := s.authProviders[authID]; providerKey != "" {
		if providerState := s.providers[providerKey]; providerState != nil {
			providerState.removeAuthLocked(authID)
		}
		delete(s.authProviders, authID)
	}
}

// ensureProviderLocked returns the provider scheduler for providerKey, creating it when needed.
func (s *authScheduler) ensureProviderLocked(providerKey string) *providerScheduler {
	if s.providers == nil {
		s.providers = make(map[string]*providerScheduler)
	}
	providerState := s.providers[providerKey]
	if providerState == nil {
		providerState = &providerScheduler{
			providerKey: providerKey,
			auths:       make(map[string]*scheduledAuthMeta),
			modelShards: make(map[string]*modelScheduler),
		}
		s.providers[providerKey] = providerState
	}
	return providerState
}

func buildScheduledAuthMetaWithModelSet(
	auth *Auth,
	modelSet map[string]struct{},
	registryEpoch uint64,
) *scheduledAuthMeta {
	if auth == nil {
		return nil
	}
	snapshot := auth.Clone()
	providerKey := strings.ToLower(strings.TrimSpace(snapshot.Provider))
	virtualParent := ""
	if snapshot.Attributes != nil {
		virtualParent = strings.TrimSpace(snapshot.Attributes["gemini_virtual_parent"])
	}
	return &scheduledAuthMeta{
		auth:              snapshot,
		providerKey:       providerKey,
		priority:          authPriority(snapshot),
		weight:            authWeight(snapshot),
		virtualParent:     virtualParent,
		websocketEnabled:  authWebsocketsEnabled(snapshot),
		supportedModelSet: modelSet,
		registryEpoch:     registryEpoch,
	}
}

func registeredModelSetForAuth(authID string) (map[string]struct{}, uint64) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, 0
	}
	models, registryEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID)
	if len(models) == 0 {
		return nil, registryEpoch
	}
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		set[modelKey] = struct{}{}
	}
	return set, registryEpoch
}

// supportedModelSetForAuth snapshots the registry models currently registered for an auth.
func supportedModelSetForAuth(authID string) map[string]struct{} {
	modelSet, _ := registeredModelSetForAuth(authID)
	return modelSet
}

func (p *providerScheduler) upsertAuthForModelsLocked(
	meta *scheduledAuthMeta,
	targetModels []string,
	fullClient bool,
	now time.Time,
) {
	if p == nil || meta == nil || meta.auth == nil {
		return
	}
	p.auths[meta.auth.ID] = meta
	if fullClient || len(targetModels) == 0 {
		for modelKey, shard := range p.modelShards {
			if shard == nil {
				continue
			}
			if !meta.supportsModel(modelKey) {
				shard.removeEntryLocked(meta.auth.ID)
				continue
			}
			shard.upsertEntryLocked(meta, now)
		}
		return
	}
	p.updateTargetShardsLocked(meta, targetModels, now)
}

func (p *providerScheduler) updateTargetShardsLocked(
	meta *scheduledAuthMeta,
	targetModels []string,
	now time.Time,
) {
	if p == nil || meta == nil || meta.auth == nil {
		return
	}
	if emptyShard := p.modelShards[""]; emptyShard != nil {
		emptyShard.upsertEntryLocked(meta, now)
	}
	seen := make(map[string]struct{}, len(targetModels))
	for _, model := range targetModels {
		modelKey := canonicalModelKey(model)
		if modelKey == "" {
			continue
		}
		if _, duplicate := seen[modelKey]; duplicate {
			continue
		}
		seen[modelKey] = struct{}{}
		shard := p.modelShards[modelKey]
		if shard == nil {
			continue
		}
		if !meta.supportsModel(modelKey) {
			shard.removeEntryLocked(meta.auth.ID)
			continue
		}
		shard.upsertEntryLocked(meta, now)
	}
}

// removeAuthLocked removes an auth from all model shards owned by the provider scheduler.
func (p *providerScheduler) removeAuthLocked(authID string) {
	if p == nil || authID == "" {
		return
	}
	delete(p.auths, authID)
	for _, shard := range p.modelShards {
		if shard != nil {
			shard.removeEntryLocked(authID)
		}
	}
}

// ensureModelLocked returns the shard for modelKey, building it lazily from provider auths.
func (p *providerScheduler) ensureModelLocked(modelKey string, now time.Time) *modelScheduler {
	if p == nil {
		return nil
	}
	modelKey = canonicalModelKey(modelKey)
	if shard, ok := p.modelShards[modelKey]; ok && shard != nil {
		shard.promoteExpiredLocked(now)
		return shard
	}
	shard := &modelScheduler{
		modelKey:               modelKey,
		entries:                make(map[string]*scheduledAuth),
		readyByPriority:        make(map[int]*readyBucket),
		cursorStatesByPriority: make(map[int]readyBucketCursorState),
	}
	for _, meta := range p.auths {
		if meta == nil || !meta.supportsModel(modelKey) {
			continue
		}
		shard.upsertEntryLocked(meta, now)
	}
	p.modelShards[modelKey] = shard
	return shard
}

// supportsModel reports whether the auth metadata currently supports modelKey.
func (m *scheduledAuthMeta) supportsModel(modelKey string) bool {
	modelKey = canonicalModelKey(modelKey)
	if modelKey == "" {
		return true
	}
	if len(m.supportedModelSet) == 0 {
		m.supportedModelSet = supportedModelSetForAuth(m.auth.ID)
	}
	if len(m.supportedModelSet) == 0 {
		return false
	}
	if _, ok := m.supportedModelSet[modelKey]; ok {
		return true
	}
	refreshed := supportedModelSetForAuth(m.auth.ID)
	if len(refreshed) == 0 {
		return false
	}
	m.supportedModelSet = refreshed
	_, ok := refreshed[modelKey]
	return ok
}

// upsertEntryLocked updates or inserts one auth entry and rebuilds indexes when ordering changes.
func (m *modelScheduler) upsertEntryLocked(meta *scheduledAuthMeta, now time.Time) {
	if m == nil || meta == nil || meta.auth == nil {
		return
	}
	entry, ok := m.entries[meta.auth.ID]
	if !ok || entry == nil {
		entry = &scheduledAuth{}
		m.entries[meta.auth.ID] = entry
	}
	previousState := entry.state
	previousNextRetryAt := entry.nextRetryAt
	previousPriority := 0
	previousWeight := int64(0)
	previousParent := ""
	previousWebsocketEnabled := false
	if entry.meta != nil {
		previousPriority = entry.meta.priority
		previousWeight = entry.meta.weight
		previousParent = entry.meta.virtualParent
		previousWebsocketEnabled = entry.meta.websocketEnabled
	}

	entry.meta = meta
	entry.auth = meta.auth
	entry.nextRetryAt = time.Time{}
	blocked, reason, next := isAuthBlockedForModel(meta.auth, m.modelKey, now)
	switch {
	case !blocked:
		entry.state = scheduledStateReady
	case reason == blockReasonCooldown:
		entry.state = scheduledStateCooldown
		entry.nextRetryAt = next
	case reason == blockReasonDisabled:
		entry.state = scheduledStateDisabled
	default:
		entry.state = scheduledStateBlocked
		entry.nextRetryAt = next
	}

	if ok && previousState == entry.state && previousNextRetryAt.Equal(entry.nextRetryAt) &&
		previousPriority == meta.priority && previousWeight == meta.weight && previousParent == meta.virtualParent &&
		previousWebsocketEnabled == meta.websocketEnabled {
		return
	}
	m.rebuildIndexesLocked()
}

// removeEntryLocked deletes one auth entry and rebuilds the shard indexes if needed.
func (m *modelScheduler) removeEntryLocked(authID string) {
	if m == nil || authID == "" {
		return
	}
	if _, ok := m.entries[authID]; !ok {
		return
	}
	delete(m.entries, authID)
	m.rebuildIndexesLocked()
}

// demoteExpiredTokensLocked moves ready credentials with expired access tokens out of the ready set.
func (m *modelScheduler) demoteExpiredTokensLocked(now time.Time) bool {
	if m == nil || len(m.entries) == 0 {
		return false
	}
	changed := false
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil || entry.state != scheduledStateReady {
			continue
		}
		expiresAt, ok := entry.auth.AccessTokenExpirationTime()
		if !ok || expiresAt.IsZero() || expiresAt.After(now) {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(entry.auth, m.modelKey, now)
		if !blocked {
			continue
		}
		switch {
		case reason == blockReasonCooldown:
			entry.state = scheduledStateCooldown
			entry.nextRetryAt = next
		case reason == blockReasonDisabled:
			entry.state = scheduledStateDisabled
			entry.nextRetryAt = time.Time{}
		default:
			entry.state = scheduledStateBlocked
			entry.nextRetryAt = next
		}
		changed = true
	}
	return changed
}

// promoteExpiredLocked reevaluates blocked auths and removes expired access tokens from ready views.
func (m *modelScheduler) promoteExpiredLocked(now time.Time) {
	if m == nil {
		return
	}
	changed := m.demoteExpiredTokensLocked(now)
	for _, entry := range m.blocked {
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.nextRetryAt.IsZero() || entry.nextRetryAt.After(now) {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(entry.auth, m.modelKey, now)
		switch {
		case !blocked:
			entry.state = scheduledStateReady
			entry.nextRetryAt = time.Time{}
		case reason == blockReasonCooldown:
			entry.state = scheduledStateCooldown
			entry.nextRetryAt = next
		case reason == blockReasonDisabled:
			entry.state = scheduledStateDisabled
			entry.nextRetryAt = time.Time{}
		default:
			entry.state = scheduledStateBlocked
			entry.nextRetryAt = next
		}
		changed = true
	}
	if changed {
		m.rebuildIndexesLocked()
	}
}

// pickReadyLocked selects the next ready auth from the highest available priority bucket.
func (m *modelScheduler) pickReadyLocked(
	preferWebsocket bool,
	strategy schedulerStrategy,
	predicate func(*scheduledAuth) bool,
) *Auth {
	if m == nil {
		return nil
	}
	m.promoteExpiredLocked(time.Now())
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, predicate)
	if !okPriority {
		return nil
	}
	return m.pickReadyAtPriorityLocked(preferWebsocket, priorityReady, strategy, predicate)
}

// highestReadyPriorityLocked returns the highest priority bucket that still has a matching ready auth.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) highestReadyPriorityLocked(preferWebsocket bool, predicate func(*scheduledAuth) bool) (
	int,
	bool,
) {
	if m == nil {
		return 0, false
	}
	if preferWebsocket {
		for _, priority := range m.priorityOrder {
			bucket := m.readyByPriority[priority]
			if bucket == nil {
				continue
			}
			if bucket.ws.pickFirst(predicate) != nil {
				return priority, true
			}
		}
	}
	for _, priority := range m.priorityOrder {
		bucket := m.readyByPriority[priority]
		if bucket == nil {
			continue
		}
		if bucket.all.pickFirst(predicate) != nil {
			return priority, true
		}
	}
	return 0, false
}

func (m *modelScheduler) readyViewAtPriorityLocked(
	preferWebsocket bool,
	priority int,
	predicate func(*scheduledAuth) bool,
) *readyView {
	if m == nil {
		return nil
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	if preferWebsocket && bucket.ws.pickFirst(predicate) != nil {
		view = &bucket.ws
	}
	return view
}

// pickReadyAtPriorityLocked selects the next ready auth from a specific priority bucket.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) pickReadyAtPriorityLocked(
	preferWebsocket bool,
	priority int,
	strategy schedulerStrategy,
	predicate func(*scheduledAuth) bool,
) *Auth {
	view := m.readyViewAtPriorityLocked(preferWebsocket, priority, predicate)
	if view == nil {
		return nil
	}
	var picked *scheduledAuth
	switch strategy {
	case schedulerStrategyFillFirst:
		picked = view.pickFirst(predicate)
	case schedulerStrategyWeightedRoundRobin:
		picked = view.pickWeighted(predicate)
	default:
		picked = view.pickRoundRobin(predicate)
	}
	if picked == nil || picked.auth == nil {
		return nil
	}
	return picked.auth
}

func (m *modelScheduler) routeDecisionSelectionRulesLocked(
	auth *Auth,
	preferWebsocket bool,
	strategy schedulerStrategy,
	groupedRoundRobin bool,
) []string {
	if m == nil || auth == nil {
		return nil
	}
	entry := m.entries[auth.ID]
	if entry == nil || entry.meta == nil {
		return nil
	}
	bucket := m.readyByPriority[entry.meta.priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	var rules []string
	if preferWebsocket && entry.meta.websocketEnabled {
		rules = append(rules, "transport_preference")
		view = &bucket.ws
	}
	if groupedRoundRobin && strategy == schedulerStrategyRoundRobin &&
		len(view.parentOrder) > 1 && len(view.children) > 0 {
		rules = append(rules, "round_robin_group")
	}
	return rules
}

func (m *modelScheduler) readyEntriesAtPriorityLocked(
	preferWebsocket bool,
	priority int,
	predicate func(*scheduledAuth) bool,
) []*scheduledAuth {
	if m == nil {
		return nil
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := bucket.all
	if preferWebsocket && bucket.ws.pickFirst(predicate) != nil {
		view = bucket.ws
	}
	entries := make([]*scheduledAuth, 0, len(view.flat))
	for _, entry := range view.flat {
		if predicate == nil || predicate(entry) {
			entries = append(entries, entry)
		}
	}
	return entries
}

// unavailableErrorLocked returns the correct unavailable or cooldown error for the shard.
func (m *modelScheduler) unavailableErrorLocked(provider, model string, predicate func(*scheduledAuth) bool) error {
	now := time.Now()
	total, cooldownCount, earliest := m.availabilitySummaryLocked(predicate)
	lastCandidateErr := m.latestCandidateErrorWithTimeLocked(model, predicate).err
	if total == 0 {
		return WithCause(&Error{Code: "auth_not_found", Message: "no auth available"}, lastCandidateErr)
	}
	if cooldownCount == total && !earliest.IsZero() {
		providerForError := provider
		if providerForError == "mixed" {
			providerForError = ""
		}
		resetIn := max(earliest.Sub(now), 0)
		return newModelCooldownErrorWithCause(model, providerForError, resetIn, lastCandidateErr)
	}
	return WithCause(&Error{Code: "auth_unavailable", Message: "no auth available"}, lastCandidateErr)
}

type candidateErrorSnapshot struct {
	err        error
	observedAt time.Time
	authID     string
}

func (m *modelScheduler) latestCandidateErrorWithTimeLocked(
	model string,
	predicate func(*scheduledAuth) bool,
) candidateErrorSnapshot {
	modelCandidate, authCandidate := m.candidateErrorsLocked(model, predicate)
	if modelCandidate.err != nil {
		return modelCandidate
	}
	return authCandidate
}

func (m *modelScheduler) candidateErrorsLocked(
	model string,
	predicate func(*scheduledAuth) bool,
) (candidateErrorSnapshot, candidateErrorSnapshot) {
	if m == nil {
		return candidateErrorSnapshot{}, candidateErrorSnapshot{}
	}
	var modelCandidate candidateErrorSnapshot
	var authCandidate candidateErrorSnapshot
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil || predicate != nil && !predicate(entry) {
			continue
		}
		candidateModel := modelCandidateError(entry.auth, model)
		if candidateModel.err != nil &&
			(modelCandidate.err == nil || candidateModel.observedAt.After(modelCandidate.observedAt) ||
				candidateModel.observedAt.Equal(modelCandidate.observedAt) && candidateModel.authID > modelCandidate.authID) {
			modelCandidate = candidateModel
		}
		candidateAuth := credentialCandidateError(entry.auth)
		if candidateAuth.err != nil &&
			(authCandidate.err == nil || candidateAuth.observedAt.After(authCandidate.observedAt) ||
				candidateAuth.observedAt.Equal(authCandidate.observedAt) && candidateAuth.authID > authCandidate.authID) {
			authCandidate = candidateAuth
		}
	}
	return modelCandidate, authCandidate
}

func modelCandidateError(auth *Auth, model string) candidateErrorSnapshot {
	if auth == nil {
		return candidateErrorSnapshot{}
	}
	if state, _ := findModelState(auth, model); state != nil {
		if state.LastError != nil {
			return candidateErrorSnapshot{
				err:        state.LastError,
				observedAt: candidateErrorTime(state.UpdatedAt, auth.UpdatedAt),
				authID:     auth.ID,
			}
		}
		if message := strings.TrimSpace(state.StatusMessage); message != "" {
			return candidateErrorSnapshot{
				err:        errors.New(message),
				observedAt: candidateErrorTime(state.UpdatedAt, auth.UpdatedAt),
				authID:     auth.ID,
			}
		}
	}
	family := quotaFamilyKey(auth, model)
	if family == "" {
		return candidateErrorSnapshot{}
	}
	var (
		latestErr  error
		latestTime time.Time
		latestKey  string
	)
	for stateModel, state := range auth.ModelStates {
		if state == nil || quotaFamilyKeyForState(auth, stateModel, state) != family {
			continue
		}
		candidateTime := candidateErrorTime(state.UpdatedAt, auth.UpdatedAt)
		var candidateErr error
		if state.LastError != nil {
			candidateErr = state.LastError
		} else if message := strings.TrimSpace(state.StatusMessage); message != "" {
			candidateErr = errors.New(message)
		}
		if candidateErr != nil &&
			(latestErr == nil || candidateTime.After(latestTime) ||
				candidateTime.Equal(latestTime) && stateModel > latestKey) {
			latestErr = candidateErr
			latestTime = candidateTime
			latestKey = stateModel
		}
	}
	return candidateErrorSnapshot{err: latestErr, observedAt: latestTime, authID: auth.ID}
}

func credentialCandidateError(auth *Auth) candidateErrorSnapshot {
	if auth == nil {
		return candidateErrorSnapshot{}
	}
	if auth.LastError != nil {
		return candidateErrorSnapshot{err: auth.LastError, observedAt: auth.UpdatedAt, authID: auth.ID}
	}
	if message := strings.TrimSpace(auth.StatusMessage); message != "" {
		return candidateErrorSnapshot{err: errors.New(message), observedAt: auth.UpdatedAt, authID: auth.ID}
	}
	return candidateErrorSnapshot{}
}

func candidateErrorTime(primary, fallback time.Time) time.Time {
	if primary.IsZero() {
		return fallback
	}
	return primary
}

// availabilitySummaryLocked summarizes total candidates, cooldown count, and earliest retry time.
func (m *modelScheduler) availabilitySummaryLocked(predicate func(*scheduledAuth) bool) (int, int, time.Time) {
	if m == nil {
		return 0, 0, time.Time{}
	}
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, entry := range m.entries {
		if predicate != nil && !predicate(entry) {
			continue
		}
		total++
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.state != scheduledStateCooldown {
			continue
		}
		cooldownCount++
		if !entry.nextRetryAt.IsZero() && (earliest.IsZero() || entry.nextRetryAt.Before(earliest)) {
			earliest = entry.nextRetryAt
		}
	}
	return total, cooldownCount, earliest
}

// rebuildIndexesLocked reconstructs ready and blocked views from the current entry map.
func (m *modelScheduler) rebuildIndexesLocked() {
	cursorStates := m.snapshotCursorStatesLocked()
	knownPriorities := make(map[int]struct{})

	m.readyByPriority = make(map[int]*readyBucket)
	m.priorityOrder = m.priorityOrder[:0]
	m.blocked = m.blocked[:0]
	priorityBuckets := make(map[int][]*scheduledAuth)
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil || entry.meta == nil {
			continue
		}
		priority := entry.meta.priority
		knownPriorities[priority] = struct{}{}
		// noinspection GoSwitchMissingCasesForIotaConsts — scheduledStateDisabled entries are skipped (nil guard above).
		switch entry.state {
		case scheduledStateReady:
			priorityBuckets[priority] = append(priorityBuckets[priority], entry)
		case scheduledStateCooldown, scheduledStateBlocked:
			m.blocked = append(m.blocked, entry)
		}
	}
	m.cursorStatesByPriority = make(map[int]readyBucketCursorState, len(knownPriorities))
	for priority := range knownPriorities {
		if state, ok := cursorStates[priority]; ok {
			m.cursorStatesByPriority[priority] = state
		}
	}
	for priority, entries := range priorityBuckets {
		sort.Slice(
			entries, func(i, j int) bool {
				return entries[i].auth.ID < entries[j].auth.ID
			},
		)
		bucket := buildReadyBucket(entries)
		if cursorState, ok := cursorStates[priority]; ok && bucket != nil {
			restoreReadyViewCursors(&bucket.all, cursorState.all)
			restoreReadyViewCursors(&bucket.ws, cursorState.ws)
		}
		m.readyByPriority[priority] = bucket
		m.priorityOrder = append(m.priorityOrder, priority)
	}
	sort.Slice(
		m.priorityOrder, func(i, j int) bool {
			return m.priorityOrder[i] > m.priorityOrder[j]
		},
	)
	sort.Slice(
		m.blocked, func(i, j int) bool {
			left := m.blocked[i]
			right := m.blocked[j]
			if left == nil || right == nil {
				return left != nil
			}
			if left.nextRetryAt.Equal(right.nextRetryAt) {
				return left.auth.ID < right.auth.ID
			}
			if left.nextRetryAt.IsZero() {
				return false
			}
			if right.nextRetryAt.IsZero() {
				return true
			}
			return left.nextRetryAt.Before(right.nextRetryAt)
		},
	)
}

// buildReadyBucket prepares the general and websocket-only ready views for one priority bucket.
func buildReadyBucket(entries []*scheduledAuth) *readyBucket {
	bucket := &readyBucket{}
	bucket.all = buildReadyView(entries)
	wsEntries := make([]*scheduledAuth, 0, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.meta != nil && entry.meta.websocketEnabled {
			wsEntries = append(wsEntries, entry)
		}
	}
	bucket.ws = buildReadyView(wsEntries)
	return bucket
}

// buildReadyView creates either a flat view or a grouped parent/child view for rotation.
func buildReadyView(entries []*scheduledAuth) readyView {
	view := readyView{flat: append([]*scheduledAuth(nil), entries...)}
	if len(entries) == 0 {
		return view
	}
	groups := make(map[string][]*scheduledAuth)
	for _, entry := range entries {
		if entry == nil || entry.meta == nil || entry.meta.virtualParent == "" {
			return view
		}
		groups[entry.meta.virtualParent] = append(groups[entry.meta.virtualParent], entry)
	}
	if len(groups) <= 1 {
		return view
	}
	view.children = make(map[string]*childBucket, len(groups))
	view.parentOrder = make([]string, 0, len(groups))
	for parent := range groups {
		view.parentOrder = append(view.parentOrder, parent)
	}
	sort.Strings(view.parentOrder)
	for _, parent := range view.parentOrder {
		view.children[parent] = &childBucket{items: append([]*scheduledAuth(nil), groups[parent]...)}
	}
	return view
}

// pickFirst returns the first ready entry that satisfies predicate without advancing cursors.
func (v *readyView) pickFirst(predicate func(*scheduledAuth) bool) *scheduledAuth {
	for _, entry := range v.flat {
		if predicate == nil || predicate(entry) {
			return entry
		}
	}
	return nil
}

// pickRoundRobin returns the next ready entry using flat or grouped round-robin traversal.
func (v *readyView) pickRoundRobin(predicate func(*scheduledAuth) bool) *scheduledAuth {
	if len(v.parentOrder) > 1 && len(v.children) > 0 {
		return v.pickGroupedRoundRobin(predicate)
	}
	if len(v.flat) == 0 {
		return nil
	}
	start := nextSortedScheduledAuthIndex(v.flat, v.lastPickedAuth)
	for offset := 0; offset < len(v.flat); offset++ {
		entry := v.flat[(start+offset)%len(v.flat)]
		if entry == nil || entry.auth == nil || predicate != nil && !predicate(entry) {
			continue
		}
		v.lastPickedAuth = entry.auth.ID
		return entry
	}
	return nil
}

func nextSortedScheduledAuthIndex(entries []*scheduledAuth, lastPicked string) int {
	if len(entries) == 0 || lastPicked == "" {
		return 0
	}
	index := sort.Search(
		len(entries), func(index int) bool {
			entry := entries[index]
			return entry != nil && entry.auth != nil && entry.auth.ID >= lastPicked
		},
	)
	if index < len(entries) && entries[index] != nil && entries[index].auth != nil &&
		entries[index].auth.ID == lastPicked {
		return (index + 1) % len(entries)
	}
	if index < len(entries) {
		return index
	}
	return 0
}

// pickWeighted returns the next ready entry using smooth weighted round-robin.
func (v *readyView) pickWeighted(predicate func(*scheduledAuth) bool) *scheduledAuth {
	if v == nil || len(v.flat) == 0 {
		return nil
	}
	v.weightedState.prepare(scheduledWeightVectorMatching(v.flat, predicate))
	return pickSmoothWeightedScheduled(v.flat, &v.weightedState, predicate)
}

func scheduledWeightVector(entries []*scheduledAuth) map[string]int64 {
	return scheduledWeightVectorMatching(entries, nil)
}

func scheduledWeightVectorMatching(
	entries []*scheduledAuth,
	predicate func(*scheduledAuth) bool,
) map[string]int64 {
	weights := make(map[string]int64, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || entry.meta == nil || entry.meta.weight <= 0 {
			continue
		}
		if predicate != nil && !predicate(entry) {
			continue
		}
		weights[entry.auth.ID] = entry.meta.weight
	}
	return weights
}

func pickSmoothWeightedScheduled(
	entries []*scheduledAuth,
	state *smoothWeightedState,
	predicate func(*scheduledAuth) bool,
) *scheduledAuth {
	if state == nil {
		return nil
	}
	start := nextSortedScheduledAuthIndex(entries, state.lastPicked)
	var picked *scheduledAuth
	var pickedCurrent int64
	var totalWeight int64
	for offset := range entries {
		entry := entries[(start+offset)%len(entries)]
		if entry == nil || entry.auth == nil || entry.meta == nil || entry.meta.weight <= 0 {
			continue
		}
		if predicate != nil && !predicate(entry) {
			continue
		}
		state.current[entry.auth.ID] = saturatingAddInt64(state.current[entry.auth.ID], entry.meta.weight)
		totalWeight = saturatingAddInt64(totalWeight, entry.meta.weight)
		if picked == nil || state.current[entry.auth.ID] > pickedCurrent {
			picked = entry
			pickedCurrent = state.current[entry.auth.ID]
		}
	}
	if picked == nil {
		return nil
	}
	state.current[picked.auth.ID] = saturatingAddInt64(state.current[picked.auth.ID], -totalWeight)
	state.lastPicked = picked.auth.ID
	return picked
}

// pickGroupedRoundRobin rotates across parents first and then within the selected parent.
func (v *readyView) pickGroupedRoundRobin(predicate func(*scheduledAuth) bool) *scheduledAuth {
	start := nextSortedStringIndex(v.parentOrder, v.lastPickedParent)
	for offset := 0; offset < len(v.parentOrder); offset++ {
		parent := v.parentOrder[(start+offset)%len(v.parentOrder)]
		child := v.children[parent]
		if child == nil || len(child.items) == 0 {
			continue
		}
		itemStart := nextSortedScheduledAuthIndex(child.items, child.lastPickedAuth)
		for itemOffset := 0; itemOffset < len(child.items); itemOffset++ {
			entry := child.items[(itemStart+itemOffset)%len(child.items)]
			if entry == nil || entry.auth == nil || predicate != nil && !predicate(entry) {
				continue
			}
			child.lastPickedAuth = entry.auth.ID
			v.lastPickedAuth = entry.auth.ID
			v.lastPickedParent = parent
			return entry
		}
	}
	return nil
}
