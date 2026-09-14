package logging

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

const (
	RouteTraceGinKey    = "ROUTE_TRACE"
	maxRouteTraceEvents = cliproxyexecutor.MaxRouteTraceEvents
)

// RouteTraceSnapshot contains the bounded events captured for one request.
type RouteTraceSnapshot struct {
	Events          []cliproxyexecutor.RouteTraceEvent
	TruncatedEvents int
}

// RouteTrace stores non-secret routing events for one request.
type RouteTrace struct {
	mu                     sync.Mutex
	events                 []cliproxyexecutor.RouteTraceEvent
	truncatedEvents        int
	awaitingStreamTerminal bool
	streamTerminal         chan struct{}
}

// NewRouteTrace creates an empty request-scoped route trace.
func NewRouteTrace() *RouteTrace {
	return &RouteTrace{}
}

// Add records one event without allowing tracing failures to affect request execution.
func (t *RouteTrace) Add(event cliproxyexecutor.RouteTraceEvent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) >= maxRouteTraceEvents {
		t.truncatedEvents++
		return
	}
	event = sanitizeRouteTraceEvent(event)
	if event.Action == "" {
		return
	}
	event.Sequence = len(t.events) + 1
	event.Timestamp = time.Now().UTC()
	t.events = append(t.events, event)
	t.updateStreamTerminalLocked(event)
}

func (t *RouteTrace) updateStreamTerminalLocked(event cliproxyexecutor.RouteTraceEvent) {
	switch event.Action {
	case "attempt.stream_started":
		if !t.awaitingStreamTerminal {
			t.awaitingStreamTerminal = true
			t.streamTerminal = make(chan struct{})
		}
	case "result.recorded":
		t.completeStreamTerminalLocked()
	case "attempt.failed":
		if event.Reason == "request_canceled" {
			t.completeStreamTerminalLocked()
		}
	}
}

func (t *RouteTrace) completeStreamTerminalLocked() {
	if !t.awaitingStreamTerminal {
		return
	}
	t.awaitingStreamTerminal = false
	close(t.streamTerminal)
	t.streamTerminal = nil
}

// WaitForStreamTerminal waits for the result recorder when a streaming handler returns first.
func (t *RouteTrace) WaitForStreamTerminal(timeout time.Duration) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	if !t.awaitingStreamTerminal || t.streamTerminal == nil {
		t.mu.Unlock()
		return true
	}
	terminal := t.streamTerminal
	t.mu.Unlock()
	if timeout <= 0 {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-terminal:
		return true
	case <-timer.C:
		return false
	}
}

// addTruncated records events omitted by the producer-side request budget.
func (t *RouteTrace) addTruncated(count int) {
	if t == nil || count <= 0 {
		return
	}
	t.mu.Lock()
	t.truncatedEvents += count
	t.mu.Unlock()
}

// Callback returns a function that can safely outlive the Gin context.
func (t *RouteTrace) Callback() func(cliproxyexecutor.RouteTraceEvent) {
	if t == nil {
		return nil
	}
	return t.Add
}

// TruncatedCallback returns a function that records producer-side omissions.
func (t *RouteTrace) TruncatedCallback() func(int) {
	if t == nil {
		return nil
	}
	return t.addTruncated
}

// Snapshot returns a copy of the currently captured events.
func (t *RouteTrace) Snapshot() RouteTraceSnapshot {
	if t == nil {
		return RouteTraceSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	events := make([]cliproxyexecutor.RouteTraceEvent, len(t.events))
	for index := range t.events {
		events[index] = cloneRouteTraceEvent(t.events[index])
	}
	return RouteTraceSnapshot{
		Events:          events,
		TruncatedEvents: t.truncatedEvents,
	}
}

// GinRouteTraceCallback returns the callback attached by request logging middleware.
func GinRouteTraceCallback(c *gin.Context) func(cliproxyexecutor.RouteTraceEvent) {
	if c == nil {
		return nil
	}
	value, exists := c.Get(RouteTraceGinKey)
	if !exists {
		return nil
	}
	trace, _ := value.(*RouteTrace)
	return trace.Callback()
}

// GinRouteTraceTruncatedCallback returns the omission callback attached by request logging middleware.
func GinRouteTraceTruncatedCallback(c *gin.Context) func(int) {
	if c == nil {
		return nil
	}
	value, exists := c.Get(RouteTraceGinKey)
	if !exists {
		return nil
	}
	trace, _ := value.(*RouteTrace)
	return trace.TruncatedCallback()
}

// WriteRouteTrace writes newline-delimited JSON after the request timeline.
func WriteRouteTrace(w io.Writer, snapshot RouteTraceSnapshot) error {
	if len(snapshot.Events) == 0 && snapshot.TruncatedEvents == 0 {
		return nil
	}
	if _, errWrite := io.WriteString(w, "=== ROUTE TRACE ===\n"); errWrite != nil {
		return errWrite
	}
	encoder := json.NewEncoder(w)
	for _, event := range snapshot.Events {
		if errEncode := encoder.Encode(event); errEncode != nil {
			return errEncode
		}
	}
	if snapshot.TruncatedEvents > 0 {
		if errEncode := encoder.Encode(
			struct {
				TruncatedEvents int `json:"truncated_events"`
			}{TruncatedEvents: snapshot.TruncatedEvents},
		); errEncode != nil {
			return errEncode
		}
	}
	_, errWrite := io.WriteString(w, "\n")
	return errWrite
}

func sanitizeRouteTraceEvent(event cliproxyexecutor.RouteTraceEvent) cliproxyexecutor.RouteTraceEvent {
	event.Sequence = 0
	event.Timestamp = time.Time{}
	event.Provider = sanitizeRouteTraceIdentifier(event.Provider, 64)
	event.Model = sanitizeRouteTraceIdentifier(event.Model, 256)
	event.TargetModel = sanitizeRouteTraceIdentifier(event.TargetModel, 256)
	event.AuthIndex = sanitizeRouteTraceAuthIndex(event.AuthIndex)
	event.Action = sanitizeRouteTraceAction(event.Action)
	if event.Action == "" {
		return cliproxyexecutor.RouteTraceEvent{}
	}
	event.Reason = sanitizeRouteTraceReason(event.Reason)
	event.ResultCode = sanitizeRouteTraceResultCode(event.ResultCode)
	if event.Attempt < 0 {
		event.Attempt = 0
	}
	if event.PreviousAttempt < 0 || event.PreviousAttempt >= event.Attempt {
		event.PreviousAttempt = 0
	}
	if event.HTTPStatus < 100 || event.HTTPStatus > 599 {
		event.HTTPStatus = 0
	}
	if event.RetryAfterMS < 0 {
		event.RetryAfterMS = 0
	}
	event.DispatchPhase = sanitizeRouteTraceDispatchPhase(event.DispatchPhase)
	event.Outcome = sanitizeRouteTraceOutcome(event.Outcome)
	event.RetryDirective = sanitizeRouteTraceRetryDirective(event.RetryDirective)
	event.EffectScope = sanitizeRouteTraceEffectScope(event.EffectScope)
	if event.EffectScope == "" {
		event.EffectKey = ""
	} else if event.EffectScope == cliproxyexecutor.StateEffectScopeCredential {
		if event.EffectKey != "" {
			event.EffectKey = event.AuthIndex
		}
	} else {
		event.EffectKey = sanitizeRouteTraceIdentifier(event.EffectKey, 256)
	}
	if event.Action == "route.decision" {
		event.Decision = sanitizeRouteDecisionSnapshot(event.Decision)
		if event.Decision == nil {
			return cliproxyexecutor.RouteTraceEvent{}
		}
	} else {
		event.Decision = nil
	}
	return event
}

func sanitizeRouteTraceIdentifier(value string, maxRunes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if value == "" || strings.Contains(value, "://") {
		return ""
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"authorization", "bearer ", "api_key", "apikey", "cookie", "password", "proxy_url", "secret", "token",
	} {
		if strings.Contains(lower, marker) {
			return ""
		}
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '-', '_', '.', ':', '/', '+', '@':
			continue
		default:
			return ""
		}
	}
	return truncateRouteTraceString(value, maxRunes)
}

func sanitizeRouteTraceAuthIndex(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 16 {
		return ""
	}
	for _, char := range value {
		if char >= '0' && char <= '9' || char >= 'a' && char <= 'f' {
			continue
		}
		return ""
	}
	return value
}

func sanitizeRouteTraceAction(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "attempt.completed", "attempt.execute_retried", "attempt.execute_started", "attempt.failed",
		"attempt.intercept_failed", "attempt.model_rejected", "attempt.prepare_completed", "attempt.prepare_failed",
		"attempt.prepare_started", "attempt.retry", "attempt.stream_started", "auth.refresh_completed",
		"candidate.excluded", "candidate.selected", "cooldown.completed", "cooldown.failed", "cooldown.wait",
		"provider.excluded", "result.recorded", "retry.stopped", "route.decision", "route.selected",
		"route.selection_failed", "route.stopped":
		return value
	default:
		return ""
	}
}

func sanitizeRouteTraceReason(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "", "already_tried", "antigravity_credits_fallback", "auth_refreshed", "blocked", "context_1m_unsupported",
		"cooldown", "credential_limit_reached", "custom", "disabled", "executor_not_registered", "fill_first",
		"home_dispatch", "home_dispatch_failed", "home_redispatch", "local_scheduler", "missing_execution_target",
		"model_not_supported", "model_shard_missing", "model_unsupported", "next_credential",
		"no_selectable_credential", "not_retryable", "pinned_auth_mismatch", "pinned_auth_provider_unavailable",
		"prepare_failed", "provider_missing", "provider_not_registered", "provider_not_requested", "request_canceled",
		"request_filter", "request_intercepted", "request_invalid", "request_scoped", "retry_after_failure",
		"round_robin", "round_robin_group", "runtime_auth_changed", "session_affinity",
		"session_affinity_fallback", "stream_bootstrap_failed", "stream_terminated", "transport_preference",
		"unavailable", "upstream_failure", "wait_interrupted", "weight_non_positive", "weighted_round_robin":
		return value
	default:
		return ""
	}
}

func sanitizeRouteTraceResultCode(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "", "conflict", "context_canceled", "cooldown_wait_failed", "credential_limit_reached",
		"deadline_exceeded", "execution_failed", "executor_not_found", "failed", "forbidden",
		"home_dispatch_failed", "model_not_supported", "payment_required", "prepare_failed", "rate_limited",
		"request_intercepted", "request_scoped", "request_timeout", "retry_stopped", "selection_failed",
		"stale_selection", "stream_bootstrap_failed", "stream_canceled", "stream_failed", "stream_started",
		"success", "unauthorized", "upstream_error":
		return value
	default:
		return ""
	}
}

func sanitizeRouteTraceDispatchPhase(
	value cliproxyexecutor.DispatchPhase,
) cliproxyexecutor.DispatchPhase {
	switch value {
	case "", cliproxyexecutor.DispatchPhaseNotSent, cliproxyexecutor.DispatchPhaseMaybeSent,
		cliproxyexecutor.DispatchPhaseResponseStarted, cliproxyexecutor.DispatchPhaseDownstreamCommitted:
		return value
	default:
		return ""
	}
}

func sanitizeRouteTraceOutcome(value cliproxyexecutor.ExecutionOutcome) cliproxyexecutor.ExecutionOutcome {
	switch value {
	case "", cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		cliproxyexecutor.ExecutionOutcomeSucceeded, cliproxyexecutor.ExecutionOutcomeFailed,
		cliproxyexecutor.ExecutionOutcomeUnknown:
		return value
	default:
		return ""
	}
}

func sanitizeRouteTraceRetryDirective(
	value cliproxyexecutor.RetryDirective,
) cliproxyexecutor.RetryDirective {
	switch value {
	case "", cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh,
		cliproxyexecutor.RetryDirectiveNextAuth, cliproxyexecutor.RetryDirectiveNextModel,
		cliproxyexecutor.RetryDirectiveNextProvider, cliproxyexecutor.RetryDirectiveRetryAfter,
		cliproxyexecutor.RetryDirectiveNoRetry:
		return value
	default:
		return ""
	}
}

func sanitizeRouteTraceEffectScope(
	value cliproxyexecutor.StateEffectScope,
) cliproxyexecutor.StateEffectScope {
	switch value {
	case "", cliproxyexecutor.StateEffectScopeRequest, cliproxyexecutor.StateEffectScopeExactModel,
		cliproxyexecutor.StateEffectScopeQuotaFamily, cliproxyexecutor.StateEffectScopeCredential,
		cliproxyexecutor.StateEffectScopeGroup, cliproxyexecutor.StateEffectScopeProvider:
		return value
	default:
		return ""
	}
}

func cloneRouteTraceEvent(event cliproxyexecutor.RouteTraceEvent) cliproxyexecutor.RouteTraceEvent {
	if event.Decision != nil {
		decision := *event.Decision
		decision.Providers = append([]string(nil), decision.Providers...)
		decision.Candidates = append([]cliproxyexecutor.RouteDecisionCandidate(nil), decision.Candidates...)
		for index := range decision.Candidates {
			candidate := decision.Candidates[index]
			candidate.Native = cloneRouteTraceBool(candidate.Native)
			candidate.Converted = cloneRouteTraceBool(candidate.Converted)
			candidate.MatchedRules = append([]string(nil), candidate.MatchedRules...)
			candidate.ExcludedReasons = append([]string(nil), candidate.ExcludedReasons...)
			if candidate.QuotaHeadroom != nil {
				candidate.QuotaHeadroom = new(*candidate.QuotaHeadroom)
			}
			if candidate.InFlight != nil {
				candidate.InFlight = new(*candidate.InFlight)
			}
			decision.Candidates[index] = candidate
		}
		if decision.Selected != nil {
			selected := *decision.Selected
			decision.Selected = &selected
		}
		event.Decision = &decision
	}
	return event
}

func sanitizeRouteDecisionSnapshot(
	snapshot *cliproxyexecutor.RouteDecisionSnapshot,
) *cliproxyexecutor.RouteDecisionSnapshot {
	if snapshot == nil {
		return nil
	}
	clean := *snapshot
	clean.Strategy = sanitizeRouteDecisionStrategy(clean.Strategy)
	if clean.Strategy == "" {
		return nil
	}
	clean.RequestedModel = sanitizeRouteTraceIdentifier(clean.RequestedModel, 256)
	clean.RoutedModel = sanitizeRouteTraceIdentifier(clean.RoutedModel, 256)
	clean.Affinity = sanitizeRouteDecisionAffinity(clean.Affinity)
	clean.MatchedRule = sanitizeRouteDecisionRule(clean.MatchedRule)
	clean.CandidateScope = sanitizeRouteDecisionCandidateScope(clean.CandidateScope)
	clean.Providers = sanitizeRouteDecisionIdentifiers(clean.Providers, 64)
	if clean.TruncatedCandidates < 0 {
		clean.TruncatedCandidates = 0
	}
	candidates := snapshot.Candidates
	if len(candidates) > cliproxyexecutor.MaxRouteDecisionCandidates {
		clean.TruncatedCandidates += len(candidates) - cliproxyexecutor.MaxRouteDecisionCandidates
		candidates = candidates[:cliproxyexecutor.MaxRouteDecisionCandidates]
	}
	clean.Candidates = make([]cliproxyexecutor.RouteDecisionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = sanitizeRouteDecisionCandidate(candidate)
		if candidate.AuthIndex == "" {
			clean.TruncatedCandidates++
			continue
		}
		clean.Candidates = append(clean.Candidates, candidate)
	}
	clean.Selected = sanitizeRouteDecisionSelection(snapshot.Selected)
	return &clean
}

func sanitizeRouteDecisionCandidate(
	candidate cliproxyexecutor.RouteDecisionCandidate,
) cliproxyexecutor.RouteDecisionCandidate {
	candidate.Provider = sanitizeRouteTraceIdentifier(candidate.Provider, 64)
	candidate.Model = sanitizeRouteTraceIdentifier(candidate.Model, 256)
	candidate.TargetModel = sanitizeRouteTraceIdentifier(candidate.TargetModel, 256)
	candidate.AuthIndex = sanitizeRouteTraceAuthIndex(candidate.AuthIndex)
	if candidate.EffectiveWeight < 0 {
		candidate.EffectiveWeight = 0
	}
	candidate.Native = cloneRouteTraceBool(candidate.Native)
	candidate.Converted = cloneRouteTraceBool(candidate.Converted)
	candidate.Affinity = sanitizeRouteDecisionAffinity(candidate.Affinity)
	credentialEffectKnown := strings.TrimSpace(candidate.Cooldowns.Credential.EffectKey) != ""
	candidate.Cooldowns = cliproxyexecutor.RouteDecisionCooldowns{
		ExactModel:  sanitizeRouteDecisionCooldown(candidate.Cooldowns.ExactModel),
		QuotaFamily: sanitizeRouteDecisionCooldown(candidate.Cooldowns.QuotaFamily),
		Credential:  sanitizeRouteDecisionCooldown(candidate.Cooldowns.Credential),
		Provider:    sanitizeRouteDecisionCooldown(candidate.Cooldowns.Provider),
	}
	if credentialEffectKnown {
		candidate.Cooldowns.Credential.EffectKey = candidate.AuthIndex
	}
	candidate.QuotaStatus = sanitizeRouteDecisionQuotaStatus(candidate.QuotaStatus)
	if candidate.QuotaStatus != "fresh" {
		candidate.QuotaHeadroom = nil
		candidate.QuotaResetAfterMS = nil
	}
	if candidate.QuotaHeadroom != nil && (*candidate.QuotaHeadroom != *candidate.QuotaHeadroom ||
		*candidate.QuotaHeadroom < 0 || *candidate.QuotaHeadroom > 1) {
		candidate.QuotaHeadroom = nil
	} else if candidate.QuotaHeadroom != nil {
		candidate.QuotaHeadroom = new(*candidate.QuotaHeadroom)
	}
	if candidate.QuotaResetAfterMS != nil && *candidate.QuotaResetAfterMS < 0 {
		candidate.QuotaResetAfterMS = nil
	} else if candidate.QuotaResetAfterMS != nil {
		candidate.QuotaResetAfterMS = new(*candidate.QuotaResetAfterMS)
	}
	if candidate.InFlight != nil && *candidate.InFlight < 0 {
		candidate.InFlight = nil
	} else if candidate.InFlight != nil {
		candidate.InFlight = new(*candidate.InFlight)
	}
	candidate.MatchedRules = sanitizeRouteDecisionValues(candidate.MatchedRules, sanitizeRouteDecisionRule)
	candidate.ExcludedReasons = sanitizeRouteDecisionValues(candidate.ExcludedReasons, sanitizeRouteTraceReason)
	return candidate
}

func sanitizeRouteDecisionSelection(
	selection *cliproxyexecutor.RouteDecisionSelection,
) *cliproxyexecutor.RouteDecisionSelection {
	if selection == nil {
		return nil
	}
	clean := *selection
	clean.Provider = sanitizeRouteTraceIdentifier(clean.Provider, 64)
	clean.Model = sanitizeRouteTraceIdentifier(clean.Model, 256)
	clean.TargetModel = sanitizeRouteTraceIdentifier(clean.TargetModel, 256)
	clean.AuthIndex = sanitizeRouteTraceAuthIndex(clean.AuthIndex)
	clean.MatchedRule = sanitizeRouteDecisionRule(clean.MatchedRule)
	if clean.AuthIndex == "" {
		return nil
	}
	return &clean
}

func sanitizeRouteDecisionCooldown(
	cooldown cliproxyexecutor.RouteDecisionCooldown,
) cliproxyexecutor.RouteDecisionCooldown {
	if !cooldown.Known {
		return cliproxyexecutor.RouteDecisionCooldown{}
	}
	cooldown.EffectKey = sanitizeRouteTraceIdentifier(cooldown.EffectKey, 256)
	cooldown.ResetAuthority = sanitizeRouteDecisionResetAuthority(cooldown.ResetAuthority)
	if !cooldown.Active {
		cooldown.Indefinite = false
		cooldown.RetryAfterMS = 0
		cooldown.Reason = ""
		return cooldown
	}
	cooldown.Reason = sanitizeRouteDecisionCooldownReason(cooldown.Reason)
	if cooldown.RetryAfterMS < 0 {
		cooldown.RetryAfterMS = 0
	}
	if cooldown.Indefinite {
		cooldown.RetryAfterMS = 0
	}
	return cooldown
}

func sanitizeRouteDecisionIdentifiers(values []string, maxRunes int) []string {
	return sanitizeRouteDecisionValues(
		values, func(value string) string {
			return sanitizeRouteTraceIdentifier(value, maxRunes)
		},
	)
}

func sanitizeRouteDecisionValues(values []string, sanitize func(string) string) []string {
	if len(values) == 0 {
		return nil
	}
	clean := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = sanitize(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		clean = append(clean, value)
	}
	return clean
}

func sanitizeRouteDecisionStrategy(value string) string {
	switch strings.TrimSpace(value) {
	case "custom", "fill-first", "quota-aware", "round-robin", "weighted-round-robin":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func sanitizeRouteDecisionQuotaStatus(value string) string {
	switch strings.TrimSpace(value) {
	case "fresh", "stale", "unknown":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func sanitizeRouteDecisionAffinity(value string) string {
	switch strings.TrimSpace(value) {
	case "none", "pinned", "session_affinity", "session_affinity_fallback":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func sanitizeRouteDecisionCandidateScope(value string) string {
	switch strings.TrimSpace(value) {
	case "complete", "selected_only":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
}

func sanitizeRouteDecisionRule(value string) string {
	switch strings.TrimSpace(value) {
	case "custom", "fill_first", "highest_priority", "home_dispatch", "model_mapping", "pinned_auth",
		"quota_available_score", "quota_aware", "quota_pool_cursor", "quota_reset_proximity", "quota_weighted_fallback",
		"round_robin", "round_robin_group", "session_affinity", "session_affinity_fallback",
		"transport_preference", "weighted_round_robin":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func sanitizeRouteDecisionCooldownReason(value string) string {
	switch strings.TrimSpace(value) {
	case "credential_unavailable", "disabled", "exact_model_unavailable", "provider_unavailable", "quota_exceeded":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func sanitizeRouteDecisionResetAuthority(value string) string {
	switch strings.TrimSpace(value) {
	case "operator", "reset", "same_generation_success":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func cloneRouteTraceBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	return new(*value)
}

func truncateRouteTraceString(value string, maxRunes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if value == "" || maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}
