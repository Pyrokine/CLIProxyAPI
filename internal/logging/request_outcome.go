package logging

import (
	"context"
	"strings"
	"sync"
)

// RequestOutcomeKind classifies the semantic result of one proxied request.
type RequestOutcomeKind string

const (
	requestOutcomeSuccess        RequestOutcomeKind = "success"
	RequestOutcomeClientError    RequestOutcomeKind = "client_error"
	RequestOutcomeUpstreamError  RequestOutcomeKind = "upstream_error"
	RequestOutcomeServerError    RequestOutcomeKind = "server_error"
	RequestOutcomeCanceled       RequestOutcomeKind = "canceled"
	RequestOutcomeDeadline       RequestOutcomeKind = "deadline"
	RequestOutcomeStreamError    RequestOutcomeKind = "stream_error"
	RequestOutcomeWebsocketError RequestOutcomeKind = "websocket_error"
	RequestOutcomePanic          RequestOutcomeKind = "panic"
)

// RequestOutcome is a point-in-time snapshot of the semantic request result.
type RequestOutcome struct {
	Failed bool
	Kind   RequestOutcomeKind
	Stage  string
	Reason string
}

// RequestOutcomeState is shared by every context derived from one HTTP request.
type RequestOutcomeState struct {
	mu      sync.RWMutex
	outcome RequestOutcome
}

type requestOutcomeStateContextKey struct{}

// NewRequestOutcomeState creates a success outcome that may later be marked failed.
func NewRequestOutcomeState() *RequestOutcomeState {
	return &RequestOutcomeState{
		outcome: RequestOutcome{Kind: requestOutcomeSuccess},
	}
}

// WithRequestOutcomeState attaches a shared outcome state to ctx.
func WithRequestOutcomeState(ctx context.Context, state *RequestOutcomeState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if state == nil {
		return ctx
	}
	return context.WithValue(ctx, requestOutcomeStateContextKey{}, state)
}

// RequestOutcomeStateFromContext returns the shared outcome state attached to ctx.
func RequestOutcomeStateFromContext(ctx context.Context) *RequestOutcomeState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(requestOutcomeStateContextKey{}).(*RequestOutcomeState)
	return state
}

// MarkRequestOutcomeFailure marks the outcome state carried by ctx, if present.
func MarkRequestOutcomeFailure(
	ctx context.Context,
	kind RequestOutcomeKind,
	stage string,
	reason string,
) {
	if state := RequestOutcomeStateFromContext(ctx); state != nil {
		state.MarkFailure(kind, stage, reason)
	}
}

// MarkFailure records a failure. A later, more specific failure may replace a
// generic HTTP failure, while later generic observations cannot erase details.
func (s *RequestOutcomeState) MarkFailure(
	kind RequestOutcomeKind,
	stage string,
	reason string,
) {
	if s == nil {
		return
	}
	if kind == "" || kind == requestOutcomeSuccess {
		kind = RequestOutcomeServerError
	}
	incoming := RequestOutcome{
		Failed: true,
		Kind:   kind,
		Stage:  strings.TrimSpace(stage),
		Reason: strings.TrimSpace(reason),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.outcome.Failed || requestOutcomePriority(incoming.Kind) > requestOutcomePriority(s.outcome.Kind) {
		s.outcome = incoming
		return
	}
	if incoming.Kind != s.outcome.Kind {
		return
	}
	if s.outcome.Stage == "" {
		s.outcome.Stage = incoming.Stage
	}
	if s.outcome.Reason == "" {
		s.outcome.Reason = incoming.Reason
	}
}

// Snapshot returns a copy safe for callers to inspect without holding a lock.
func (s *RequestOutcomeState) Snapshot() RequestOutcome {
	if s == nil {
		return RequestOutcome{Kind: requestOutcomeSuccess}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outcome
}

func requestOutcomePriority(kind RequestOutcomeKind) int {
	switch kind {
	case RequestOutcomePanic:
		return 100
	case RequestOutcomeDeadline, RequestOutcomeCanceled:
		return 90
	case RequestOutcomeWebsocketError, RequestOutcomeStreamError:
		return 80
	case RequestOutcomeUpstreamError:
		return 70
	case RequestOutcomeClientError, RequestOutcomeServerError:
		return 50
	default:
		return 0
	}
}
