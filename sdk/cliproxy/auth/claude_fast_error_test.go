package auth

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type claudeFastDirectResponseTestError struct {
	response *cliproxyexecutor.RequestTerminatedError
}

func (*claudeFastDirectResponseTestError) Error() string {
	return "Fast upstream request failed"
}

func (e *claudeFastDirectResponseTestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.response
}

func (e *claudeFastDirectResponseTestError) IsRequestScoped() bool {
	return e != nil
}

func newClaudeFastDirectResponseTestError(status int, body string) error {
	return &claudeFastDirectResponseTestError{
		response: &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       []byte(body),
		},
	}
}

func TestManagerClaudeFastDirectErrorDoesNotRefreshRetryOrCoolCredential(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*claudeOAuthCancellationTestExecutor, *atomic.Int32)
		run       func(*Manager, string) error
	}{
		{
			name: "non-stream",
			configure: func(executor *claudeOAuthCancellationTestExecutor, calls *atomic.Int32) {
				executor.executeFn = func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
					if calls.Add(1) == 1 {
						return cliproxyexecutor.Response{}, newClaudeFastDirectResponseTestError(
							http.StatusUnauthorized,
							`{"type":"error","error":{"message":"Fast denied"}}`,
						)
					}
					return cliproxyexecutor.Response{Payload: []byte(`{"type":"message","content":[]}`)}, nil
				}
			},
			run: func(manager *Manager, model string) error {
				_, errExecute := manager.Execute(
					context.Background(),
					[]string{"claude"},
					cliproxyexecutor.Request{Model: model},
					cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "stream",
			configure: func(executor *claudeOAuthCancellationTestExecutor, calls *atomic.Int32) {
				executor.streamFn = func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
					if calls.Add(1) == 1 {
						return nil, newClaudeFastDirectResponseTestError(
							http.StatusUnauthorized,
							`{"type":"error","error":{"message":"Fast denied"}}`,
						)
					}
					chunks := make(chan cliproxyexecutor.StreamChunk, 1)
					chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}
			},
			run: func(manager *Manager, model string) error {
				stream, errStream := manager.ExecuteStream(
					context.Background(),
					[]string{"claude"},
					cliproxyexecutor.Request{Model: model},
					cliproxyexecutor.Options{Stream: true},
				)
				if errStream != nil {
					return errStream
				}
				for range stream.Chunks {
				}
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				var calls atomic.Int32
				executor := &claudeOAuthCancellationTestExecutor{}
				test.configure(executor, &calls)
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetRetryConfig(2, 0, 2)
				manager.RegisterExecutor(executor)
				model, ids := registerClaudeOAuthCancellationTestAuths(t, manager, 2)

				errExecute := test.run(manager, model)
				if errExecute == nil {
					t.Fatal("first Fast request error = nil")
				}
				var direct *cliproxyexecutor.RequestTerminatedError
				if !errors.As(errExecute, &direct) || direct == nil {
					t.Fatalf("first error = %T %v, want direct response", errExecute, errExecute)
				}
				if got := direct.StatusCode(); got != http.StatusUnauthorized {
					t.Fatalf("direct status = %d, want 401", got)
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("first request upstream calls = %d, want 1", got)
				}
				if got := executor.refreshCalls.Load(); got != 0 {
					t.Fatalf("refresh calls = %d, want 0", got)
				}
				assertClaudeOAuthCancellationAuthsAvailable(t, manager, ids, model)

				if errFollowUp := test.run(manager, model); errFollowUp != nil {
					t.Fatalf("follow-up request error = %v", errFollowUp)
				}
				if got := calls.Load(); got != 2 {
					t.Fatalf("total upstream calls = %d, want 2", got)
				}
				assertClaudeOAuthCancellationAuthsAvailable(t, manager, ids, model)
			},
		)
	}
}
