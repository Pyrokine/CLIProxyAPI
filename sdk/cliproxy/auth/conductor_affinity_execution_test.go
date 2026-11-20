package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type affinityRecordingExecutor struct {
	affinityTestExecutor
	before  func(*Auth)
	failure error
}

func (e affinityRecordingExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e.before != nil {
		e.before(auth)
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, e.failure
}

func (e affinityRecordingExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e affinityRecordingExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	response, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
	chunks <- cliproxyexecutor.StreamChunk{Terminal: true}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestManagerLegacyAffinityReachesAllExecutionEntries(t *testing.T) {
	for _, entry := range []string{"execute", "count", "stream"} {
		for _, bytesKey := range []bool{false, true} {
			name := entry + "/string"
			if bytesKey {
				name = entry + "/bytes"
			}
			t.Run(
				name, func(t *testing.T) {
					provider, model := "affinity-"+entry, "affinity-model-"+entry
					manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
					t.Cleanup(
						func() {
							//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
							if err := manager.StopAutoRefresh(nil); err != nil {
								t.Error(err)
							}
						},
					)
					manager.RegisterExecutor(affinityRecordingExecutor{id: provider})
					for _, id := range []string{provider + "-a", provider + "-b"} {
						if _, err := manager.Register(
							context.Background(), &Auth{ID: id, Provider: provider, Status: StatusActive},
						); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					}
					var key any = " original-affinity "
					if bytesKey {
						key = []byte(" original-affinity ")
					}
					invoke := func(pinned string) string {
						t.Helper()
						opts := cliproxyexecutor.Options{
							Stream: entry == "stream", Metadata: map[string]any{"auth_affinity_key": key},
						}
						if pinned != "" {
							opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = pinned
						}
						req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"synthetic"}`)}
						if entry == "stream" {
							result, err := manager.ExecuteStream(context.Background(), []string{provider}, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							var resultBody strings.Builder
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								resultBody.Write(chunk.Payload)
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
							return resultBody.String()
						}
						var response cliproxyexecutor.Response
						var err error
						if entry == "count" {
							response, err = manager.ExecuteCount(context.Background(), []string{provider}, req, opts)
						} else {
							response, err = manager.Execute(context.Background(), []string{provider}, req, opts)
						}
						if err != nil {
							t.Fatal(err)
						}
						return string(response.Payload)
					}
					first, second := provider+"-a", provider+"-b"
					if got := invoke(second); got != second || manager.AuthAffinity("original-affinity") != second {
						t.Fatalf(
							"first explicit selection=%q binding=%q, want %q", got,
							manager.AuthAffinity("original-affinity"), second,
						)
					}
					if got := invoke(""); got != second {
						t.Fatalf("cached selection=%q, want %q", got, second)
					}
					if got := invoke(first); got != first || manager.AuthAffinity("original-affinity") != first {
						t.Fatalf(
							"explicit pin did not take priority: selection=%q binding=%q", got,
							manager.AuthAffinity("original-affinity"),
						)
					}
					manager.ClearAuthAffinity(" original-affinity ")
					if manager.AuthAffinity("original-affinity") != "" {
						t.Fatal("cleared binding remains available")
					}
				},
			)
		}
	}
}

func TestManagerLegacyAffinityRejectsFailedAndReplacedResults(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "failure"
		if replacement {
			name = "credential replacement"
		}
		t.Run(
			name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				t.Cleanup(
					func() {
						//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
						if err := manager.StopAutoRefresh(nil); err != nil {
							t.Error(err)
						}
					},
				)
				executor := affinityRecordingExecutor{
					id:      "affinity-result",
					failure: errors.New("synthetic affinity failure"),
				}
				if replacement {
					executor.failure = nil
					executor.before = func(auth *Auth) {
						next := auth.Clone()
						next.Attributes = map[string]string{"api_key": "synthetic-replacement"}
						if _, err := manager.Update(context.Background(), next); err != nil {
							t.Fatal(err)
						}
					}
				}
				manager.RegisterExecutor(executor)
				const id, model = "affinity-result-auth", "affinity-result-model"
				if _, err := manager.Register(
					context.Background(), &Auth{ID: id, Provider: executor.Identifier(), Status: StatusActive},
				); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(
					id, executor.Identifier(), []*registry.ModelInfo{{ID: model}},
				)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				_, err := manager.Execute(
					context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model},
					cliproxyexecutor.Options{Metadata: map[string]any{"auth_affinity_key": "result-key"}},
				)
				if !replacement && err == nil {
					t.Fatal("failed execution returned success")
				}
				if got := manager.AuthAffinity("result-key"); got != "" {
					t.Fatalf("failed or replaced result published binding %q", got)
				}
			},
		)
	}
}

func TestManagerLegacyAffinityInvalidatesRemovedAndReloadedCredentials(t *testing.T) {
	store := &credentialWeightValidationStore{}
	manager := NewManager(store, nil, nil)
	t.Cleanup(
		func() {
			//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
			if err := manager.StopAutoRefresh(nil); err != nil {
				t.Error(err)
			}
		},
	)
	auth, err := manager.Register(
		context.Background(), &Auth{ID: "affinity-reload-auth", Provider: "test", Status: StatusActive},
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetAuthAffinity("reload-key", auth.ID)
	store.auths = []*Auth{auth.Clone()}
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.AuthAffinity("reload-key") != auth.ID {
		t.Fatal("unchanged reload removed the binding")
	}
	store.auths[0].Attributes = map[string]string{"api_key": "synthetic-other-owner"}
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.AuthAffinity("reload-key") != "" {
		t.Fatal("replacement reload retained the old credential binding")
	}
	manager.SetAuthAffinity("remove-key", auth.ID)
	if err := manager.Remove(context.Background(), auth.ID); err != nil {
		t.Fatal(err)
	}
	if manager.AuthAffinity("remove-key") != "" {
		t.Fatal("removed credential retained its binding")
	}
}

func TestManagerLegacyAffinityKeepsHomeSelectionAuthority(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	t.Cleanup(
		func() {
			//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
			if err := manager.StopAutoRefresh(nil); err != nil {
				t.Error(err)
			}
		},
	)
	manager.RegisterExecutor(affinityTestExecutor{id: "codex"})
	cfg := &internalconfig.Config{}
	cfg.Home.Enabled = true
	manager.SetConfig(cfg)
	manager.SetAuthAffinity("home-key", "local-credential")
	payload, err := json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "home-index", Auth: Auth{ID: "home-credential", Provider: "codex"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &testHomeDispatcher{heartbeat: true, payload: payload}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			"auth_affinity_key": "home-key", cliproxyexecutor.ExecutionSessionMetadataKey: "affinity-home-session",
		},
	}
	selected, _, _, err := manager.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5", opts, nil)
	if err != nil || selected == nil || selected.ID != "home-credential" || dispatcher.calls != 1 {
		t.Fatalf("Home selection=%v error=%v calls=%d", selected, err, dispatcher.calls)
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" || manager.AuthAffinity("home-key") != "local-credential" {
		t.Fatal("local affinity changed Home selection or was overwritten by it")
	}
}

func TestManagerLegacyAffinityConcurrentPublicOperations(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	t.Cleanup(
		func() {
			//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
			if err := manager.StopAutoRefresh(nil); err != nil {
				t.Error(err)
			}
		},
	)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(
			func() {
				for range 100 {
					manager.SetAuthAffinity(" shared-key ", " synthetic-auth ")
					if got := manager.AuthAffinity("shared-key"); got != "" && got != "synthetic-auth" {
						t.Errorf("unexpected binding %q", got)
					}
					manager.ClearAuthAffinity(" shared-key ")
				}
			},
		)
	}
	workers.Wait()
	var empty *Manager
	empty.SetAuthAffinity("key", "auth")
	empty.ClearAuthAffinity("key")
	if empty.AuthAffinity("key") != "" {
		t.Fatal("nil manager returned a binding")
	}
}
