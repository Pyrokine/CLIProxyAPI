package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type c09RequestScopedStatusError struct {
	status int
}

func (e c09RequestScopedStatusError) Error() string {
	return `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`
}

func (e c09RequestScopedStatusError) StatusCode() int {
	return e.status
}

func (c09RequestScopedStatusError) IsRequestScoped() bool {
	return true
}

type c09FallbackExecutor struct {
	provider string
	calls    atomic.Int32
}

func (e *c09FallbackExecutor) Identifier() string {
	return e.provider
}

func (e *c09FallbackExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, c09RequestScopedStatusError{status: http.StatusRequestEntityTooLarge}
}

func (e *c09FallbackExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, c09RequestScopedStatusError{status: http.StatusRequestEntityTooLarge}
}

func (e *c09FallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *c09FallbackExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("unexpected CountTokens call")
}

func (e *c09FallbackExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected HttpRequest call")
}

type c09ResultHook struct {
	NoopHook
	results atomic.Int32
}

func (h *c09ResultHook) OnResult(context.Context, Result) {
	h.results.Add(1)
}

func TestC09RequestScopedMessageTooBigStopsCredentialFallback(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		t.Run(
			provider, func(t *testing.T) {
				hook := &c09ResultHook{}
				manager := NewManager(nil, &RoundRobinSelector{}, hook)
				manager.SetRetryConfig(2, 30*time.Second, 0)
				executor := &c09FallbackExecutor{provider: provider}
				manager.RegisterExecutor(executor)
				model := "c09-model"

				for _, id := range []string{"aa-first", "bb-second"} {
					if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: provider}); err != nil {
						t.Fatalf("Register(%s) error = %v", id, err)
					}
					registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}

				result, err := manager.ExecuteStream(
					context.Background(),
					[]string{provider},
					cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)},
					cliproxyexecutor.Options{Stream: true},
				)
				if result != nil {
					t.Fatalf("ExecuteStream() result = %#v, want nil", result)
				}
				if err == nil {
					t.Fatal("ExecuteStream() error = nil")
				}
				statusErr, ok := err.(interface{ StatusCode() int })
				if !ok || statusErr.StatusCode() != http.StatusRequestEntityTooLarge {
					t.Fatalf("ExecuteStream() error = %T %v, want status 413", err, err)
				}
				if got := executor.calls.Load(); got != 1 {
					t.Fatalf("executor calls = %d, want 1 without credential fallback", got)
				}
				if got := hook.results.Load(); got != 1 {
					t.Fatalf("observed results = %d, want 1 availability-neutral failure", got)
				}
				for _, id := range []string{"aa-first", "bb-second"} {
					auth, okAuth := manager.GetByID(id)
					if !okAuth || auth == nil {
						t.Fatalf("GetByID(%s) missing", id)
					}
					if auth.Unavailable || auth.Disabled {
						t.Fatalf("auth %s changed availability after request-scoped error: %+v", id, auth)
					}
				}
			},
		)
	}
}

func TestC09RequestScopedResultDoesNotChangeAuthAvailability(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "request-scoped-auth", Provider: "codex"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	const model = "gpt-5.5"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	resultErr := errorForResult(c09RequestScopedStatusError{status: http.StatusRequestTimeout})
	if resultErr == nil || !resultErr.IsRequestScoped() {
		t.Fatalf("result error = %#v, want request-scoped classification", resultErr)
	}
	manager.MarkResult(
		context.Background(),
		Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: false, Error: resultErr},
	)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("registered auth missing")
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("auth availability changed after request-scoped result: %+v", updated)
	}
	if state := updated.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("model state changed after request-scoped result: %+v", state)
	}

	badRequest := errorForResult(
		&Error{
			HTTPStatus: http.StatusBadRequest,
			Message:    `{"error":{"type":"bad_request_error","message":"bad input"}}`,
		},
	)
	if badRequest == nil || !badRequest.IsRequestScoped() {
		t.Fatalf("bad request result = %#v, want request-scoped classification", badRequest)
	}
}

func TestC09ExplicitSessionBoundariesBeforeAffinity(t *testing.T) {
	oversized := strings.Repeat("x", 257)
	tests := []struct {
		name     string
		headers  http.Header
		body     []byte
		metadata map[string]any
	}{
		{name: "leading control header", headers: http.Header{"X-Session-ID": {"\ttenant"}}},
		{name: "trailing control header", headers: http.Header{"X-Session-ID": {"tenant\n"}}},
		{name: "oversized header", headers: http.Header{"X-Session-ID": {oversized}}},
		{name: "control body", body: []byte(`{"prompt_cache_key":"tenant\n"}`)},
		{name: "oversized body", body: []byte(`{"session_id":"` + oversized + `"}`)},
		{name: "control metadata", metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "tenant\n"}},
		{name: "oversized metadata", metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: oversized}},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if got := ExtractSessionID(test.headers, test.body, test.metadata); got != "" {
					t.Fatalf("ExtractSessionID() = %q, want empty", got)
				}
			},
		)
	}

	headers := http.Header{"X-Session-ID": {"tenant\n", "valid-session"}}
	if got := ExtractSessionID(headers, nil, nil); got != "valid-session" {
		t.Fatalf("ExtractSessionID() = %q, want later valid value", got)
	}
}
