package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func resourceMutationEngine(t *testing.T, h *Handler) *gin.Engine {
	t.Helper()
	if h.stateStore == nil {
		store, err := openManagementStateStorePath(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		h.stateStore = store
		t.Cleanup(func() { _ = store.Close() })
	}
	engine := gin.New()
	engine.Use(fakeManagementAuditIdentity(), h.MutationAuditMiddleware())
	return engine
}

func callResourceMutation(
	t *testing.T,
	engine *gin.Engine,
	method, path, body string,
	status int,
	outcome string,
	kinds ...string,
) MutationResult {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	result := decodeMutationHeader(t, response)
	if response.Code != status || result.Outcome != outcome || result.ConfirmedEffect != (len(kinds) > 0) {
		t.Fatalf("%s %s status=%d body=%s result=%+v", method, path, response.Code, response.Body.String(), result)
	}
	if len(result.Effects) != len(kinds) {
		t.Fatalf("effects=%+v want kinds=%v", result.Effects, kinds)
	}
	for _, kind := range kinds {
		found := false
		for _, effect := range result.Effects {
			if effect.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %+v", kind, result.Effects)
		}
	}
	return result
}

func TestMutationCredentialEffectsFollowPersistencePublicationAndBatchFailure(t *testing.T) {
	for _, audit := range []bool{true, false} {
		t.Run(
			fmt.Sprintf("audit=%t", audit), func(t *testing.T) {
				dir := t.TempDir()
				store := sdkauth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				h := &Handler{cfg: &config.Config{AuthDir: dir}, authManager: manager, tokenStore: store}
				engine := resourceMutationEngine(t, h)
				if !audit {
					h.stateStore = nil
				}
				engine.POST("/auth-files", h.UploadAuthFile)
				engine.PATCH("/auth-fields", h.PatchAuthFileFields)
				engine.DELETE("/auth-files", h.DeleteAuthFile)
				outcome := func(value string) string {
					if !audit {
						return "uncertain"
					}
					return value
				}
				result := callResourceMutation(
					t, engine, http.MethodPost, "/auth-files?name=first.json",
					`{"type":"codex","access_token":"synthetic-first"}`, 200, outcome("succeeded"),
					"credential.persisted", "credential.published",
				)
				if result.AuditAvailable != audit {
					t.Fatalf("audit availability=%t want %t", result.AuditAvailable, audit)
				}
				if _, err := os.Stat(filepath.Join(dir, "first.json")); err != nil {
					t.Fatal(err)
				}
				callResourceMutation(
					t, engine, http.MethodPatch, "/auth-fields", `{"name":"first.json","note":"retained"}`, 200,
					outcome("succeeded"), "credential.published",
				)
				auth, found := manager.GetByID("first.json")
				if !found || auth.Metadata["note"] != "retained" {
					t.Fatalf("published fields=%+v", auth)
				}
				callResourceMutation(
					t, engine, http.MethodDelete, "/auth-files", `{"names":["first.json","missing.json"]}`, 207,
					outcome("partial"), "credential.deleted",
				)
				if _, err := os.Stat(filepath.Join(dir, "first.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("successful batch member still exists: %v", err)
				}
				h.postAuthPersistHook = func(
					context.Context,
					*coreauth.Auth,
				) error {
					return errors.New("synthetic runtime failure")
				}
				callResourceMutation(
					t, engine, http.MethodPost, "/auth-files?name=second.json",
					`{"type":"codex","access_token":"synthetic-second"}`, 500, outcome("partial"),
					"credential.persisted", "credential.published",
				)
				if _, err := os.Stat(filepath.Join(dir, "second.json")); err != nil {
					t.Fatal(err)
				}
				callResourceMutation(
					t, engine, http.MethodPost, "/auth-files?name=invalid.json", `{"type":`, 500, outcome("rejected"),
				)
			},
		)
	}
}

func TestMutationUsageEffectsRequireCommittedChangesAndQueueOnlyRepricing(t *testing.T) {
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("synthetic usage database unavailable")
	}
	defer persister.Stop()
	h := &Handler{usagePersister: persister}
	engine := resourceMutationEngine(t, h)
	engine.POST("/usage/import", h.ImportUsageStatistics)
	engine.POST("/usage/trim", h.TrimUsageStatistics)
	engine.POST("/usage/reprice", h.RecalculateCosts)
	body := fmt.Sprintf(
		`[{"timestamp":%q,"model":"synthetic","completed":true,"tokens":{"input_tokens":1,"total_tokens":1}}]`,
		time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano),
	)
	callResourceMutation(t, engine, http.MethodPost, "/usage/import", body, 200, "succeeded", "usage.persisted")
	callResourceMutation(t, engine, http.MethodPost, "/usage/import", body, 200, "uncertain")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	persister.SetPriceFunc(
		func(string) (float64, float64, float64, bool) {
			once.Do(func() { close(entered) })
			<-release
			return 1, 0, 0, true
		},
	)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	callResourceMutation(t, engine, http.MethodPost, "/usage/reprice", "", 202, "succeeded", "task.queued")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reprice task was not queued")
	}
	callResourceMutation(t, engine, http.MethodPost, "/usage/reprice", "", 200, "uncertain")
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for persister.RecalculateCostsStatus().Running {
		if time.Now().After(deadline) {
			t.Fatal("synthetic repricing did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	persister.SetRetention(config.UsageRetention{Days: 1})
	callResourceMutation(t, engine, http.MethodPost, "/usage/trim", "", 200, "succeeded", "usage.deleted")
	requests, _, err := persister.QueryRequestCountsContext(context.Background())
	if err != nil || requests != 0 {
		t.Fatalf("trim committed requests=%d err=%v", requests, err)
	}
	callResourceMutation(t, engine, http.MethodPost, "/usage/trim", "", 200, "uncertain")
	callResourceMutation(t, engine, http.MethodPost, "/usage/import", `{"invalid":`, 400, "rejected")
}

func TestMutationQuotaTaskAcknowledgmentsDoNotClaimExecutionSuccess(t *testing.T) {
	started := make(chan struct{}, 1)
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(ctx context.Context, _ *quota.Entry) ([]byte, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	scheduler.Register("claude.json", quota.TypeClaude, "synthetic-auth")
	h := newQuotaRefreshTaskTestHandler(t, scheduler)
	engine := resourceMutationEngine(t, h)
	engine.POST("/refresh", h.PostQuotaRefresh)
	engine.POST("/task/:task_id/pause", h.PostQuotaRefreshTaskPause)
	engine.POST("/task/:task_id/resume", h.PostQuotaRefreshTaskResume)
	engine.POST("/task/:task_id/cancel", h.PostQuotaRefreshTaskCancel)
	callResourceMutation(
		t, engine, http.MethodPost, "/refresh", `{"credentials":["claude.json"]}`, 202, "succeeded", "task.created",
	)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("synthetic task did not start")
	}
	service := h.quotaRefreshService()
	service.mu.Lock()
	id := service.order[0]
	service.mu.Unlock()
	for _, control := range []string{"pause", "resume", "cancel"} {
		status := 200
		if control == "cancel" {
			status = 202
		}
		callResourceMutation(
			t, engine, http.MethodPost, "/task/"+id+"/"+control, "", status, "succeeded", "task.persisted",
		)
	}
	task := waitQuotaRefreshTask(t, service, id, quotaRefreshTaskStatusCancelled)
	if task.Status == quotaRefreshTaskStatusSucceeded {
		t.Fatal("task acknowledgment became execution success")
	}
	callResourceMutation(t, engine, http.MethodPost, "/task/missing/pause", "", 404, "uncertain")
}

func TestMutationLoggingEffectsRequireAnActualFileOperation(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{cfg: &config.Config{LoggingToFile: true}, logDir: dir}
	engine := resourceMutationEngine(t, h)
	engine.DELETE("/logs", h.DeleteLogs)
	callResourceMutation(t, engine, http.MethodDelete, "/logs", "", 200, "uncertain")
	path := filepath.Join(dir, defaultLogFileName)
	if err := os.WriteFile(path, []byte("synthetic log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	callResourceMutation(t, engine, http.MethodDelete, "/logs", "", 200, "succeeded", "logging.deleted")
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 0 {
		t.Fatalf("log truncation data=%q err=%v", data, err)
	}
}

func TestMutationPanelUpdateEffectsTrackTheSpecificSynchronousPublication(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", dir)
	t.Setenv("GITHUB_TOKEN", "")
	path := filepath.Join(dir, "management.html")
	if err := os.WriteFile(path, []byte("old panel"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	fail := false
	http.DefaultTransport = releaseRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			status, body := 200, "abc"
			if request.URL.Host == "api.github.com" {
				body = `{"tag_name":"v1.2.3","assets":[{"name":"management.html","browser_download_url":"https://github.com/` +
					`synthetic/panel/releases/download/v1.2.3/management.html","digest":"sha256:` +
					`ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}]}`
			}
			if fail {
				status, body = 503, "synthetic failure"
			}
			return &http.Response{
				StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
				Request: request,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previous })
	h := &Handler{cfg: &config.Config{}, configFilePath: filepath.Join(dir, "config.yaml")}
	engine := resourceMutationEngine(t, h)
	engine.POST("/panel", h.PostPanelUpdate)
	callResourceMutation(t, engine, http.MethodPost, "/panel", `{}`, 200, "succeeded", "update.persisted")
	callResourceMutation(
		t, engine, http.MethodPost, "/panel", `{"version":"v1.2.3"}`, 200, "succeeded", "update.persisted",
	)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "abc" {
		t.Fatalf("published panel=%q err=%v", data, err)
	}
	callResourceMutation(t, engine, http.MethodPost, "/panel", `{}`, 200, "uncertain")
	fail = true
	callResourceMutation(t, engine, http.MethodPost, "/panel", `{"version":"v1.2.3"}`, 502, "uncertain")
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "abc" {
		t.Fatalf("failed update changed panel=%q err=%v", data, err)
	}
}

func TestMutationInspectionQueueRemainsCompatibleWithoutStorage(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	engine := resourceMutationEngine(t, h)
	h.stateStore = nil
	engine.POST("/inspection", h.PostAccountInspectionRun)
	result := callResourceMutation(t, engine, http.MethodPost, "/inspection", "", 202, "uncertain", "task.queued")
	if result.AuditAvailable {
		t.Fatal("legacy task without persistence reported available audit")
	}
	service := h.inspectionService()
	service.mu.Lock()
	run := cloneInspectionRun(service.currentRun)
	if run == nil {
		run = cloneInspectionRun(service.lastRun)
	}
	service.mu.Unlock()
	if run == nil || run.ID == "" {
		t.Fatal("queue acknowledgment did not create a run")
	}
	encoded, err := json.Marshal(run)
	if err != nil || len(encoded) == 0 {
		t.Fatal("created task is unreadable")
	}
}
