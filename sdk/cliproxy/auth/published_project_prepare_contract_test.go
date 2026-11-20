package auth

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type projectPrepareExecutor struct {
	replaceAwareExecutor
	prepareCalls atomic.Int32
	executeCalls atomic.Int32
}

func (*projectPrepareExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth == nil || auth.Metadata["project_id"] != "prepared-project"
}

func (e *projectPrepareExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.prepareCalls.Add(1)
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["project_id"] = "prepared-project"
	return updated, nil
}

func (e *projectPrepareExecutor) Execute(
	_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executeCalls.Add(1)
	if auth.Metadata["project_id"] != "prepared-project" {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: 400, Message: "missing prepared project"}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *projectPrepareExecutor) CountTokens(
	ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	response, err := e.Execute(ctx, auth, req, opts)
	if err == nil {
		response.Payload = []byte(`{"input_tokens":1}`)
	}
	return response, err
}

func (e *projectPrepareExecutor) ExecuteStream(
	ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	response, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestPublishedProjectPreparationReachesEveryExecutionEntry(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				const model = "gemini-3.1-pro"
				store := &refreshCommitStore{}
				provider := &projectPrepareExecutor{id: "antigravity"}
				manager := NewManager(store, nil, nil)
				manager.RegisterExecutor(provider)
				auth := &Auth{
					ID: "auth-request-prepare", Provider: "antigravity",
					Metadata: map[string]any{"access_token": "token"},
				}
				if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "antigravity", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				request := cliproxyexecutor.Request{Model: model}
				for range 2 {
					switch mode {
					case "execute":
						response, err := manager.Execute(
							t.Context(), []string{"antigravity"}, request, cliproxyexecutor.Options{},
						)
						if err != nil || string(response.Payload) != "ok" {
							t.Fatalf("Execute payload=%s error=%v", response.Payload, err)
						}
					case "count":
						response, err := manager.ExecuteCount(
							t.Context(), []string{"antigravity"}, request, cliproxyexecutor.Options{},
						)
						if err != nil || string(response.Payload) != `{"input_tokens":1}` {
							t.Fatalf("ExecuteCount payload=%s error=%v", response.Payload, err)
						}
					case "stream":
						stream, err := manager.ExecuteStream(
							t.Context(), []string{"antigravity"}, request,
							cliproxyexecutor.Options{Stream: true},
						)
						if err != nil {
							t.Fatal(err)
						}
						var payload string
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							payload += string(chunk.Payload)
						}
						if payload != "ok" {
							t.Fatalf("ExecuteStream payload=%s", payload)
						}
					}
				}
				store.mu.Lock()
				persisted, saves := store.saved.Clone(), store.saveCalls
				store.mu.Unlock()
				if persisted == nil || persisted.Metadata["project_id"] != "prepared-project" || saves < 1 {
					t.Fatalf("persisted=%+v saves=%d", persisted, saves)
				}
				current, ok := manager.GetByID(auth.ID)
				if !ok || current.Metadata["project_id"] != "prepared-project" ||
					provider.prepareCalls.Load() != 1 || provider.executeCalls.Load() != 2 {
					t.Fatalf(
						"current=%+v prepare=%d execute=%d", current,
						provider.prepareCalls.Load(), provider.executeCalls.Load(),
					)
				}
			},
		)
	}
}
