package executor

import (
	"context"
	"net/http"
	"net/url"
	"time"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

// RequestedModelMetadataKey stores the client-requested model name in Options.Metadata.
const RequestedModelMetadataKey = "requested_model"

// ReasoningEffortMetadataKey stores the client-requested reasoning effort for usage logs.
const ReasoningEffortMetadataKey = "reasoning_effort"

// ServiceTierMetadataKey stores the client-requested service tier for usage logs.
const ServiceTierMetadataKey = "service_tier"

// GenerateMetadataKey stores whether the client requested actual generation for usage logs.
const GenerateMetadataKey = "generate"

const (
	// PinnedAuthMetadataKey locks execution to a specific auth ID.
	PinnedAuthMetadataKey = "pinned_auth_id"
	// PinSelectedAuthMetadataKey locks retries to the first auth selected for the request.
	PinSelectedAuthMetadataKey = "pin_selected_auth"
	// SelectedAuthMetadataKey stores the auth ID selected by the scheduler.
	SelectedAuthMetadataKey = "selected_auth_id"
	// SelectedAuthCallbackMetadataKey carries an optional callback invoked with the selected auth ID.
	SelectedAuthCallbackMetadataKey = "selected_auth_callback"
	// SelectedAuthIndexMetadataKey stores the stable index of the auth selected by the scheduler.
	SelectedAuthIndexMetadataKey = "selected_auth_index"
	// SelectedAuthIndexCallbackMetadataKey carries an optional callback invoked with the selected auth index.
	SelectedAuthIndexCallbackMetadataKey = "selected_auth_index_callback"
	// SelectedAuthInfoCallbackMetadataKey carries an optional callback invoked with non-secret selected auth details.
	SelectedAuthInfoCallbackMetadataKey = "selected_auth_info_callback"
	// RouteTraceCallbackMetadataKey carries an optional callback invoked with non-secret routing events.
	RouteTraceCallbackMetadataKey = "route_trace_callback"
	// RouteTraceTruncatedCallbackMetadataKey carries an optional callback invoked for omitted routing events.
	RouteTraceTruncatedCallbackMetadataKey = "route_trace_truncated_callback"
	// ExecutionSessionMetadataKey identifies a long-lived downstream execution session.
	ExecutionSessionMetadataKey = "execution_session_id"
	// DerivedSessionIDMetadataKey stores a stable session identity inferred from request context.
	DerivedSessionIDMetadataKey = "derived_session_id"
	// CallerScopeMetadataKey isolates inferred session identities between downstream callers.
	CallerScopeMetadataKey = "caller_scope"
	// RequestPathMetadataKey stores the inbound HTTP request path (e.g. "/v1/images/generations") in Options.Metadata.
	RequestPathMetadataKey = "request_path"
	// DisallowFreeAuthMetadataKey instructs auth selection to skip known free-tier credentials.
	DisallowFreeAuthMetadataKey = "disallow_free_auth"
	// RequireResponsesContinuationMetadataKey restricts selection to credentials with native Responses continuation.
	RequireResponsesContinuationMetadataKey = "require_responses_continuation"
)

// DispatchPhase identifies the furthest externally observable request boundary.
type DispatchPhase string

const (
	DispatchPhaseNotSent             DispatchPhase = "not_sent"
	DispatchPhaseMaybeSent           DispatchPhase = "maybe_sent"
	DispatchPhaseResponseStarted     DispatchPhase = "response_started"
	DispatchPhaseDownstreamCommitted DispatchPhase = "downstream_committed"
)

// ExecutionOutcome identifies whether upstream processing reached a known terminal state.
type ExecutionOutcome string

const (
	ExecutionOutcomeRejectedBeforeProcessing ExecutionOutcome = "rejected_before_processing"
	ExecutionOutcomeSucceeded                ExecutionOutcome = "succeeded"
	ExecutionOutcomeFailed                   ExecutionOutcome = "failed"
	ExecutionOutcomeUnknown                  ExecutionOutcome = "unknown"
)

// RetryDirective tells the existing conductor which retry transition is safe.
type RetryDirective string

const (
	RetryDirectiveSameAuthAfterRefresh RetryDirective = "same_auth_after_refresh"
	RetryDirectiveNextAuth             RetryDirective = "next_auth"
	RetryDirectiveNextModel            RetryDirective = "next_model"
	RetryDirectiveNextProvider         RetryDirective = "next_provider"
	RetryDirectiveRetryAfter           RetryDirective = "retry_after"
	RetryDirectiveNoRetry              RetryDirective = "no_retry"
)

// StateEffectScope identifies the narrowest runtime state affected by a result.
type StateEffectScope string

const (
	StateEffectScopeRequest     StateEffectScope = "request"
	StateEffectScopeExactModel  StateEffectScope = "exact_model"
	StateEffectScopeQuotaFamily StateEffectScope = "quota_family"
	StateEffectScopeCredential  StateEffectScope = "credential"
	StateEffectScopeGroup       StateEffectScope = "group"
	StateEffectScopeProvider    StateEffectScope = "provider"
)

// ExecutionDisposition carries replay and state-mutation semantics for one attempt.
type ExecutionDisposition struct {
	DispatchPhase  DispatchPhase
	Outcome        ExecutionOutcome
	RetryDirective RetryDirective
	EffectScope    StateEffectScope
	EffectKey      string
}

// Normalize fills omitted fields and preserves the furthest known dispatch phase.
func (d ExecutionDisposition) Normalize(fallbackPhase DispatchPhase) ExecutionDisposition {
	if dispatchPhaseRank(d.DispatchPhase) < dispatchPhaseRank(fallbackPhase) {
		d.DispatchPhase = fallbackPhase
	}
	if d.DispatchPhase == "" {
		d.DispatchPhase = DispatchPhaseMaybeSent
	}
	if d.Outcome == "" {
		d.Outcome = ExecutionOutcomeUnknown
	}
	if d.RetryDirective == "" {
		d.RetryDirective = RetryDirectiveNoRetry
	}
	if d.EffectScope == "" {
		d.EffectScope = StateEffectScopeRequest
	}
	return d
}

func dispatchPhaseRank(phase DispatchPhase) int {
	switch phase {
	case DispatchPhaseNotSent:
		return 1
	case DispatchPhaseMaybeSent:
		return 2
	case DispatchPhaseResponseStarted:
		return 3
	case DispatchPhaseDownstreamCommitted:
		return 4
	default:
		return 0
	}
}

// HTTPStatusDisposition classifies an upstream HTTP response without assuming
// that 5xx or timeout responses are safe to replay.
func HTTPStatusDisposition(statusCode int, hasRetryAfter bool) ExecutionDisposition {
	disposition := ExecutionDisposition{
		DispatchPhase:  DispatchPhaseResponseStarted,
		Outcome:        ExecutionOutcomeUnknown,
		RetryDirective: RetryDirectiveNoRetry,
		EffectScope:    StateEffectScopeProvider,
	}
	switch statusCode {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotAcceptable,
		http.StatusLengthRequired, http.StatusPreconditionFailed, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		disposition.Outcome = ExecutionOutcomeRejectedBeforeProcessing
		disposition.EffectScope = StateEffectScopeRequest
	case http.StatusUnauthorized:
		disposition.Outcome = ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = RetryDirectiveSameAuthAfterRefresh
		disposition.EffectScope = StateEffectScopeCredential
	case http.StatusPaymentRequired, http.StatusForbidden:
		disposition.Outcome = ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = RetryDirectiveNextAuth
		disposition.EffectScope = StateEffectScopeCredential
	case http.StatusNotFound:
		disposition.Outcome = ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = RetryDirectiveNextAuth
		disposition.EffectScope = StateEffectScopeExactModel
	case http.StatusConflict:
		disposition.Outcome = ExecutionOutcomeFailed
		disposition.EffectScope = StateEffectScopeRequest
	case http.StatusTooEarly, http.StatusTooManyRequests:
		disposition.Outcome = ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = RetryDirectiveRetryAfter
		disposition.EffectScope = StateEffectScopeQuotaFamily
	case http.StatusRequestTimeout:
		disposition.Outcome = ExecutionOutcomeUnknown
	case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		disposition.Outcome = ExecutionOutcomeUnknown
	default:
		if statusCode >= 400 && statusCode < 500 {
			disposition.Outcome = ExecutionOutcomeRejectedBeforeProcessing
			disposition.EffectScope = StateEffectScopeRequest
		}
	}
	if hasRetryAfter && disposition.Outcome == ExecutionOutcomeRejectedBeforeProcessing &&
		disposition.RetryDirective == RetryDirectiveNoRetry {
		disposition.RetryDirective = RetryDirectiveRetryAfter
	}
	return disposition
}

// ExecutionDispositionError exposes stable retry and state-effect semantics.
type ExecutionDispositionError interface {
	error
	ExecutionDisposition() ExecutionDisposition
}

// SelectedAuthInfo contains the non-secret selected auth details needed by request handlers.
type SelectedAuthInfo struct {
	ID                                    string
	SupportsIncrementalResponsesWebsocket bool
	SupportsResponsesContinuation         bool
}

const (
	// MaxRouteTraceEvents is the maximum number of route trace events emitted for one request.
	MaxRouteTraceEvents = 256
	// MaxRouteDecisionCandidates is the maximum number of candidates copied into one decision snapshot.
	MaxRouteDecisionCandidates = 128
)

// RouteDecisionCooldown describes one scheduler cooldown scope at decision time.
type RouteDecisionCooldown struct {
	Known           bool   `json:"known"`
	Active          bool   `json:"active"`
	Indefinite      bool   `json:"indefinite,omitempty"`
	RetryAfterMS    int64  `json:"retry_after_ms,omitempty"`
	Reason          string `json:"reason,omitempty"`
	EffectKey       string `json:"effect_key,omitempty"`
	StateGeneration uint64 `json:"state_generation,omitempty"`
	ResetAuthority  string `json:"reset_authority,omitempty"`
}

// RouteDecisionCooldowns contains scheduler state at each supported effect scope.
type RouteDecisionCooldowns struct {
	ExactModel  RouteDecisionCooldown `json:"exact_model"`
	QuotaFamily RouteDecisionCooldown `json:"quota_family"`
	Credential  RouteDecisionCooldown `json:"credential"`
	Provider    RouteDecisionCooldown `json:"provider"`
}

// RouteDecisionCandidate contains one non-secret candidate frozen by the scheduler.
type RouteDecisionCandidate struct {
	Provider          string                 `json:"provider,omitempty"`
	Model             string                 `json:"model,omitempty"`
	TargetModel       string                 `json:"target_model,omitempty"`
	AuthIndex         string                 `json:"auth_index,omitempty"`
	Priority          int                    `json:"priority"`
	EffectiveWeight   int64                  `json:"effective_weight"`
	Native            *bool                  `json:"native"`
	Converted         *bool                  `json:"converted"`
	Affinity          string                 `json:"affinity,omitempty"`
	Cooldowns         RouteDecisionCooldowns `json:"cooldowns"`
	QuotaStatus       string                 `json:"quota_status,omitempty"`
	QuotaHeadroom     *float64               `json:"quota_headroom"`
	QuotaResetAfterMS *int64                 `json:"quota_reset_after_ms"`
	InFlight          *int64                 `json:"in_flight"`
	MatchedRules      []string               `json:"matched_rules,omitempty"`
	ExcludedReasons   []string               `json:"excluded_reasons,omitempty"`
	Selected          bool                   `json:"selected,omitempty"`
}

// RouteDecisionSelection identifies the final candidate selected from a snapshot.
type RouteDecisionSelection struct {
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	TargetModel string `json:"target_model,omitempty"`
	AuthIndex   string `json:"auth_index,omitempty"`
	MatchedRule string `json:"matched_rule,omitempty"`
}

// RouteDecisionSnapshot is an immutable copy of one scheduler decision.
type RouteDecisionSnapshot struct {
	Strategy            string                   `json:"strategy"`
	RequestedModel      string                   `json:"requested_model,omitempty"`
	RoutedModel         string                   `json:"routed_model,omitempty"`
	Providers           []string                 `json:"providers,omitempty"`
	Affinity            string                   `json:"affinity,omitempty"`
	MatchedRule         string                   `json:"matched_rule,omitempty"`
	CandidateScope      string                   `json:"candidate_scope"`
	Candidates          []RouteDecisionCandidate `json:"candidates"`
	Selected            *RouteDecisionSelection  `json:"selected"`
	TruncatedCandidates int                      `json:"truncated_candidates,omitempty"`
}

// RouteTraceEvent contains one non-secret routing decision or execution event.
// Sequence and Timestamp are assigned by the request-scoped collector.
type RouteTraceEvent struct {
	Sequence        int                    `json:"sequence"`
	Timestamp       time.Time              `json:"timestamp"`
	Attempt         int                    `json:"attempt,omitempty"`
	PreviousAttempt int                    `json:"previous_attempt,omitempty"`
	Provider        string                 `json:"provider,omitempty"`
	Model           string                 `json:"model,omitempty"`
	TargetModel     string                 `json:"target_model,omitempty"`
	AuthIndex       string                 `json:"auth_index,omitempty"`
	Action          string                 `json:"action"`
	Reason          string                 `json:"reason,omitempty"`
	ResultCode      string                 `json:"result_code,omitempty"`
	HTTPStatus      int                    `json:"http_status,omitempty"`
	RetryAfterMS    int64                  `json:"retry_after_ms,omitempty"`
	DispatchPhase   DispatchPhase          `json:"dispatch_phase,omitempty"`
	Outcome         ExecutionOutcome       `json:"outcome,omitempty"`
	RetryDirective  RetryDirective         `json:"retry_directive,omitempty"`
	EffectScope     StateEffectScope       `json:"effect_scope,omitempty"`
	EffectKey       string                 `json:"effect_key,omitempty"`
	StateGeneration uint64                 `json:"state_generation,omitempty"`
	FinalTarget     bool                   `json:"final_target,omitempty"`
	Decision        *RouteDecisionSnapshot `json:"decision,omitempty"`
}

// Request encapsulates the translated payload that will be sent to a provider executor.
type Request struct {
	// Model is the upstream model identifier after translation.
	Model string
	// Payload is the provider specific JSON payload.
	Payload []byte
	// Format represents the provider payload schema.
	Format sdktranslator.Format
	// Metadata carries optional provider specific execution hints.
	Metadata map[string]any
}

// RequestAfterAuthInterceptor rewrites a request after credential selection and before executor translation.
type RequestAfterAuthInterceptor func(
	context.Context,
	RequestAfterAuthInterceptRequest,
) RequestAfterAuthInterceptResponse

// RequestAfterAuthInterceptRequest describes a selected-auth request before executor translation.
type RequestAfterAuthInterceptRequest struct {
	SourceFormat    sdktranslator.Format
	ToFormat        sdktranslator.Format
	Model           string
	RequestedModel  string
	Stream          bool
	AuthID          string
	AuthLabel       string
	AuthProvider    string
	OriginalRequest []byte
	Request         Request
	Options         Options
}

// RequestAfterAuthInterceptResponse describes a post-auth interceptor result.
type RequestAfterAuthInterceptResponse struct {
	Request Request
	Options Options
	Handled bool
	// Terminate prevents the selected executor from receiving the request.
	Terminate bool
	// StatusCode is the downstream HTTP status used when Terminate is true.
	StatusCode int
	// ResponseHeaders contains downstream response headers used when Terminate is true.
	ResponseHeaders http.Header
	// ResponseBody contains the downstream response body used when Terminate is true.
	ResponseBody []byte
}

// RequestTerminatedError carries a plugin-defined downstream response without executing upstream.
type RequestTerminatedError struct {
	HTTPStatus int
	Header     http.Header
	Body       []byte
}

func (e *RequestTerminatedError) Error() string {
	return "request terminated by plugin"
}

// StatusCode returns the plugin-defined downstream HTTP status.
func (e *RequestTerminatedError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// ResponseHeaders returns a copy of the plugin-defined downstream headers.
func (e *RequestTerminatedError) ResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	return e.Header.Clone()
}

// ResponseBody returns a copy of the plugin-defined downstream body.
func (e *RequestTerminatedError) ResponseBody() []byte {
	if e == nil {
		return nil
	}
	return append([]byte(nil), e.Body...)
}

// IsRequestScoped prevents auth retries and credential state changes for plugin termination.
func (e *RequestTerminatedError) IsRequestScoped() bool {
	return e != nil
}

// ExecutionDisposition reports that the plugin rejected the request before upstream dispatch.
func (e *RequestTerminatedError) ExecutionDisposition() ExecutionDisposition {
	return ExecutionDisposition{
		DispatchPhase:  DispatchPhaseNotSent,
		Outcome:        ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: RetryDirectiveNoRetry,
		EffectScope:    StateEffectScopeRequest,
	}
}

// Options controls execution behavior for both streaming and non-streaming calls.
type Options struct {
	// Stream toggles streaming mode.
	Stream bool
	// Alt carries optional alternate format hint (e.g. SSE JSON key).
	Alt string
	// Headers are forwarded to the provider request builder.
	Headers http.Header
	// Query contains optional query string parameters.
	Query url.Values
	// OriginalRequest preserves the inbound request bytes prior to translation.
	OriginalRequest []byte
	// SourceFormat identifies the inbound schema.
	SourceFormat sdktranslator.Format
	// ResponseFormat identifies the downstream response schema.
	ResponseFormat sdktranslator.Format
	// Metadata carries extra execution hints shared across selection and executors.
	Metadata map[string]any
	// ExecutionLifecycle owns resources associated with a Home-dispatched execution.
	ExecutionLifecycle ExecutionLifecycle
	// RequestAfterAuthInterceptor runs after credential selection and before executor translation.
	RequestAfterAuthInterceptor RequestAfterAuthInterceptor
}

// ResponseFormatOrSource returns the response target format for an execution.
func ResponseFormatOrSource(opts Options) sdktranslator.Format {
	if opts.ResponseFormat != "" {
		return opts.ResponseFormat
	}
	return opts.SourceFormat
}

// Response wraps either a full provider response or metadata for streaming flows.
type Response struct {
	// Payload is the provider response in the executor format.
	Payload []byte
	// Metadata exposes optional structured data for translators.
	Metadata map[string]any
	// Headers carries upstream HTTP response headers for passthrough to clients.
	Headers http.Header
}

// StreamChunk represents a single streaming payload unit emitted by provider executors.
type StreamChunk struct {
	// Payload is the raw provider chunk payload.
	Payload []byte
	// Err reports any terminal error encountered while producing chunks.
	Err error
	// Terminal marks the transport completion boundary after all upstream payloads.
	Terminal bool
	// Acknowledge reports whether downstream delivery of this chunk succeeded.
	// Executors that provide it must tolerate at most one call.
	Acknowledge func(error)
}

// StreamResult wraps the streaming response, providing both the chunk channel
// and the upstream HTTP response headers captured before streaming begins.
type StreamResult struct {
	// Headers carries upstream HTTP response headers from the initial connection.
	Headers http.Header
	// Chunks is the channel of streaming payload units.
	Chunks <-chan StreamChunk
}

// StatusError represents an error that carries an HTTP-like status code.
// Provider executors should implement this when possible to enable
// better auth state updates on failures (e.g., 401/402/429).
type StatusError interface {
	error
	StatusCode() int
}

// RequestScopedError identifies a failure tied to the current request rather
// than the selected credential. Auth managers should not retry these errors
// across credentials or change credential availability because of them.
type RequestScopedError interface {
	error
	IsRequestScoped() bool
}
