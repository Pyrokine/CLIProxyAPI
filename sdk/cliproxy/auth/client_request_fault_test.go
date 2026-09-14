package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestFaultStatusError struct {
	status int
	body   string
}

func (e requestFaultStatusError) Error() string   { return e.body }
func (e requestFaultStatusError) StatusCode() int { return e.status }

type requestFaultExecutor struct {
	provider           string
	err                error
	streamAfterPayload bool
	calls              atomic.Int32
}

func (e *requestFaultExecutor) Identifier() string { return e.provider }

func (e *requestFaultExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, e.err
}

func (e *requestFaultExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	if e.streamAfterPayload {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("payload")}
	}
	chunks <- cliproxyexecutor.StreamChunk{Err: e.err}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *requestFaultExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *requestFaultExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected CountTokens call")
}

func (e *requestFaultExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HttpRequest call")
}

func TestManagerRequestFaultStopsCredentialFallbackWithoutCooldown(t *testing.T) {
	tests := []struct {
		name               string
		stream             bool
		streamAfterPayload bool
		err                error
	}{
		{
			name: "plain bad request",
			err:  requestFaultStatusError{status: http.StatusBadRequest, body: "bad request"},
		},
		{
			name:   "conflict stream bootstrap",
			stream: true,
			err:    requestFaultStatusError{status: http.StatusConflict, body: "conflict"},
		},
		{
			name: "message too big",
			err: requestFaultStatusError{
				status: http.StatusRequestEntityTooLarge,
				body:   `{"error":{"code":"message_too_big","message":"too large"}}`,
			},
		},
		{
			name:   "unprocessable stream bootstrap",
			stream: true,
			err:    requestFaultStatusError{status: http.StatusUnprocessableEntity, body: "unprocessable"},
		},
		{
			name: "cyber policy behind bad gateway",
			err: requestFaultStatusError{
				status: http.StatusBadGateway,
				body:   `{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`,
			},
		},
		{
			name:   "context length behind internal error",
			stream: true,
			err: requestFaultStatusError{
				status: http.StatusInternalServerError,
				body:   `{"response":{"error":{"type":"server_error","code":"context_length_exceeded"}}}`,
			},
		},
		{
			name:   "invalid request type behind bad gateway",
			stream: true,
			err: requestFaultStatusError{
				status: http.StatusBadGateway,
				body:   `{"body":{"error":{"type":"invalid_request","message":"invalid"}}}`,
			},
		},
		{
			name:               "conflict after stream payload",
			stream:             true,
			streamAfterPayload: true,
			err:                requestFaultStatusError{status: http.StatusConflict, body: "conflict"},
		},
	}

	for index, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				provider := fmt.Sprintf("request-fault-provider-%d", index)
				model := fmt.Sprintf("request-fault-model-%d", index)
				executor := &requestFaultExecutor{
					provider:           provider,
					err:                test.err,
					streamAfterPayload: test.streamAfterPayload,
				}
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetRetryConfig(2, 30*time.Second, 0)
				manager.RegisterExecutor(executor)

				for _, suffix := range []string{"a", "b"} {
					authID := model + "-" + suffix
					if _, errRegister := manager.Register(
						context.Background(),
						&Auth{ID: authID, Provider: provider, Status: StatusActive},
					); errRegister != nil {
						t.Fatalf("register %s: %v", authID, errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(
						authID,
						provider,
						[]*registry.ModelInfo{{ID: model}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
				}

				var errExecute error
				if test.stream {
					result, errStream := manager.ExecuteStream(
						context.Background(),
						[]string{provider},
						cliproxyexecutor.Request{Model: model},
						cliproxyexecutor.Options{Stream: true},
					)
					errExecute = errStream
					if result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								errExecute = chunk.Err
							}
						}
					}
				} else {
					_, errExecute = manager.Execute(
						context.Background(),
						[]string{provider},
						cliproxyexecutor.Request{Model: model},
						cliproxyexecutor.Options{},
					)
				}
				if errExecute == nil {
					t.Fatal("execution error = nil")
				}
				if got := executor.calls.Load(); got != 1 {
					t.Fatalf("executor calls = %d, want 1", got)
				}

				failed := int64(0)
				for _, suffix := range []string{"a", "b"} {
					authID := model + "-" + suffix
					auth, ok := manager.GetByID(authID)
					if !ok || auth == nil {
						t.Fatalf("registered auth %s missing", authID)
					}
					failed += auth.Failed
					if auth.Unavailable || !auth.NextRetryAfter.IsZero() {
						t.Fatalf("auth %s was penalized: %+v", authID, auth)
					}
					if state := auth.ModelStates[model]; state != nil &&
						(state.Unavailable || !state.NextRetryAfter.IsZero()) {
						t.Fatalf("model state for %s was penalized: %+v", authID, state)
					}
				}
				if failed != 1 {
					t.Fatalf("failed request count = %d, want 1", failed)
				}
			},
		)
	}
}

func TestIsRequestInvalidErrorKeepsCredentialAndModelFailuresDistinct(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "unauthorized",
			err:  requestFaultStatusError{status: http.StatusUnauthorized, body: "invalid token"},
		},
		{
			name: "quota",
			err:  requestFaultStatusError{status: http.StatusTooManyRequests, body: "quota exhausted"},
		},
		{
			name: "transport",
			err:  requestFaultStatusError{status: http.StatusBadGateway, body: "unexpected EOF"},
		},
		{
			name: "model support",
			err: requestFaultStatusError{
				status: http.StatusBadRequest,
				body:   "invalid_request_error: requested model is not supported",
			},
		},
		{
			name: "invalid grant",
			err:  requestFaultStatusError{status: http.StatusBadRequest, body: "invalid_grant"},
		},
		{
			name: "cloudflare challenge",
			err: requestFaultStatusError{
				status: http.StatusBadRequest,
				body:   "invalid_request_error: cloudflare <html challenge-platform",
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if isRequestInvalidError(test.err) {
					t.Fatalf("%s was classified as a request fault", test.name)
				}
			},
		)
	}
}
