package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/google/uuid"
)

type claudeOAuthCancellationTestExecutor struct {
	prepareFn func(context.Context, *Auth) (*Auth, error)
	executeFn func(context.Context, *Auth) (cliproxyexecutor.Response, error)
	countFn   func(context.Context, *Auth) (cliproxyexecutor.Response, error)
	streamFn  func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error)
	refreshFn func(context.Context, *Auth) (*Auth, error)

	prepareCalls atomic.Int32
	executeCalls atomic.Int32
	countCalls   atomic.Int32
	streamCalls  atomic.Int32
	refreshCalls atomic.Int32
}

func (*claudeOAuthCancellationTestExecutor) Identifier() string {
	return "claude"
}

func (e *claudeOAuthCancellationTestExecutor) ShouldPrepareRequestAuth(*Auth) bool {
	return e.prepareFn != nil
}

func (e *claudeOAuthCancellationTestExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	e.prepareCalls.Add(1)
	return e.prepareFn(ctx, auth)
}

func (e *claudeOAuthCancellationTestExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executeCalls.Add(1)
	if e.executeFn != nil {
		return e.executeFn(ctx, auth)
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *claudeOAuthCancellationTestExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.countCalls.Add(1)
	if e.countFn != nil {
		return e.countFn(ctx, auth)
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *claudeOAuthCancellationTestExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.streamCalls.Add(1)
	if e.streamFn != nil {
		return e.streamFn(ctx, auth)
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *claudeOAuthCancellationTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	if e.refreshFn != nil {
		return e.refreshFn(ctx, auth)
	}
	return auth, nil
}

func (*claudeOAuthCancellationTestExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type claudeOAuthCancellationTestHook struct {
	NoopHook
	mu      sync.Mutex
	results []Result
}

func (h *claudeOAuthCancellationTestHook) OnResult(_ context.Context, result Result) {
	h.mu.Lock()
	h.results = append(h.results, result)
	h.mu.Unlock()
}

func (h *claudeOAuthCancellationTestHook) snapshot() []Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Result(nil), h.results...)
}

type claudeOAuthManagerCancellationError struct{}

func (claudeOAuthManagerCancellationError) Error() string {
	return context.Canceled.Error()
}

func (claudeOAuthManagerCancellationError) Unwrap() error {
	return context.Canceled
}

func (claudeOAuthManagerCancellationError) IsRequestScoped() bool {
	return true
}

func registerClaudeOAuthCancellationTestAuths(
	t *testing.T,
	manager *Manager,
	count int,
) (model string, ids []string) {
	t.Helper()
	model = "claude-oauth-cancellation-" + uuid.NewString()
	ids = make([]string, 0, count)
	for index := range count {
		id := fmt.Sprintf("claude-oauth-cancellation-%s-%d", uuid.NewString(), index)
		auth := &Auth{
			ID:       id,
			Provider: "claude",
			Attributes: map[string]string{
				"auth_kind": "oauth",
			},
			Metadata: map[string]any{
				"access_token":  "access-token",
				"refresh_token": "refresh-token",
				"request_retry": float64(0),
			},
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		idForCleanup := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(idForCleanup) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
		ids = append(ids, id)
	}
	return model, ids
}

func assertClaudeOAuthCancellationAuthsAvailable(t *testing.T, manager *Manager, ids []string, model string) {
	t.Helper()
	for _, id := range ids {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil {
			t.Fatalf("GetByID(%s) missing", id)
		}
		if auth.Unavailable || auth.Disabled || auth.Status == StatusError || !auth.NextRetryAfter.IsZero() {
			t.Fatalf("auth %s changed availability after cancellation: %+v", id, auth)
		}
		if state := auth.ModelStates[model]; state != nil &&
			(state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded) {
			t.Fatalf("model %s changed availability after cancellation: %+v", model, state)
		}
	}
}

func TestClaudeOAuthPrepareCancellationStopsFallbackWithoutRecordingFailure(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Manager, string) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errExecute := manager.Execute(
					ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "count tokens",
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errCount := manager.ExecuteCount(
					ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
				)
				return errCount
			},
		},
		{
			name: "stream",
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errStream := manager.ExecuteStream(
					ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model},
					cliproxyexecutor.Options{Stream: true},
				)
				return errStream
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				hook := &claudeOAuthCancellationTestHook{}
				manager := NewManager(nil, &RoundRobinSelector{}, hook)
				manager.SetRetryConfig(2, 30*time.Second, 2)
				ctx, cancel := context.WithCancel(context.Background())
				executor := &claudeOAuthCancellationTestExecutor{}
				executor.prepareFn = func(ctx context.Context, auth *Auth) (*Auth, error) {
					cancel()
					return auth, ctx.Err()
				}
				manager.RegisterExecutor(executor)
				model, ids := registerClaudeOAuthCancellationTestAuths(t, manager, 2)

				errExecute := test.run(ctx, manager, model)
				if !errors.Is(errExecute, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", errExecute)
				}
				if got := executor.prepareCalls.Load(); got != 1 {
					t.Fatalf("PrepareRequestAuth calls = %d, want 1 without credential fallback", got)
				}
				if calls := executor.executeCalls.Load() + executor.countCalls.Load() + executor.streamCalls.Load(); calls != 0 {
					t.Fatalf("executor calls = %d, want 0", calls)
				}
				if results := hook.snapshot(); len(results) != 0 {
					t.Fatalf("recorded results = %+v, want none", results)
				}
				assertClaudeOAuthCancellationAuthsAvailable(t, manager, ids, model)
			},
		)
	}
}

func TestClaudeOAuthRefreshCancellationStopsWithoutCooldown(t *testing.T) {
	unauthorized := &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
	tests := []struct {
		name      string
		configure func(*claudeOAuthCancellationTestExecutor)
		run       func(context.Context, *Manager, string) error
	}{
		{
			name: "execute",
			configure: func(executor *claudeOAuthCancellationTestExecutor) {
				executor.executeFn = func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
					return cliproxyexecutor.Response{}, unauthorized
				}
			},
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errExecute := manager.Execute(
					ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "count tokens",
			configure: func(executor *claudeOAuthCancellationTestExecutor) {
				executor.countFn = func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
					return cliproxyexecutor.Response{}, unauthorized
				}
			},
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errCount := manager.ExecuteCount(
					ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
				)
				return errCount
			},
		},
		{
			name: "stream",
			configure: func(executor *claudeOAuthCancellationTestExecutor) {
				executor.streamFn = func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
					return nil, unauthorized
				}
			},
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errStream := manager.ExecuteStream(
					ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model},
					cliproxyexecutor.Options{Stream: true},
				)
				return errStream
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				hook := &claudeOAuthCancellationTestHook{}
				manager := NewManager(nil, &RoundRobinSelector{}, hook)
				manager.SetRetryConfig(0, 0, 0)
				ctx, cancel := context.WithCancel(context.Background())
				executor := &claudeOAuthCancellationTestExecutor{}
				test.configure(executor)
				executor.refreshFn = func(ctx context.Context, _ *Auth) (*Auth, error) {
					cancel()
					return nil, ctx.Err()
				}
				manager.RegisterExecutor(executor)
				model, ids := registerClaudeOAuthCancellationTestAuths(t, manager, 1)
				registered, _ := manager.GetByID(ids[0])
				if !authHasRefreshCredential(registered) || !isUnauthorizedError(unauthorized) {
					t.Fatalf("refresh preconditions missing: auth=%+v unauthorized=%v", registered, unauthorized)
				}

				errExecute := test.run(ctx, manager, model)
				if !errors.Is(errExecute, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", errExecute)
				}
				if got := executor.refreshCalls.Load(); got != 1 {
					t.Fatalf("Refresh calls = %d, want 1", got)
				}
				if calls := executor.executeCalls.Load() + executor.countCalls.Load() + executor.streamCalls.Load(); calls != 1 {
					t.Fatalf("upstream calls = %d, want 1", calls)
				}
				if results := hook.snapshot(); len(results) != 0 {
					t.Fatalf("recorded results = %+v, want none", results)
				}
				assertClaudeOAuthCancellationAuthsAvailable(t, manager, ids, model)
			},
		)
	}
}

func TestClaudeOAuthDownstreamStreamCancellationDoesNotRecordSuccess(t *testing.T) {
	hook := &claudeOAuthCancellationTestHook{}
	manager := NewManager(nil, &RoundRobinSelector{}, hook)
	executor := &claudeOAuthCancellationTestExecutor{}
	executor.streamFn = func(ctx context.Context, _ *Auth) (*cliproxyexecutor.StreamResult, error) {
		chunks := make(chan cliproxyexecutor.StreamChunk)
		go func() {
			defer close(chunks)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: first\n\n")}
			<-ctx.Done()
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	manager.RegisterExecutor(executor)
	model, ids := registerClaudeOAuthCancellationTestAuths(t, manager, 1)

	ctx, cancel := context.WithCancel(context.Background())
	result, errStream := manager.ExecuteStream(
		ctx,
		[]string{"claude"},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Stream: true},
	)
	if errStream != nil {
		cancel()
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	select {
	case chunk := <-result.Chunks:
		if string(chunk.Payload) != "data: first\n\n" || chunk.Err != nil {
			cancel()
			t.Fatalf("first chunk = %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for first stream chunk")
	}
	cancel()
	for range result.Chunks {
	}

	if results := hook.snapshot(); len(results) != 0 {
		t.Fatalf("recorded results = %+v, want no success or failure after downstream cancellation", results)
	}
	assertClaudeOAuthCancellationAuthsAvailable(t, manager, ids, model)
}

func TestClaudeOAuthRequestScopedStreamCancellationKeepsCredentialAvailable(t *testing.T) {
	source := make(chan cliproxyexecutor.StreamChunk, 1)
	source <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
	executor := &claudeOAuthCancellationTestExecutor{
		streamFn: func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
			return &cliproxyexecutor.StreamResult{Chunks: source}, nil
		},
	}
	hook := &claudeOAuthCancellationTestHook{}
	manager := NewManager(nil, &RoundRobinSelector{}, hook)
	manager.RegisterExecutor(executor)
	model, ids := registerClaudeOAuthCancellationTestAuths(t, manager, 1)
	ctx, cancel := context.WithCancel(context.Background())

	stream, errStream := manager.ExecuteStream(
		ctx,
		[]string{"claude"},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Stream: true},
	)
	if errStream != nil {
		cancel()
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if chunk := <-stream.Chunks; chunk.Err != nil || string(chunk.Payload) != "first" {
		cancel()
		t.Fatalf("first chunk = %#v", chunk)
	}
	cancel()
	source <- cliproxyexecutor.StreamChunk{Err: claudeOAuthManagerCancellationError{}}
	close(source)
	for range stream.Chunks {
	}

	results := hook.snapshot()
	if len(results) != 1 || results[0].Success || results[0].Error == nil {
		t.Fatalf("results = %#v, want one failed cancellation result", results)
	}
	if !results[0].Error.IsRequestScoped() || results[0].Error.StatusCode() != 0 {
		t.Fatalf("cancellation result = %#v, want request-scoped status 0", results[0].Error)
	}
	assertClaudeOAuthCancellationAuthsAvailable(t, manager, ids, model)
}

func TestClaudeOAuthUnknownUpstreamFailureDoesNotCoolCredential(t *testing.T) {
	executor := &claudeOAuthCancellationTestExecutor{
		executeFn: func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, &Error{
				HTTPStatus: http.StatusInternalServerError, Message: "upstream failure",
			}
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	model, ids := registerClaudeOAuthCancellationTestAuths(t, manager, 1)

	_, errExecute := manager.Execute(
		context.Background(),
		[]string{"claude"},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{},
	)
	if statusCodeFromError(errExecute) != http.StatusInternalServerError {
		t.Fatalf("Execute() error = %v, want HTTP 500", errExecute)
	}
	auth, _ := manager.GetByID(ids[0])
	state := auth.ModelStates[model]
	if state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("unknown upstream failure changed model availability: %#v", state)
	}
	if auth.Unavailable || !auth.NextRetryAfter.IsZero() {
		t.Fatalf("unknown upstream failure changed credential availability: %#v", auth)
	}
}

func TestClaudeOAuthRequestCancellationExcludesOtherAuthKinds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []*Auth{
		{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}},
		{Provider: "claude", Attributes: map[string]string{"auth_kind": "api_key"}},
	}
	for _, auth := range tests {
		if errCancel := claudeOAuthRequestCancellation(ctx, auth, context.Canceled); errCancel != nil {
			t.Fatalf("auth %#v was classified as Claude OAuth cancellation: %v", auth.Attributes, errCancel)
		}
	}
}
