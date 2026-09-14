package auth

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const (
	routeTraceAttemptStateMetadataKey    = "__route_trace_attempt_state"
	routeTraceAttemptMetadataKey         = "__route_trace_attempt"
	routeTracePreviousAttemptMetadataKey = "__route_trace_previous_attempt"

	maxRouteTraceExcludedEvents    = 128
	maxRouteTraceNonTerminalEvents = cliproxyexecutor.MaxRouteTraceEvents - 8
)

type routeTraceAttemptState struct {
	mu                 sync.Mutex
	next               int
	emitted            int
	excluded           int
	pendingTruncated   int
	rootMetadata       map[string]any
	decisionCandidates map[int]*routeDecisionCandidateCollector
}

type routeDecisionCandidateCollector struct {
	candidates map[string]cliproxyexecutor.RouteDecisionCandidate
	order      []string
	truncated  int
}

func ensureRouteTraceState(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if routeTraceCallback(opts.Metadata) == nil {
		return opts
	}
	if state, _ := opts.Metadata[routeTraceAttemptStateMetadataKey].(*routeTraceAttemptState); state != nil {
		return opts
	}
	opts.Metadata[routeTraceAttemptStateMetadataKey] = &routeTraceAttemptState{rootMetadata: opts.Metadata}
	return opts
}

func nextRouteTraceAttempt(opts cliproxyexecutor.Options) (int, int) {
	state := routeTraceState(opts)
	if state == nil {
		return 0, 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.next++
	previous := state.next - 1
	if previous < 1 {
		previous = 0
	}
	return state.next, previous
}

func currentRouteTraceAttempt(opts cliproxyexecutor.Options) (int, int) {
	state := routeTraceState(opts)
	if state == nil {
		return 0, 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	previous := state.next - 1
	if previous < 1 {
		previous = 0
	}
	return state.next, previous
}

func routeTraceState(opts cliproxyexecutor.Options) *routeTraceAttemptState {
	return routeTraceStateFromMetadata(opts.Metadata)
}

func routeTraceStateFromMetadata(metadata map[string]any) *routeTraceAttemptState {
	if metadata == nil {
		return nil
	}
	state, _ := metadata[routeTraceAttemptStateMetadataKey].(*routeTraceAttemptState)
	return state
}

func bindRouteTraceRootMetadata(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	state := routeTraceState(opts)
	if state == nil {
		return opts
	}
	state.mu.Lock()
	state.rootMetadata = opts.Metadata
	state.mu.Unlock()
	return opts
}

func routeTraceRootMetadata(metadata map[string]any) map[string]any {
	state := routeTraceStateFromMetadata(metadata)
	if state == nil {
		return metadata
	}
	state.mu.Lock()
	rootMetadata := state.rootMetadata
	state.mu.Unlock()
	if rootMetadata == nil {
		return metadata
	}
	return rootMetadata
}

func isRouteTraceMetadataKey(key string) bool {
	if key == cliproxyexecutor.RouteTraceCallbackMetadataKey ||
		key == cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey {
		return true
	}
	return strings.HasPrefix(key, "__route_trace_")
}

func externalRouteTraceMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if isRouteTraceMetadataKey(key) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func externalRouteTraceOptions(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	opts.Metadata = externalRouteTraceMetadata(opts.Metadata)
	return opts
}

func restoreRouteTraceMetadata(metadata, original map[string]any) map[string]any {
	if routeTraceCallback(original) == nil {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	for key, value := range original {
		if isRouteTraceMetadataKey(key) {
			metadata[key] = value
		}
	}
	return metadata
}

func withRouteTraceAttempt(opts cliproxyexecutor.Options, attempt, previousAttempt int) cliproxyexecutor.Options {
	if routeTraceCallback(opts.Metadata) == nil {
		return opts
	}
	metadata := maps.Clone(opts.Metadata)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[routeTraceAttemptMetadataKey] = attempt
	metadata[routeTracePreviousAttemptMetadataKey] = previousAttempt
	opts.Metadata = metadata
	return opts
}

func routeTraceAttempt(opts cliproxyexecutor.Options) (int, int) {
	if opts.Metadata == nil {
		return 0, 0
	}
	attempt, _ := opts.Metadata[routeTraceAttemptMetadataKey].(int)
	previousAttempt, _ := opts.Metadata[routeTracePreviousAttemptMetadataKey].(int)
	return attempt, previousAttempt
}

type routeTraceStreamContextKey struct{}

type routeTraceStreamInfo struct {
	opts        cliproxyexecutor.Options
	targetModel string
}

func contextWithRouteTraceStream(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	targetModel string,
) context.Context {
	if routeTraceCallback(opts.Metadata) == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(
		ctx, routeTraceStreamContextKey{}, routeTraceStreamInfo{
			opts:        opts,
			targetModel: strings.TrimSpace(targetModel),
		},
	)
}

func routeTraceStreamInfoFromContext(ctx context.Context) (routeTraceStreamInfo, bool) {
	if ctx == nil {
		return routeTraceStreamInfo{}, false
	}
	info, ok := ctx.Value(routeTraceStreamContextKey{}).(routeTraceStreamInfo)
	return info, ok && routeTraceCallback(info.opts.Metadata) != nil
}

func routeTraceCallback(metadata map[string]any) func(cliproxyexecutor.RouteTraceEvent) {
	if metadata == nil {
		return nil
	}
	callback, _ := metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey].(func(cliproxyexecutor.RouteTraceEvent))
	return callback
}

func routeTraceTruncatedCallback(metadata map[string]any) func(int) {
	if metadata == nil {
		return nil
	}
	callback, _ := metadata[cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey].(func(int))
	return callback
}

func publishRouteTraceTruncated(metadata map[string]any, count int) {
	callback := routeTraceTruncatedCallback(metadata)
	if callback == nil || count <= 0 {
		return
	}
	func() {
		defer func() {
			_ = recover()
		}()
		callback(count)
	}()
}

func routeTraceEventIsExcluded(event cliproxyexecutor.RouteTraceEvent) bool {
	return event.Action == "candidate.excluded" || event.Action == "provider.excluded"
}

func routeTraceEventIsTerminal(event cliproxyexecutor.RouteTraceEvent) bool {
	if event.FinalTarget {
		return true
	}
	switch event.Action {
	case "route.selection_failed", "route.stopped", "retry.stopped", "cooldown.failed":
		return true
	default:
		return false
	}
}

func reserveRouteTraceEvent(metadata map[string]any, event cliproxyexecutor.RouteTraceEvent) bool {
	if routeTraceCallback(metadata) == nil || strings.TrimSpace(event.Action) == "" {
		return false
	}
	state := routeTraceStateFromMetadata(metadata)
	if state == nil {
		return true
	}
	state.mu.Lock()
	excluded := routeTraceEventIsExcluded(event)
	terminal := routeTraceEventIsTerminal(event)
	allowed := state.emitted < cliproxyexecutor.MaxRouteTraceEvents &&
		(!excluded || state.excluded < maxRouteTraceExcludedEvents) &&
		(terminal || state.emitted < maxRouteTraceNonTerminalEvents)
	if allowed {
		state.emitted++
		if excluded {
			state.excluded++
		}
	} else {
		state.pendingTruncated++
	}
	state.mu.Unlock()
	return allowed
}

func flushRouteTraceTruncated(metadata map[string]any) {
	state := routeTraceStateFromMetadata(metadata)
	if state == nil {
		return
	}
	state.mu.Lock()
	count := state.pendingTruncated
	state.pendingTruncated = 0
	state.mu.Unlock()
	publishRouteTraceTruncated(metadata, count)
}

func publishReservedRouteTraceEvent(metadata map[string]any, event cliproxyexecutor.RouteTraceEvent) {
	callback := routeTraceCallback(metadata)
	if callback == nil {
		return
	}
	func() {
		defer func() {
			_ = recover()
		}()
		callback(event)
	}()
}

func publishRouteTraceEvent(metadata map[string]any, event cliproxyexecutor.RouteTraceEvent) {
	if !reserveRouteTraceEvent(metadata, event) {
		flushRouteTraceTruncated(metadata)
		return
	}
	publishReservedRouteTraceEvent(metadata, event)
}

func derivedRouteTraceAuthIndex(auth *Auth) string {
	traceAuth := auth.Clone()
	traceAuth.Index = ""
	traceAuth.indexAssigned = false
	return strings.ToLower(strings.TrimSpace(traceAuth.EnsureIndex()))
}

func routeTraceAuthIndex(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Generation == 0 {
		if index := derivedRouteTraceAuthIndex(auth); index != "" {
			return index
		}
	}
	index := strings.ToLower(strings.TrimSpace(auth.Index))
	if len(index) == 16 {
		valid := true
		for _, char := range index {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				valid = false
				break
			}
		}
		if valid {
			return index
		}
	}
	return derivedRouteTraceAuthIndex(auth)
}

func authRouteTraceEvent(
	opts cliproxyexecutor.Options,
	auth *Auth,
	event cliproxyexecutor.RouteTraceEvent,
) (cliproxyexecutor.RouteTraceEvent, bool) {
	if routeTraceCallback(opts.Metadata) == nil || strings.TrimSpace(event.Action) == "" {
		return cliproxyexecutor.RouteTraceEvent{}, false
	}
	if event.Attempt == 0 {
		event.Attempt, event.PreviousAttempt = routeTraceAttempt(opts)
	}
	if auth != nil {
		if event.Provider == "" {
			event.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		if event.AuthIndex == "" {
			event.AuthIndex = routeTraceAuthIndex(auth)
		}
		if event.EffectScope == cliproxyexecutor.StateEffectScopeCredential {
			event.EffectKey = event.AuthIndex
		}
	}
	return event, true
}

func publishAuthRouteTraceEvent(
	opts cliproxyexecutor.Options,
	auth *Auth,
	event cliproxyexecutor.RouteTraceEvent,
) {
	if !reserveRouteTraceEvent(opts.Metadata, event) {
		flushRouteTraceTruncated(opts.Metadata)
		return
	}
	event, ok := authRouteTraceEvent(opts, auth, event)
	if !ok {
		return
	}
	publishReservedRouteTraceEvent(opts.Metadata, event)
}

func publishResultRouteTraceEvent(
	opts cliproxyexecutor.Options,
	auth *Auth,
	event cliproxyexecutor.RouteTraceEvent,
	result Result,
) {
	applyDispositionToRouteTrace(&event, result.Disposition, result.StateGeneration)
	publishAuthRouteTraceEvent(opts, auth, event)
}

func appendRouteTraceEvent(
	events *[]cliproxyexecutor.RouteTraceEvent,
	metadata map[string]any,
	event cliproxyexecutor.RouteTraceEvent,
) {
	if events == nil || !reserveRouteTraceEvent(metadata, event) {
		return
	}
	*events = append(*events, event)
}

func appendAuthRouteTraceEvent(
	events *[]cliproxyexecutor.RouteTraceEvent,
	opts cliproxyexecutor.Options,
	auth *Auth,
	event cliproxyexecutor.RouteTraceEvent,
) {
	if events == nil || !reserveRouteTraceEvent(opts.Metadata, event) {
		return
	}
	event, ok := authRouteTraceEvent(opts, auth, event)
	if !ok {
		return
	}
	*events = append(*events, event)
}

func flushRouteTraceEvents(metadata map[string]any, events []cliproxyexecutor.RouteTraceEvent) {
	for _, event := range events {
		publishReservedRouteTraceEvent(metadata, event)
	}
	flushRouteTraceTruncated(metadata)
}

func routeTraceError(
	metadata map[string]any,
	err error,
	fallback string,
) (code string, status int, retryAfterMS int64) {
	if routeTraceCallback(metadata) == nil {
		return "", 0, 0
	}
	code = normalizeRouteTraceCode(fallback)
	if code == "" {
		code = "failed"
	}
	defer func() {
		if recover() != nil {
			status = 0
			retryAfterMS = 0
		}
	}()
	if err == nil {
		return "success", 0, 0
	}
	status = statusCodeFromError(err)
	switch {
	case errors.Is(err, context.Canceled):
		code = "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	case status == 401:
		code = "unauthorized"
	case status == 402:
		code = "payment_required"
	case status == 403:
		code = "forbidden"
	case status == 408:
		code = "request_timeout"
	case status == 409:
		code = "conflict"
	case status == 429:
		code = "rate_limited"
	case status >= 500:
		code = "upstream_error"
	}
	if retryAfter := routeTraceRetryAfter(err); retryAfter != nil && *retryAfter > 0 {
		retryAfterMS = retryAfter.Milliseconds()
	}
	return code, status, retryAfterMS
}

func publishStreamRouteTraceResult(
	ctx context.Context,
	auth *Auth,
	provider,
	routeModel string,
	err error,
) {
	info, ok := routeTraceStreamInfoFromContext(ctx)
	if !ok {
		return
	}
	if err != nil {
		result := executionResult(
			auth, provider, routeModel, err, cliproxyexecutor.DispatchPhaseResponseStarted,
		)
		resultCode, httpStatus, retryAfterMS := routeTraceError(info.opts.Metadata, err, "stream_failed")
		failedEvent := cliproxyexecutor.RouteTraceEvent{
			Provider:     provider,
			Model:        routeModel,
			TargetModel:  info.targetModel,
			Action:       "attempt.failed",
			Reason:       "stream_terminated",
			ResultCode:   resultCode,
			HTTPStatus:   httpStatus,
			RetryAfterMS: retryAfterMS,
			FinalTarget:  true,
		}
		applyDispositionToRouteTrace(&failedEvent, result.Disposition, result.StateGeneration)
		publishAuthRouteTraceEvent(info.opts, auth, failedEvent)
		recordedEvent := failedEvent
		recordedEvent.Action = "result.recorded"
		recordedEvent.Reason = ""
		publishAuthRouteTraceEvent(info.opts, auth, recordedEvent)
		return
	}
	result := executionResult(
		auth, provider, routeModel, nil, cliproxyexecutor.DispatchPhaseDownstreamCommitted,
	)
	completedEvent := cliproxyexecutor.RouteTraceEvent{
		Provider:    provider,
		Model:       routeModel,
		TargetModel: info.targetModel,
		Action:      "attempt.completed",
		ResultCode:  "success",
		HTTPStatus:  200,
		FinalTarget: true,
	}
	applyDispositionToRouteTrace(&completedEvent, result.Disposition, result.StateGeneration)
	publishAuthRouteTraceEvent(info.opts, auth, completedEvent)
	recordedEvent := completedEvent
	recordedEvent.Action = "result.recorded"
	publishAuthRouteTraceEvent(info.opts, auth, recordedEvent)
}

func publishStreamRouteTraceCancellation(
	ctx context.Context,
	auth *Auth,
	provider,
	routeModel string,
	err error,
) {
	info, ok := routeTraceStreamInfoFromContext(ctx)
	if !ok || err == nil {
		return
	}
	result := executionResult(
		auth, provider, routeModel, err, cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	resultCode, httpStatus, retryAfterMS := routeTraceError(info.opts.Metadata, err, "stream_canceled")
	failedEvent := cliproxyexecutor.RouteTraceEvent{
		Provider:     provider,
		Model:        routeModel,
		TargetModel:  info.targetModel,
		Action:       "attempt.failed",
		Reason:       "request_canceled",
		ResultCode:   resultCode,
		HTTPStatus:   httpStatus,
		RetryAfterMS: retryAfterMS,
		FinalTarget:  true,
	}
	applyDispositionToRouteTrace(&failedEvent, result.Disposition, result.StateGeneration)
	publishAuthRouteTraceEvent(info.opts, auth, failedEvent)
}

func routeTraceRetryAfter(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var provider retryAfterProvider
	if !errors.As(err, &provider) || provider == nil {
		return nil
	}
	retryAfter := provider.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	return new(*retryAfter)
}

func normalizeRouteTraceCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' {
			continue
		}
		return ""
	}
	return value
}

func recordRouteDecisionCandidate(
	opts cliproxyexecutor.Options,
	auth *Auth,
	model,
	targetModel string,
	native,
	converted bool,
) {
	state := routeTraceState(opts)
	if state == nil || auth == nil {
		return
	}
	attempt, _ := routeTraceAttempt(opts)
	authIndex := routeTraceAuthIndex(auth)
	if authIndex == "" {
		return
	}
	candidate := cliproxyexecutor.RouteDecisionCandidate{
		Provider:        strings.ToLower(strings.TrimSpace(auth.Provider)),
		Model:           strings.TrimSpace(model),
		TargetModel:     strings.TrimSpace(targetModel),
		AuthIndex:       authIndex,
		Priority:        authPriority(auth),
		EffectiveWeight: authWeight(auth),
		Native:          new(native),
		Converted:       new(converted),
		Affinity:        "none",
		Cooldowns:       routeDecisionCooldowns(auth, model, time.Now()),
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) == auth.ID {
		candidate.Affinity = "pinned"
		candidate.MatchedRules = append(candidate.MatchedRules, "pinned_auth")
	}
	if converted {
		candidate.MatchedRules = append(candidate.MatchedRules, "model_mapping")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.decisionCandidates == nil {
		state.decisionCandidates = make(map[int]*routeDecisionCandidateCollector)
	}
	collector := state.decisionCandidates[attempt]
	if collector == nil {
		collector = &routeDecisionCandidateCollector{
			candidates: make(map[string]cliproxyexecutor.RouteDecisionCandidate),
		}
		state.decisionCandidates[attempt] = collector
	}
	if previous, exists := collector.candidates[authIndex]; exists {
		candidate.ExcludedReasons = append([]string(nil), previous.ExcludedReasons...)
		collector.candidates[authIndex] = candidate
		return
	}
	if len(collector.order) >= cliproxyexecutor.MaxRouteDecisionCandidates {
		collector.truncated++
		return
	}
	collector.order = append(collector.order, authIndex)
	collector.candidates[authIndex] = candidate
}

func recordRouteDecisionCandidateExclusion(opts cliproxyexecutor.Options, auth *Auth, reason string) {
	state := routeTraceState(opts)
	if state == nil || auth == nil {
		return
	}
	attempt, _ := routeTraceAttempt(opts)
	authIndex := routeTraceAuthIndex(auth)
	if authIndex == "" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	collector := state.decisionCandidates[attempt]
	if collector == nil {
		return
	}
	candidate, exists := collector.candidates[authIndex]
	if !exists {
		return
	}
	if slices.Contains(candidate.ExcludedReasons, reason) {
		return
	}
	candidate.ExcludedReasons = append(candidate.ExcludedReasons, reason)
	collector.candidates[authIndex] = candidate
}

func snapshotRouteDecisionCandidates(
	opts cliproxyexecutor.Options,
) ([]cliproxyexecutor.RouteDecisionCandidate, int) {
	state := routeTraceState(opts)
	if state == nil {
		return nil, 0
	}
	attempt, _ := routeTraceAttempt(opts)
	state.mu.Lock()
	defer state.mu.Unlock()
	collector := state.decisionCandidates[attempt]
	if collector == nil {
		return nil, 0
	}
	candidates := make([]cliproxyexecutor.RouteDecisionCandidate, 0, len(collector.order))
	for _, authIndex := range collector.order {
		candidate, exists := collector.candidates[authIndex]
		if !exists {
			continue
		}
		candidates = append(candidates, cloneRouteDecisionCandidate(candidate))
	}
	return candidates, collector.truncated
}

func cloneRouteDecisionCandidate(
	candidate cliproxyexecutor.RouteDecisionCandidate,
) cliproxyexecutor.RouteDecisionCandidate {
	candidate.Native = cloneBool(candidate.Native)
	candidate.Converted = cloneBool(candidate.Converted)
	candidate.MatchedRules = append([]string(nil), candidate.MatchedRules...)
	candidate.ExcludedReasons = append([]string(nil), candidate.ExcludedReasons...)
	if candidate.QuotaHeadroom != nil {
		candidate.QuotaHeadroom = new(*candidate.QuotaHeadroom)
	}
	if candidate.QuotaResetAfterMS != nil {
		candidate.QuotaResetAfterMS = new(*candidate.QuotaResetAfterMS)
	}
	if candidate.InFlight != nil {
		candidate.InFlight = new(*candidate.InFlight)
	}
	return candidate
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	return new(*value)
}

type routeDecisionDraft struct {
	snapshot     cliproxyexecutor.RouteDecisionSnapshot
	byIndex      map[string]int
	missing      map[string]struct{}
	countMissing bool
}

func newRouteDecisionDraft(
	opts cliproxyexecutor.Options,
	providers []string,
	model string,
	strategy schedulerStrategy,
) *routeDecisionDraft {
	if routeTraceCallback(opts.Metadata) == nil {
		return nil
	}
	candidates, truncated := snapshotRouteDecisionCandidates(opts)
	draft := &routeDecisionDraft{
		snapshot: cliproxyexecutor.RouteDecisionSnapshot{
			Strategy:            schedulerStrategyName(strategy),
			RequestedModel:      requestedModelFromMetadata(opts.Metadata, model),
			RoutedModel:         strings.TrimSpace(model),
			Providers:           append([]string(nil), providers...),
			Affinity:            "none",
			CandidateScope:      "complete",
			Candidates:          candidates,
			TruncatedCandidates: truncated,
		},
		byIndex:      make(map[string]int, len(candidates)),
		missing:      make(map[string]struct{}),
		countMissing: truncated == 0,
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		draft.snapshot.Affinity = "pinned"
	}
	for index := range draft.snapshot.Candidates {
		draft.byIndex[draft.snapshot.Candidates[index].AuthIndex] = index
	}
	return draft
}

func (d *routeDecisionDraft) mergeShard(shard *modelScheduler, model string) {
	if d == nil || shard == nil {
		return
	}
	authIDs := make([]string, 0, len(shard.entries))
	for authID := range shard.entries {
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	for _, authID := range authIDs {
		entry := shard.entries[authID]
		if entry == nil || entry.auth == nil {
			continue
		}
		d.mergeAuth(entry.auth, model, entry.meta)
		switch entry.state {
		case scheduledStateReady:
		case scheduledStateCooldown:
			d.exclude(entry.auth, model, "cooldown")
		case scheduledStateBlocked:
			d.exclude(entry.auth, model, "blocked")
		case scheduledStateDisabled:
			d.exclude(entry.auth, model, "disabled")
		}
	}
}

func (d *routeDecisionDraft) mergeAuth(auth *Auth, model string, meta *scheduledAuthMeta) {
	if d == nil || auth == nil {
		return
	}
	index := d.ensureAuth(auth, model)
	if index < 0 {
		return
	}
	candidate := d.snapshot.Candidates[index]
	candidate.Cooldowns = routeDecisionCooldowns(auth, model, time.Now())
	if meta != nil {
		candidate.Provider = meta.providerKey
		candidate.Priority = meta.priority
		candidate.EffectiveWeight = meta.weight
	}
	d.snapshot.Candidates[index] = candidate
}

func (d *routeDecisionDraft) mergeQuotaSignals(
	entries []*scheduledAuth,
	signals map[string]quotaAwareCandidateSignal,
) {
	if d == nil {
		return
	}
	for _, entry := range entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		signal, ok := signals[entry.auth.ID]
		if !ok {
			continue
		}
		index := d.ensureAuth(entry.auth, d.snapshot.RoutedModel)
		if index < 0 {
			continue
		}
		candidate := d.snapshot.Candidates[index]
		candidate.QuotaStatus = string(signal.status)
		candidate.QuotaHeadroom = nil
		if signal.status == QuotaSchedulingStatusFresh {
			candidate.QuotaHeadroom = new(signal.headroom)
		}
		candidate.QuotaResetAfterMS = nil
		if signal.resetAfterMS != nil {
			candidate.QuotaResetAfterMS = new(*signal.resetAfterMS)
		}
		candidate.InFlight = new(signal.inFlight)
		d.snapshot.Candidates[index] = candidate
	}
}

func (d *routeDecisionDraft) exclude(auth *Auth, model, reason string) {
	if d == nil || auth == nil || strings.TrimSpace(reason) == "" {
		return
	}
	index := d.ensureAuth(auth, model)
	if index < 0 {
		return
	}
	candidate := d.snapshot.Candidates[index]
	candidate.ExcludedReasons = appendUniqueRouteDecisionValue(candidate.ExcludedReasons, reason)
	d.snapshot.Candidates[index] = candidate
}

func (d *routeDecisionDraft) selectCandidate(auth *Auth, model, rule string) {
	d.selectCandidateWithPriority(auth, model, rule, true)
}

func (d *routeDecisionDraft) selectCandidateWithPriority(
	auth *Auth,
	model,
	rule string,
	highestPriority bool,
) {
	if d == nil || auth == nil {
		return
	}
	index := d.ensureAuth(auth, model)
	if index < 0 {
		index = d.insertSelectedAuth(auth, model)
	}
	if index < 0 {
		return
	}
	candidate := d.snapshot.Candidates[index]
	candidate.Selected = true
	if highestPriority {
		candidate.MatchedRules = appendUniqueRouteDecisionValue(candidate.MatchedRules, "highest_priority")
	}
	candidate.MatchedRules = appendUniqueRouteDecisionValue(candidate.MatchedRules, rule)
	d.snapshot.Candidates[index] = candidate
	d.snapshot.MatchedRule = rule
	d.snapshot.Selected = &cliproxyexecutor.RouteDecisionSelection{
		Provider:    candidate.Provider,
		Model:       candidate.Model,
		TargetModel: candidate.TargetModel,
		AuthIndex:   candidate.AuthIndex,
		MatchedRule: rule,
	}
}

func (d *routeDecisionDraft) addSelectedRules(auth *Auth, rules ...string) {
	if d == nil || auth == nil {
		return
	}
	index, exists := d.byIndex[routeTraceAuthIndex(auth)]
	if !exists {
		return
	}
	candidate := d.snapshot.Candidates[index]
	for _, rule := range rules {
		candidate.MatchedRules = appendUniqueRouteDecisionValue(candidate.MatchedRules, rule)
	}
	d.snapshot.Candidates[index] = candidate
}

func (d *routeDecisionDraft) ensureAuth(auth *Auth, model string) int {
	if d == nil || auth == nil {
		return -1
	}
	authIndex := routeTraceAuthIndex(auth)
	if authIndex == "" {
		return -1
	}
	if index, exists := d.byIndex[authIndex]; exists {
		return index
	}
	candidate := cliproxyexecutor.RouteDecisionCandidate{
		Provider:        strings.ToLower(strings.TrimSpace(auth.Provider)),
		Model:           strings.TrimSpace(model),
		TargetModel:     strings.TrimSpace(model),
		AuthIndex:       authIndex,
		Priority:        authPriority(auth),
		EffectiveWeight: authWeight(auth),
		Affinity:        "none",
		Cooldowns:       routeDecisionCooldowns(auth, model, time.Now()),
	}
	if d.snapshot.Affinity == "pinned" {
		candidate.Affinity = "pinned"
		candidate.MatchedRules = append(candidate.MatchedRules, "pinned_auth")
	}
	if len(d.snapshot.Candidates) < cliproxyexecutor.MaxRouteDecisionCandidates {
		index := len(d.snapshot.Candidates)
		d.snapshot.Candidates = append(d.snapshot.Candidates, candidate)
		d.byIndex[authIndex] = index
		return index
	}
	if d.countMissing {
		if _, exists := d.missing[authIndex]; !exists {
			d.missing[authIndex] = struct{}{}
			d.snapshot.TruncatedCandidates++
		}
	}
	return -1
}

func (d *routeDecisionDraft) insertSelectedAuth(auth *Auth, model string) int {
	if d == nil || auth == nil || len(d.snapshot.Candidates) == 0 {
		return -1
	}
	authIndex := routeTraceAuthIndex(auth)
	if authIndex == "" {
		return -1
	}
	index := len(d.snapshot.Candidates) - 1
	delete(d.byIndex, d.snapshot.Candidates[index].AuthIndex)
	d.snapshot.Candidates[index] = cliproxyexecutor.RouteDecisionCandidate{
		Provider:        strings.ToLower(strings.TrimSpace(auth.Provider)),
		Model:           strings.TrimSpace(model),
		TargetModel:     strings.TrimSpace(model),
		AuthIndex:       authIndex,
		Priority:        authPriority(auth),
		EffectiveWeight: authWeight(auth),
		Affinity:        d.snapshot.Affinity,
		Cooldowns:       routeDecisionCooldowns(auth, model, time.Now()),
	}
	d.byIndex[authIndex] = index
	return index
}

func (d *routeDecisionDraft) event(opts cliproxyexecutor.Options) cliproxyexecutor.RouteTraceEvent {
	if d == nil {
		return cliproxyexecutor.RouteTraceEvent{}
	}
	attempt, previousAttempt := routeTraceAttempt(opts)
	snapshot := cloneRouteDecisionSnapshot(d.snapshot)
	provider := ""
	switch len(snapshot.Providers) {
	case 1:
		provider = snapshot.Providers[0]
	default:
		if len(snapshot.Providers) > 1 {
			provider = "mixed"
		}
	}
	return cliproxyexecutor.RouteTraceEvent{
		Attempt:         attempt,
		PreviousAttempt: previousAttempt,
		Provider:        provider,
		Model:           snapshot.RoutedModel,
		Action:          "route.decision",
		Reason:          snapshot.MatchedRule,
		Decision:        &snapshot,
	}
}

func appendRouteDecisionEvent(
	events *[]cliproxyexecutor.RouteTraceEvent,
	opts cliproxyexecutor.Options,
	draft *routeDecisionDraft,
) {
	if draft == nil {
		return
	}
	appendRouteTraceEvent(events, opts.Metadata, draft.event(opts))
}

func publishObservedRouteDecision(
	opts cliproxyexecutor.Options,
	providers []string,
	model string,
	strategy schedulerStrategy,
	selected *Auth,
	rule,
	affinity string,
) {
	publishObservedRouteDecisionWithScope(
		opts, providers, model, strategy, selected, rule, affinity, "complete",
	)
}

func publishSelectedOnlyRouteDecision(
	opts cliproxyexecutor.Options,
	providers []string,
	model string,
	strategy schedulerStrategy,
	selected *Auth,
	rule,
	affinity string,
) {
	publishObservedRouteDecisionWithScope(
		opts, providers, model, strategy, selected, rule, affinity, "selected_only",
	)
}

func publishObservedRouteDecisionWithScope(
	opts cliproxyexecutor.Options,
	providers []string,
	model string,
	strategy schedulerStrategy,
	selected *Auth,
	rule,
	affinity,
	candidateScope string,
) {
	draft := newRouteDecisionDraft(opts, providers, model, strategy)
	if draft == nil {
		return
	}
	draft.snapshot.CandidateScope = candidateScope
	if affinity != "" {
		draft.snapshot.Affinity = affinity
	}
	if selected != nil {
		draft.selectCandidateWithPriority(selected, model, rule, false)
		if index, exists := draft.byIndex[routeTraceAuthIndex(selected)]; exists {
			candidate := draft.snapshot.Candidates[index]
			if affinity != "" {
				candidate.Affinity = affinity
			}
			candidate.MatchedRules = appendUniqueRouteDecisionValue(candidate.MatchedRules, rule)
			draft.snapshot.Candidates[index] = candidate
		}
	}
	publishRouteTraceEvent(opts.Metadata, draft.event(opts))
}

func cloneRouteDecisionSnapshot(
	snapshot cliproxyexecutor.RouteDecisionSnapshot,
) cliproxyexecutor.RouteDecisionSnapshot {
	snapshot.Providers = append([]string(nil), snapshot.Providers...)
	snapshot.Candidates = append([]cliproxyexecutor.RouteDecisionCandidate(nil), snapshot.Candidates...)
	for index := range snapshot.Candidates {
		snapshot.Candidates[index] = cloneRouteDecisionCandidate(snapshot.Candidates[index])
	}
	if snapshot.Selected != nil {
		selected := *snapshot.Selected
		snapshot.Selected = &selected
	}
	return snapshot
}

func routeDecisionCooldowns(auth *Auth, model string, now time.Time) cliproxyexecutor.RouteDecisionCooldowns {
	cooldowns := cliproxyexecutor.RouteDecisionCooldowns{
		ExactModel:  cliproxyexecutor.RouteDecisionCooldown{Known: true},
		QuotaFamily: cliproxyexecutor.RouteDecisionCooldown{Known: true},
		Credential:  cliproxyexecutor.RouteDecisionCooldown{Known: true},
	}
	if auth == nil {
		return cooldowns
	}
	state, _ := findModelState(auth, model)
	if state != nil {
		hasQuotaState := hasQuotaCooldownState(state.Quota, state.LastError)
		exactObserved := state.Status == StatusDisabled ||
			state.EffectScope == cliproxyexecutor.StateEffectScopeExactModel ||
			state.Unavailable && !hasQuotaState
		if exactObserved {
			exactActive := state.Status == StatusDisabled || state.Unavailable && !hasQuotaState
			cooldowns.ExactModel = routeDecisionCooldown(
				exactActive,
				state.NextRetryAfter,
				"exact_model_unavailable",
				state.EffectKey,
				state.Generation,
				state.ResetAuthority,
				now,
			)
		}
		quotaObserved := state.Quota.Exceeded ||
			state.EffectScope == cliproxyexecutor.StateEffectScopeQuotaFamily
		if quotaObserved {
			cooldowns.QuotaFamily = routeDecisionCooldown(
				state.Unavailable && state.Quota.Exceeded,
				state.Quota.NextRecoverAt,
				"quota_exceeded",
				state.Quota.Family,
				state.Quota.Generation,
				state.Quota.ResetAuthority,
				now,
			)
		}
	}
	if !cooldowns.QuotaFamily.Active &&
		auth.StateEffectScope == cliproxyexecutor.StateEffectScopeQuotaFamily {
		cooldowns.QuotaFamily = routeDecisionCooldown(
			auth.Unavailable && auth.Quota.Exceeded,
			auth.Quota.NextRecoverAt,
			"quota_exceeded",
			auth.Quota.Family,
			auth.Quota.Generation,
			auth.Quota.ResetAuthority,
			now,
		)
	}
	disabled := auth.Disabled || auth.Status == StatusDisabled
	credentialObserved := disabled || auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential ||
		auth.Unavailable && auth.StateEffectScope == "" && len(auth.ModelStates) == 0
	if credentialObserved {
		reason := "credential_unavailable"
		if disabled {
			reason = "disabled"
		}
		cooldowns.Credential = routeDecisionCooldown(
			disabled || auth.Unavailable && auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential ||
				auth.Unavailable && auth.StateEffectScope == "" && len(auth.ModelStates) == 0,
			auth.NextRetryAfter,
			reason,
			routeTraceAuthIndex(auth),
			auth.StateGeneration,
			auth.ResetAuthority,
			now,
		)
	}
	if auth.StateEffectScope == cliproxyexecutor.StateEffectScopeProvider {
		cooldowns.Provider = routeDecisionCooldown(
			auth.Unavailable,
			auth.NextRetryAfter,
			"provider_unavailable",
			auth.StateEffectKey,
			auth.StateGeneration,
			auth.ResetAuthority,
			now,
		)
	}
	return cooldowns
}

func routeDecisionCooldown(
	blocked bool,
	nextRetry time.Time,
	reason,
	effectKey string,
	generation uint64,
	resetAuthority StateResetAuthority,
	now time.Time,
) cliproxyexecutor.RouteDecisionCooldown {
	active := blocked && (nextRetry.IsZero() || nextRetry.After(now))
	cooldown := cliproxyexecutor.RouteDecisionCooldown{
		Known:           true,
		Active:          active,
		Indefinite:      active && nextRetry.IsZero(),
		EffectKey:       effectKey,
		StateGeneration: generation,
		ResetAuthority:  string(resetAuthority),
	}
	if !active {
		return cooldown
	}
	cooldown.Reason = reason
	if nextRetry.After(now) {
		cooldown.RetryAfterMS = nextRetry.Sub(now).Milliseconds()
	}
	return cooldown
}

func appendUniqueRouteDecisionValue(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func traceCandidateExcluded(opts cliproxyexecutor.Options, auth *Auth, model, reason string) {
	publishAuthRouteTraceEvent(
		opts, auth, cliproxyexecutor.RouteTraceEvent{
			Model:  model,
			Action: "candidate.excluded",
			Reason: reason,
		},
	)
}

func appendCandidateExcluded(
	events *[]cliproxyexecutor.RouteTraceEvent,
	opts cliproxyexecutor.Options,
	auth *Auth,
	model,
	reason string,
) {
	recordRouteDecisionCandidateExclusion(opts, auth, reason)
	appendAuthRouteTraceEvent(
		events, opts, auth, cliproxyexecutor.RouteTraceEvent{
			Model:  model,
			Action: "candidate.excluded",
			Reason: reason,
		},
	)
}

func schedulerRouteTraceReason(strategy schedulerStrategy) string {
	switch strategy {
	case schedulerStrategyFillFirst:
		return "fill_first"
	case schedulerStrategyWeightedRoundRobin:
		return "weighted_round_robin"
	case schedulerStrategyQuotaAware:
		return "quota_aware"
	case schedulerStrategyRoundRobin:
		return "round_robin"
	default:
		return "custom"
	}
}
