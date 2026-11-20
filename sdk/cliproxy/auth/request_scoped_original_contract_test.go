package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManager_RequestScopedErrorStopsCredentialFallbackWithoutSuspendingAuth(t *testing.T) {
	incomplete := NewRequestScopedError(
		"stream error: stream disconnected before completion: stream closed before response.completed",
		http.StatusRequestTimeout,
	)
	tooBig := NewRequestScopedError(
		`{"error":{"message":"upstream websocket message too big","type":`+
			`"invalid_request_error","code":`+
			`"message_too_big"}}`,
		http.StatusRequestEntityTooLarge,
	)
	invalid := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid input."}}`,
	}
	badRequest := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"bad_request_error","code":"invalid_value","message":"Bad input."}}`,
	}
	for _, test := range []struct {
		name, provider    string
		stream, afterData bool
		err               error
		wantStatus        int
	}{
		{"non-streaming incomplete", "codex", false, false, incomplete, http.StatusRequestTimeout},
		{"streaming incomplete", "codex", true, false, incomplete, http.StatusRequestTimeout},
		{"streaming codex websocket message too big", "codex", true, false, tooBig, http.StatusRequestEntityTooLarge},
		{"streaming xai websocket message too big", "xai", true, false, tooBig, http.StatusRequestEntityTooLarge},
		{"non-streaming invalid request", "codex", false, false, invalid, http.StatusBadRequest},
		{"streaming invalid request", "codex", true, false, invalid, http.StatusBadRequest},
		{"non-streaming bad request", "codex", false, false, badRequest, http.StatusBadRequest},
		{"streaming bad request", "codex", true, false, badRequest, http.StatusBadRequest},
		{
			"streaming cyber policy", "codex", true, false, &Error{
				HTTPStatus: http.StatusBadGateway,
				Message: `{"error":{"type":"invalid_request","code":"cyber_policy","message":"Th` +
					`is content was flagged for possible cybersecurity risk."}}`,
			}, http.StatusBadGateway,
		},
		{
			"non-streaming message too big", "codex", false, false, &Error{
				HTTPStatus: http.StatusRequestEntityTooLarge,
				Message:    `{"error":{"code":"message_too_big","message":"upstream websocket message too big"}}`,
			}, http.StatusRequestEntityTooLarge,
		},
		{
			"streaming message too big", "codex", true, false, &Error{
				HTTPStatus: http.StatusRequestEntityTooLarge,
				Message:    `{"error":{"code":"message_too_big","message":"upstream websocket message too big"}}`,
			}, http.StatusRequestEntityTooLarge,
		},
		{
			"non-streaming plain bad request", "codex", false, false,
			&Error{HTTPStatus: http.StatusBadRequest, Message: "bad request"}, http.StatusBadRequest,
		},
		{
			"streaming plain bad request", "codex", true, false,
			&Error{HTTPStatus: http.StatusBadRequest, Message: "bad request"}, http.StatusBadRequest,
		},
		{
			"non-streaming conflict", "codex", false, false, &Error{
				HTTPStatus: http.StatusConflict,
				Message:    `{"error":{"type":"conflict_error","code":"conflict","message":"request conflict"}}`,
			}, http.StatusConflict,
		},
		{
			"streaming conflict", "codex", true, false, &Error{
				HTTPStatus: http.StatusConflict,
				Message:    `{"error":{"type":"conflict_error","code":"conflict","message":"request conflict"}}`,
			}, http.StatusConflict,
		},
		{
			"streaming conflict after payload", "codex", true, true, &Error{
				HTTPStatus: http.StatusConflict,
				Message:    `{"error":{"type":"conflict_error","code":"conflict","message":"request conflict"}}`,
			}, http.StatusConflict,
		},
		{
			"non-streaming context length behind bad gateway", "codex", false, false, &Error{
				HTTPStatus: http.StatusBadGateway,
				Message: `{"error":{"type":"server_error","code":"context_length_exceeded",` +
					`"message":"input too ` +
					`long"}}`,
			}, http.StatusBadGateway,
		},
		{
			"streaming context length behind bad gateway", "codex", true, false, &Error{
				HTTPStatus: http.StatusBadGateway,
				Message: `{"error":{"type":"server_error","code":"context_length_exceeded",` +
					`"message":"input too ` +
					`long"}}`,
			}, http.StatusBadGateway,
		},
		{
			"streaming invalid request type behind bad gateway", "codex", true, false, &Error{
				HTTPStatus: http.StatusBadGateway,
				Message:    `{"body":{"error":{"type":"invalid_request","message":"invalid input"}}}`,
			}, http.StatusBadGateway,
		},
		{
			"non-streaming item not persisted", "codex", false, false,
			&Error{HTTPStatus: http.StatusNotFound, Message: originalOverrideItemNotPersistedMessage},
			http.StatusNotFound,
		},
		{
			"streaming item not persisted", "codex", true, false,
			&Error{HTTPStatus: http.StatusNotFound, Message: originalOverrideItemNotPersistedMessage},
			http.StatusNotFound,
		},
		{
			"streaming item not persisted after payload", "codex", true, true,
			&Error{HTTPStatus: http.StatusNotFound, Message: originalOverrideItemNotPersistedMessage},
			http.StatusNotFound,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetRetryConfig(2, 30*time.Second, 0)
				executor := &routeTraceTestExecutor{provider: test.provider}
				executor.executeFn = func(auth *Auth) (core.Response, error) {
					if auth.ID == "aa-bad-auth" {
						return core.Response{}, test.err
					}
					return core.Response{Payload: []byte("ok")}, nil
				}
				executor.streamFn = func(auth *Auth) (*core.StreamResult, error) {
					chunks := make(chan core.StreamChunk, 2)
					if auth.ID != "aa-bad-auth" || test.afterData {
						chunks <- core.StreamChunk{Payload: []byte(auth.ID)}
					}
					if auth.ID == "aa-bad-auth" {
						chunks <- core.StreamChunk{Err: test.err}
					}
					close(chunks)
					return &core.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: chunks}, nil
				}
				manager.RegisterExecutor(executor)
				const model = "gpt-5.5"
				for _, id := range []string{"aa-bad-auth", "bb-good-auth"} {
					registry.GetGlobalRegistry().
						RegisterClient(id, test.provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, err := manager.Register(
						context.Background(), &Auth{ID: id, Provider: test.provider},
					); err != nil {
						t.Fatal(err)
					}
				}
				var executionErr error
				var calls []string
				if test.stream {
					result, err := manager.ExecuteStream(
						context.Background(), []string{test.provider},
						core.Request{Model: model}, core.Options{Stream: true},
					)
					executionErr = err
					if result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								executionErr = chunk.Err
							}
						}
					}
					calls = executor.StreamCalls()
				} else {
					_, executionErr = manager.Execute(
						context.Background(), []string{test.provider},
						core.Request{Model: model}, core.Options{},
					)
					calls = executor.ExecuteCalls()
				}
				if executionErr == nil || statusCodeFromError(executionErr) != test.wantStatus {
					t.Fatalf("request-scoped error = %v, want status %d", executionErr, test.wantStatus)
				}
				if len(calls) != 1 || calls[0] != "aa-bad-auth" {
					t.Fatalf("credential calls = %v, want [aa-bad-auth]", calls)
				}
				updated, ok := manager.GetByID("aa-bad-auth")
				if !ok || updated == nil || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
					t.Fatalf("request-scoped error changed credential availability: %+v", updated)
				}
				if state := updated.ModelStates[model]; state != nil {
					t.Fatalf("request-scoped error created a model cooldown: %+v", state)
				}
				available, _ := manager.GetByID("bb-good-auth")
				if updated.Failed != 1 || available == nil || available.Failed != 0 {
					t.Fatalf("request failure statistics: attempted=%+v unattempted=%+v", updated, available)
				}
			},
		)
	}
}
