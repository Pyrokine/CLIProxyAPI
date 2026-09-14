package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const c08HomeRefreshProvider = "c08-home-refresh"

type c08HomeRefreshDispatcher struct {
	calls atomic.Int32
}

func (*c08HomeRefreshDispatcher) HeartbeatOK() bool { return true }

func (d *c08HomeRefreshDispatcher) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	d.calls.Add(1)
	return json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "c08-home-index",
			Auth: Auth{
				ID:       "c08-home-auth",
				Provider: c08HomeRefreshProvider,
				Status:   StatusActive,
				Attributes: map[string]string{
					"auth_kind":  "oauth",
					"websockets": "true",
				},
				Metadata: map[string]any{"access_token": "stale-access-token"},
			},
		},
	)
}

type c08HomeRefreshExecutor struct {
	streamMode     string
	nilRetryStream bool
	nilRetryChunks bool
	requirePrepare bool
	executeCalls   atomic.Int32
	countCalls     atomic.Int32
	streamCalls    atomic.Int32
	refreshCalls   atomic.Int32
	prepareCalls   atomic.Int32
}

func (*c08HomeRefreshExecutor) Identifier() string { return c08HomeRefreshProvider }

func (e *c08HomeRefreshExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executeCalls.Add(1)
	if errAuth := e.validateAuth(auth); errAuth != nil {
		return cliproxyexecutor.Response{}, errAuth
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *c08HomeRefreshExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.streamCalls.Add(1)
	if authAccessToken(auth) == "stale-access-token" {
		if e.streamMode == "bootstrap" {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Err: c08UnauthorizedError()}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		}
		if e.streamMode == "started" {
			chunks := make(chan cliproxyexecutor.StreamChunk, 2)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("started")}
			chunks <- cliproxyexecutor.StreamChunk{Err: c08UnauthorizedError()}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		}
		return nil, c08UnauthorizedError()
	}
	if errAuth := e.validateAuth(auth); errAuth != nil {
		return nil, errAuth
	}
	if e.nilRetryStream {
		return nil, nil
	}
	if e.nilRetryChunks {
		return &cliproxyexecutor.StreamResult{}, nil
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *c08HomeRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["access_token"] = "fresh-access-token"
	delete(updated.Metadata, "project_id")
	return updated, nil
}

func (e *c08HomeRefreshExecutor) CountTokens(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.countCalls.Add(1)
	if errAuth := e.validateAuth(auth); errAuth != nil {
		return cliproxyexecutor.Response{}, errAuth
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (*c08HomeRefreshExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, nil
}

func (e *c08HomeRefreshExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return e.requirePrepare && auth != nil && auth.Metadata["project_id"] != "prepared-project"
}

func (e *c08HomeRefreshExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.prepareCalls.Add(1)
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["project_id"] = "prepared-project"
	return updated, nil
}

func (e *c08HomeRefreshExecutor) validateAuth(auth *Auth) error {
	if authAccessToken(auth) == "stale-access-token" {
		return c08UnauthorizedError()
	}
	if e.requirePrepare && auth.Metadata["project_id"] != "prepared-project" {
		return &Error{HTTPStatus: http.StatusBadRequest, Message: "missing prepared auth"}
	}
	return nil
}

func c08UnauthorizedError() error {
	return &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired access token"}
}

func newC08HomeRefreshManager(
	dispatcher *c08HomeRefreshDispatcher,
	executor *c08HomeRefreshExecutor,
) *Manager {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(executor)
	return manager
}

func TestC08HomeUnauthorizedRefreshRepreparesAllExecutionModes(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Manager) error
	}{
		{
			name: "execute",
			run: func(manager *Manager) error {
				_, errExecute := manager.Execute(
					context.Background(), []string{c08HomeRefreshProvider},
					cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "count tokens",
			run: func(manager *Manager) error {
				_, errCount := manager.ExecuteCount(
					context.Background(), []string{c08HomeRefreshProvider},
					cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{},
				)
				return errCount
			},
		},
		{
			name: "stream",
			run: func(manager *Manager) error {
				result, errStream := manager.ExecuteStream(
					context.Background(), []string{c08HomeRefreshProvider},
					cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{Stream: true},
				)
				if errStream != nil {
					return errStream
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				dispatcher := &c08HomeRefreshDispatcher{}
				executor := &c08HomeRefreshExecutor{requirePrepare: true}
				manager := newC08HomeRefreshManager(dispatcher, executor)

				if errRun := test.run(manager); errRun != nil {
					t.Fatalf("execution error = %v", errRun)
				}
				if got := dispatcher.calls.Load(); got != 1 {
					t.Fatalf("Home dispatch calls = %d, want 1", got)
				}
				if got := executor.refreshCalls.Load(); got != 1 {
					t.Fatalf("refresh calls = %d, want 1", got)
				}
				if got := executor.prepareCalls.Load(); got != 2 {
					t.Fatalf("prepare calls = %d, want initial and refreshed preparation", got)
				}
			},
		)
	}
}

func TestC08HomeUnauthorizedBootstrapRetryRejectsInvalidStream(t *testing.T) {
	for _, test := range []struct {
		name           string
		nilRetryStream bool
		nilRetryChunks bool
	}{
		{name: "nil result", nilRetryStream: true},
		{name: "nil chunks", nilRetryChunks: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				executor := &c08HomeRefreshExecutor{
					streamMode:     "bootstrap",
					nilRetryStream: test.nilRetryStream,
					nilRetryChunks: test.nilRetryChunks,
				}
				manager := newC08HomeRefreshManager(&c08HomeRefreshDispatcher{}, executor)
				result, errStream := manager.ExecuteStream(
					context.Background(), []string{c08HomeRefreshProvider},
					cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{Stream: true},
				)
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				var streamErr error
				for chunk := range result.Chunks {
					streamErr = errors.Join(streamErr, chunk.Err)
				}
				var authErr *Error
				if !errors.As(streamErr, &authErr) || authErr.Code != "empty_stream" {
					t.Fatalf("stream error = %#v, want empty_stream", streamErr)
				}
				if got := executor.streamCalls.Load(); got != 2 {
					t.Fatalf("stream calls = %d, want initial attempt and one retry", got)
				}
			},
		)
	}
}

func TestC08HomeUnauthorizedStartedStreamDoesNotReplay(t *testing.T) {
	executor := &c08HomeRefreshExecutor{streamMode: "started"}
	manager := newC08HomeRefreshManager(&c08HomeRefreshDispatcher{}, executor)
	result, errStream := manager.ExecuteStream(
		context.Background(), []string{c08HomeRefreshProvider},
		cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{Stream: true},
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	var sawPayload, sawUnauthorized bool
	for chunk := range result.Chunks {
		sawPayload = sawPayload || string(chunk.Payload) == "started"
		sawUnauthorized = sawUnauthorized || statusCodeFromError(chunk.Err) == http.StatusUnauthorized
	}
	if !sawPayload || !sawUnauthorized {
		t.Fatalf("stream results = payload %v unauthorized %v, want both", sawPayload, sawUnauthorized)
	}
	if got := executor.refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0 after stream started", got)
	}
	if got := executor.streamCalls.Load(); got != 1 {
		t.Fatalf("stream calls = %d, want 1", got)
	}
}
