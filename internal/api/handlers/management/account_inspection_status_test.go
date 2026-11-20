package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

type inspectionStatusStore struct {
	coreauth.Store
	fail atomic.Bool
}

func (s *inspectionStatusStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if s.fail.Load() {
		return "", errors.New("synthetic persistence failure")
	}
	return s.Store.Save(ctx, auth)
}

func newInspectionStatusFixture(t *testing.T) (
	*Handler,
	*inspectionStatusStore,
	*coreauth.Auth,
	accountInspectionResult,
) {
	t.Helper()
	dir := t.TempDir()
	files := sdkauth.NewFileTokenStore()
	files.SetBaseDir(dir)
	store := &inspectionStatusStore{Store: files}
	manager := coreauth.NewManager(store, nil, nil)
	if err := os.WriteFile(
		filepath.Join(dir, "claude.json"),
		[]byte(`{"type":"claude","access_token":"synthetic-inspection","disabled":true}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	auth, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "claude.json", FileName: "claude.json", Provider: "claude", Disabled: true,
			Status:   coreauth.StatusDisabled,
			Metadata: map[string]any{"type": "claude", "access_token": "synthetic-inspection", "disabled": true},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(context.Context, *quota.Entry) ([]byte, error) {
			return []byte(`{"usage":"ok"}`), nil
		},
	)
	scheduler.Register(auth.FileName, quota.TypeClaude, auth.Index)
	h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store, authManager: manager, quotaScheduler: scheduler}
	service := h.inspectionService()
	results := service.inspectAccounts(context.Background(), auth.Index)
	if len(results) != 1 || results[0].ProbeStatus != accountProbePassed || results[0].Advice != "enable_when_needed" {
		t.Fatalf("inspection results=%+v", results)
	}
	service.results = results
	return h, store, auth, results[0]
}

func inspectionStatusBody(t *testing.T, auth *coreauth.Auth, probe any) string {
	t.Helper()
	body := map[string]any{"name": auth.FileName, "auth_index": auth.Index, "disabled": false}
	if probe != nil {
		body["expected_probe"] = probe
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestInspectionStatusBindsLatestPassedResultAndPreservesManualControl(t *testing.T) {
	for _, name := range []string{
		"passed", "generation", "digest", "revision", "checked_at", "incomplete", "failed", "inconclusive", "outdated",
		"persistence_failure", "manual",
	} {
		t.Run(
			name, func(t *testing.T) {
				h, store, auth, result := newInspectionStatusFixture(t)
				probe := map[string]any{
					"auth_generation": result.AuthGeneration, "credential_digest": result.CredentialDigest,
					"probe_revision": result.ProbeRevision, "checked_at": result.CheckedAt,
				}
				status, outcome := 409, "rejected"
				switch name {
				case "passed", "manual":
					status, outcome = 200, "succeeded"
				case "generation":
					probe["auth_generation"] = result.AuthGeneration + 1
				case "digest":
					probe["credential_digest"] = strings.Repeat("b", 64)
				case "revision":
					probe["probe_revision"] = "older-probe"
				case "checked_at":
					probe["checked_at"] = result.CheckedAt.Add(-time.Second)
				case "incomplete":
					delete(probe, "credential_digest")
					status = 400
				case "failed", "inconclusive", "outdated":
					h.accountInspection.results[0].ProbeStatus = name
				case "persistence_failure":
					store.fail.Store(true)
					status, outcome = 500, "uncertain"
				}
				path := filepath.Join(h.cfg.AuthDir, auth.FileName)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				engine := resourceMutationEngine(t, h)
				engine.PATCH("/status", h.PatchAuthFileStatus)
				var supplied any = probe
				if name == "manual" {
					supplied = nil
					h.accountInspection.results[0].ProbeStatus = accountProbeFailed
				}
				var effects []string
				if status == 200 {
					effects = []string{"credential.published"}
				}
				callResourceMutation(
					t, engine, http.MethodPatch, "/status", inspectionStatusBody(t, auth, supplied), status, outcome,
					effects...,
				)
				current, ok := h.authManager.GetByID(auth.ID)
				if !ok || current.Disabled != (status != 200) {
					t.Fatalf("published disabled=%t status=%d", current.Disabled, status)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if status != 200 && string(after) != string(before) {
					t.Fatal("rejected operation changed persisted credential")
				}
				if status == 200 {
					var saved map[string]any
					if err := json.Unmarshal(after, &saved); err != nil || saved["disabled"] != false {
						t.Fatalf("persisted disabled=%v err=%v", saved["disabled"], err)
					}
				}
			},
		)
	}
}

type inspectionQueueContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (ctx *inspectionQueueContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.entered) })
	return ctx.Context.Done()
}

func TestInspectionStatusRechecksAfterAuthMutationQueue(t *testing.T) {
	for _, rotation := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("credential_rotation=%t", rotation), func(t *testing.T) {
				h, store, auth, result := newInspectionStatusFixture(t)
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				go func() {
					defer close(finished)
					_, err := h.authManager.MutatePersistentAuth(
						context.Background(), auth.ID, func(ctx context.Context) (*coreauth.Auth, error) {
							close(entered)
							<-release
							if !rotation {
								return nil, errors.New("synthetic no-op mutation")
							}
							next := auth.Clone()
							next.Metadata["access_token"] = "synthetic-rotation"
							if _, err := store.Save(ctx, next); err != nil {
								return nil, err
							}
							return next, nil
						},
					)
					if rotation && err != nil {
						t.Errorf("rotate: %v", err)
					}
				}()
				<-entered
				engine := gin.New()
				engine.PATCH("/status", h.PatchAuthFileStatus)
				queued := &inspectionQueueContext{Context: context.Background(), entered: make(chan struct{})}
				request := httptest.NewRequest(
					http.MethodPatch, "/status", strings.NewReader(inspectionStatusBody(t, auth, result)),
				).WithContext(queued)
				request.Header.Set("Content-Type", "application/json")
				response, done := httptest.NewRecorder(), make(chan struct{})
				go func() { engine.ServeHTTP(response, request); close(done) }()
				select {
				case <-queued.entered:
				case <-time.After(3 * time.Second):
					close(release)
					t.Fatal("status request did not reach auth mutation queue")
				}
				if !rotation {
					h.accountInspection.mu.Lock()
					h.accountInspection.results[0].CheckedAt = result.CheckedAt.Add(time.Second)
					h.accountInspection.results[0].ProbeStatus = accountProbeFailed
					h.accountInspection.mu.Unlock()
				}
				close(release)
				<-finished
				<-done
				if response.Code != 409 || !strings.Contains(response.Body.String(), "inspection_result_outdated") {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				current, _ := h.authManager.GetByID(auth.ID)
				if !current.Disabled || rotation && current.Metadata["access_token"] != "synthetic-rotation" {
					t.Fatal("queued status changed or replaced the current credential")
				}
			},
		)
	}
}

func TestInspectionDoesNotRecommendUnsupportedPerAccountStatusChanges(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey: "synthetic-config-key", ExcludedModels: []string{"*"},
			},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesized auths=%d err=%v", len(auths), err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), auths[0])
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		t.Fatal("registered config credential is nil")
	}
	if auth.FileName != "" {
		t.Fatalf("config auth file=%q err=%v", auth.FileName, err)
	}
	h := &Handler{cfg: cfg, authManager: manager}
	results := h.inspectionService().inspectAccounts(context.Background(), auth.Index)
	if len(results) != 1 || results[0].ProbeStatus != accountProbeInconclusive || results[0].Advice != "none" {
		t.Fatalf("config results=%+v", results)
	}
	pluginHandler, pluginManager, _, _, indexes := newPluginVirtualManagementHandler(t)
	var child *coreauth.Auth
	for _, entry := range pluginManager.List() {
		if entry.Disabled {
			child = entry
			break
		}
	}
	if child == nil {
		t.Fatal("disabled plugin child missing")
	}
	child.Provider = "claude"
	child, err = pluginManager.Update(coreauth.WithSkipPersist(context.Background()), child)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(),
		func(context.Context, *quota.Entry) ([]byte, error) { return []byte(`{"usage":"ok"}`), nil },
	)
	scheduler.Register(child.FileName, quota.TypeClaude, child.Index)
	pluginHandler.quotaScheduler = scheduler
	results = pluginHandler.inspectionService().inspectAccounts(context.Background(), child.Index)
	if len(results) != 1 || results[0].ProbeStatus != accountProbePassed || results[0].Advice != "none" {
		t.Fatalf("plugin child results=%+v", results)
	}
	response := patchPluginVirtualStatus(
		t, pluginHandler, fmt.Sprintf(`{"auth_index":%q,"disabled":false}`, indexes[child.ID]),
	)
	if response.Code != 409 {
		t.Fatalf("individual plugin child status=%d", response.Code)
	}
}
