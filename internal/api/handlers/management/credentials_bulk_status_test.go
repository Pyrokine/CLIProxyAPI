package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBulkAuthFileStatusDoesNotHoldLockDuringRuntimeSync(t *testing.T) {
	for _, source := range []string{"file", "plugin", "config", "config single"} {
		t.Run(
			source, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				block := func() { close(started); <-release }
				manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
				handler := &Handler{cfg: &config.Config{}, authManager: manager}
				name := "first.json"
				switch source {
				case "file":
					if _, err := manager.Register(
						t.Context(), &coreauth.Auth{
							ID: name, FileName: name, Provider: "qwen", Status: coreauth.StatusActive,
							Metadata: map[string]any{"type": "qwen"},
						},
					); err != nil {
						t.Fatal(err)
					}
					handler.SetPostAuthPersistHook(
						func(_ context.Context, auth *coreauth.Auth) error {
							if auth.ID == name {
								block()
							}
							return nil
						},
					)
				case "plugin":
					handler, manager, _, _, _ = newPluginVirtualManagementHandler(t)
					name = "plugin-source.json"
					handler.SetPostAuthPersistHook(
						func(_ context.Context, auth *coreauth.Auth) error {
							if auth.ID == "plugin-a" {
								block()
							}
							return nil
						},
					)
				case "config", "config single":
					cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "fixture-key"}}}
					auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
						&synthesizer.SynthesisContext{
							Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
						},
					)
					if err != nil || len(auths) != 1 {
						t.Fatalf("synthesize config: %v, auths=%d", err, len(auths))
					}
					if _, err = manager.Register(t.Context(), auths[0]); err != nil {
						t.Fatal(err)
					}
					handler = newConfigListTestHandler(t, cfg)
					handler.authManager = manager
					handler.configCommitted = func(context.Context, ConfigCommit) error { block(); return nil }
					name = auths[0].ID
				}
				if _, err := manager.Register(
					coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
						ID: "other.json", FileName: "other.json", Provider: "qwen", Status: coreauth.StatusActive,
						Metadata: map[string]any{"type": "qwen"},
					},
				); err != nil {
					t.Fatal(err)
				}

				firstDone := make(chan struct{})
				var secondDone chan struct{}
				var first, second *httptest.ResponseRecorder
				t.Cleanup(
					func() {
						releaseOnce.Do(func() { close(release) })
						for _, done := range []chan struct{}{firstDone, secondDone} {
							if done == nil {
								continue
							}
							select {
							case <-done:
							case <-time.After(2 * time.Second):
								t.Error("status request did not finish after releasing runtime sync")
							}
						}
					},
				)
				go func() {
					defer close(firstDone)
					if source == "config single" {
						first = runConfigListRequest(
							handler.PatchAuthFileStatus, http.MethodPatch,
							"/auth-files/status", `{"name":"`+name+`","disabled":true}`,
						)
						return
					}
					first = runConfigListRequest(
						handler.PatchAuthFilesBulkDisable, http.MethodPatch,
						"/auth-files/bulk-disable", `{"names":["`+name+`"]}`,
					)
				}()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("bulk runtime synchronization did not start")
				}
				secondDone = make(chan struct{})
				go func() {
					defer close(secondDone)
					second = runConfigListRequest(
						handler.PatchAuthFileStatus, http.MethodPatch,
						"/auth-files/status", `{"name":"other.json","disabled":true}`,
					)
				}()
				select {
				case <-secondDone:
					if second.Code != http.StatusOK {
						t.Fatalf("unrelated status=%d body=%s", second.Code, second.Body.String())
					}
				case <-time.After(time.Second):
					t.Fatal("bulk runtime synchronization blocked an unrelated status update")
				}
				releaseOnce.Do(func() { close(release) })
				select {
				case <-firstDone:
					if first.Code != http.StatusOK {
						t.Fatalf("bulk status=%d body=%s", first.Code, first.Body.String())
					}
				case <-time.After(2 * time.Second):
					t.Fatal("bulk status did not finish after runtime synchronization")
				}
			},
		)
	}
}

func TestBulkAuthFileStatusRetainsPerNameOutcomes(t *testing.T) {
	for _, partial := range []bool{true, false} {
		t.Run(
			map[bool]string{true: "partial", false: "all rejected"}[partial], func(t *testing.T) {
				handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
				before, err := os.ReadFile(sourcePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = manager.Register(
					coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
						ID: "ordinary.json", FileName: "ordinary.json", Provider: "qwen", Status: coreauth.StatusActive,
						Metadata: map[string]any{"type": "qwen"},
					},
				); err != nil {
					t.Fatal(err)
				}
				names := []string{"plugin-a", "missing.json"}
				wantStatus := http.StatusUnprocessableEntity
				if partial {
					names = append(names, "ordinary.json")
					wantStatus = http.StatusOK
				}
				body, err := json.Marshal(bulkAuthFilesRequest{Names: names})
				if err != nil {
					t.Fatal(err)
				}
				ctx, response := newConfigCommitTestContext(http.MethodPatch, "/auth-files/bulk-disable", string(body))
				mutation := &mutationState{claimed: true, contextValid: true}
				ctx.Request = ctx.Request.WithContext(
					context.WithValue(
						ctx.Request.Context(), mutationContextKey{}, mutation,
					),
				)
				handler.PatchAuthFilesBulkDisable(ctx)
				if response.Code != wantStatus {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				var result bulkAuthFilesResult
				if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if len(result.Failed) != 2 || result.Failed["plugin-a"] != errPluginVirtualAuth.Error() ||
					result.Failed["missing.json"] == "" {
					t.Fatalf("failed results=%#v", result.Failed)
				}
				if partial && !reflect.DeepEqual(
					result.Updated, []string{"ordinary.json"},
				) || !partial && len(result.Updated) != 0 {
					t.Fatalf("updated=%v, partial=%v", result.Updated, partial)
				}
				current, ok := manager.GetByID("ordinary.json")
				if !ok || current.Disabled != partial {
					t.Fatalf("ordinary auth=%#v, partial=%v", current, partial)
				}
				mutation.mu.Lock()
				receipt := mutation.snapshotLocked()
				mutation.mu.Unlock()
				if receipt.ConfirmedEffect != partial || partial && receipt.Outcome != "partial" {
					t.Fatalf("partial=%v mutation receipt=%#v", partial, receipt)
				}
				child, ok := manager.GetByID("plugin-a")
				if !ok || child.Disabled {
					t.Fatalf("rejected plugin child changed: %#v", child)
				}
				if after, errRead := os.ReadFile(sourcePath); errRead != nil || string(after) != string(before) {
					t.Fatalf("plugin source changed: %q, error=%v", after, errRead)
				}
			},
		)
	}
}

func TestBulkAuthFileStatusSynchronizesEveryPublishedAliasAfterHookFailure(t *testing.T) {
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	for _, id := range []string{"devin-a", "devin-b"} {
		if _, err := manager.Register(
			t.Context(), &coreauth.Auth{
				ID: id, FileName: "shared.json", Provider: "devin", Status: coreauth.StatusActive,
				Metadata: map[string]any{"type": "devin"},
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Handler{cfg: &config.Config{}, authManager: manager}
	hooked := make(map[string]bool)
	handler.SetPostAuthPersistHook(
		func(_ context.Context, auth *coreauth.Auth) error {
			hooked[auth.ID] = auth.Disabled
			if len(hooked) == 1 {
				return errors.New("injected first alias runtime failure")
			}
			return nil
		},
	)
	response := runConfigListRequest(
		handler.PatchAuthFilesBulkDisable, http.MethodPatch,
		"/auth-files/bulk-disable", `{"names":["shared.json"]}`,
	)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(
		response.Body.String(), "first alias runtime failure",
	) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, id := range []string{"devin-a", "devin-b"} {
		current, ok := manager.GetByID(id)
		if !ok || !current.Disabled || !hooked[id] {
			t.Fatalf("published alias %s was not synchronized: auth=%#v hooks=%v", id, current, hooked)
		}
	}
}

func TestBulkConfigAuthFileStatusDistinguishesSaveAndRuntimeFailure(t *testing.T) {
	for _, failure := range []string{"save", "runtime"} {
		t.Run(
			failure, func(t *testing.T) {
				cfg := &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "fixture-key"}}}
				auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
					&synthesizer.SynthesisContext{
						Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
					},
				)
				if err != nil || len(auths) != 1 {
					t.Fatalf("synthesize config: %v, auths=%d", err, len(auths))
				}
				manager := coreauth.NewManager(nil, nil, nil)
				if _, err = manager.Register(t.Context(), auths[0]); err != nil {
					t.Fatal(err)
				}
				handler := newConfigListTestHandler(t, cfg)
				handler.authManager = manager
				called := false
				handler.configCommitted = func(context.Context, ConfigCommit) error {
					called = true
					return errors.New("injected config runtime failure")
				}
				if failure == "save" {
					blocked := filepath.Join(t.TempDir(), "not-a-directory")
					if err = os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
						t.Fatal(err)
					}
					handler.configFilePath = filepath.Join(blocked, "config.yaml")
				}
				response := runConfigListRequest(
					handler.PatchAuthFilesBulkDisable, http.MethodPatch,
					"/auth-files/bulk-disable", `{"names":["`+auths[0].ID+`"]}`,
				)
				if response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				var result bulkAuthFilesResult
				if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if len(result.Updated) != 0 || len(result.Failed) != 1 || result.Failed[auths[0].ID] == "" {
					t.Fatalf("failed config mutation result=%#v", result)
				}
				current, ok := manager.GetByID(auths[0].ID)
				if !ok || current.Disabled {
					t.Fatalf("failed runtime synchronization changed credential: %#v", current)
				}
				if failure == "save" {
					if called || len(handler.configSnapshot().GeminiKey[0].ExcludedModels) != 0 {
						t.Fatal("failed save published config or invoked runtime synchronization")
					}
				} else {
					persisted, errLoad := config.LoadConfig(handler.configFilePath)
					if errLoad != nil {
						t.Fatal(errLoad)
					}
					if !called || !reflect.DeepEqual(persisted.GeminiKey[0].ExcludedModels, []string{"*"}) ||
						!strings.Contains(result.Failed[auths[0].ID], "runtime") {
						t.Fatalf(
							"committed runtime failure: called=%v, config=%#v, result=%#v", called, persisted.GeminiKey,
							result,
						)
					}
				}
			},
		)
	}
}
