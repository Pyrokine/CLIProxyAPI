package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func inspectionPreconditionRouter(h *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/account-inspection/status", h.GetAccountInspectionStatus)
	router.POST("/account-inspection/run", h.PostAccountInspectionRun)
	router.POST("/account-inspection/inspect-one", h.PostAccountInspectionInspectOne)
	router.POST("/account-inspection/stop", h.PostAccountInspectionStop)
	return router
}

func inspectionPreconditionRequest(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/account-inspection/"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func waitInspectionPreconditionStatus(
	t *testing.T, service *accountInspectionService, status string,
) accountInspectionPersistedState {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		state := service.persistenceSnapshot()
		if state.Status == status {
			return state
		}
		select {
		case <-timeout.C:
			t.Fatalf("inspection status = %q, want %q", state.Status, status)
		case <-ticker.C:
		}
	}
}

type inspectionPreconditionFixture struct {
	handler    *Handler
	router     *gin.Engine
	entered    chan context.Context
	release    func()
	probeCalls atomic.Int32
}

func newInspectionPreconditionFixture(t *testing.T) *inspectionPreconditionFixture {
	t.Helper()
	fixture := &inspectionPreconditionFixture{entered: make(chan context.Context, 4)}
	release := make(chan struct{})
	var releaseOnce sync.Once
	fixture.release = func() { releaseOnce.Do(func() { close(release) }) }
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "inspection-precondition-auth", FileName: "inspection-precondition.json",
			Provider: "codex", Status: coreauth.StatusActive,
		},
	)
	if err != nil {
		t.Fatalf("register synthetic auth: %v", err)
	}
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(ctx context.Context, _ *quota.Entry) ([]byte, error) {
			fixture.probeCalls.Add(1)
			fixture.entered <- ctx
			// Keep the worker blocked after cancellation to expose premature re-entry.
			<-release
			return []byte(`{"usage":"ok"}`), nil
		},
	)
	scheduler.Register(auth.FileName, quota.TypeCodex, auth.Index)
	fixture.handler = &Handler{authManager: manager, quotaScheduler: scheduler}
	service := fixture.handler.inspectionService()
	fixture.router = inspectionPreconditionRouter(fixture.handler)
	t.Cleanup(
		func() {
			service.stop()
			fixture.release()
			waitInspectionPreconditionStatus(t, service, accountInspectionStatusStopped)
			if err := fixture.handler.Close(); err != nil {
				t.Errorf("close inspection handler: %v", err)
			}
		},
	)
	return fixture
}

func (f *inspectionPreconditionFixture) start(t *testing.T) (accountInspectionRun, context.Context) {
	t.Helper()
	response := inspectionPreconditionRequest(f.router, http.MethodPost, "run", "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("start inspection = %d %s", response.Code, response.Body.String())
	}
	var run accountInspectionRun
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.ID == "" || run.Status != accountInspectionStatusRunning {
		t.Fatalf("accepted run = %+v", run)
	}
	select {
	case ctx := <-f.entered:
		return run, ctx
	case <-time.After(5 * time.Second):
		t.Fatal("inspection did not reach the injected probe")
		return run, nil
	}
}

func TestAccountInspectionPreconditionsRejectConcurrentReentryAndMissingIdentifier(t *testing.T) {
	fixture := newInspectionPreconditionFixture(t)
	run, ctx := fixture.start(t)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, path := range []string{"run", "inspect-one"} {
		go func() {
			<-start
			responses <- inspectionPreconditionRequest(
				fixture.router, http.MethodPost, path, `{"id":"inspection-precondition-auth"}`,
			)
		}()
	}
	close(start)
	for range 2 {
		select {
		case response := <-responses:
			if response.Code != http.StatusConflict ||
				!strings.Contains(response.Body.String(), "account inspection already running") {
				t.Fatalf("re-entry = %d %s", response.Code, response.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent inspection request did not return")
		}
	}
	for _, body := range []string{`{}`, `{"id":" ","file_name":" ","auth_index":" "}`} {
		response := inspectionPreconditionRequest(fixture.router, http.MethodPost, "inspect-one", body)
		if response.Code != http.StatusBadRequest || !strings.Contains(
			response.Body.String(), "missing account identifier",
		) {
			t.Fatalf("missing identifier = %d %s", response.Code, response.Body.String())
		}
	}
	state := fixture.handler.accountInspection.persistenceSnapshot()
	if state.Status != accountInspectionStatusRunning || state.CurrentRun == nil || state.CurrentRun.ID != run.ID ||
		state.LastRun != nil || len(state.Runs) != 0 || len(state.Logs) != 1 ||
		fixture.probeCalls.Load() != 1 || ctx.Err() != nil {
		t.Fatalf(
			"re-entry changed the active run: state=%+v probes=%d context=%v",
			state, fixture.probeCalls.Load(), ctx.Err(),
		)
	}
}

func TestAccountInspectionPreconditionsStopWaitsForWorkerAndIdleStopIsIdempotent(t *testing.T) {
	fixture := newInspectionPreconditionFixture(t)
	run, ctx := fixture.start(t)
	service := fixture.handler.accountInspection
	var cancelCalls atomic.Int32
	service.mu.Lock()
	cancel := service.cancel
	service.cancel = func() {
		cancelCalls.Add(1)
		cancel()
	}
	service.mu.Unlock()

	response := inspectionPreconditionRequest(fixture.router, http.MethodPost, "stop", "")
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"stopped"`) {
		t.Fatalf("stop running inspection = %d %s", response.Code, response.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel the probe context")
	}
	response = inspectionPreconditionRequest(fixture.router, http.MethodPost, "run", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("re-entry before worker exits = %d %s", response.Code, response.Body.String())
	}
	state := service.persistenceSnapshot()
	if state.Status != accountInspectionStatusRunning || state.CurrentRun == nil || state.CurrentRun.ID != run.ID ||
		state.LastRun != nil || len(state.Runs) != 0 || fixture.probeCalls.Load() != 1 || cancelCalls.Load() != 1 {
		t.Fatalf(
			"cancellation released the run before its worker: state=%+v probes=%d cancels=%d",
			state, fixture.probeCalls.Load(), cancelCalls.Load(),
		)
	}

	fixture.release()
	state = waitInspectionPreconditionStatus(t, service, accountInspectionStatusStopped)
	if state.CurrentRun != nil || state.LastRun == nil || state.LastRun.ID != run.ID ||
		state.LastRun.Status != accountInspectionStatusStopped || state.LastRun.EndedAt == nil || len(state.Runs) != 1 {
		t.Fatalf("worker did not finalize the stopped run: %+v", state)
	}
	service.mu.Lock()
	hasCancel := service.cancel != nil
	service.mu.Unlock()
	if hasCancel {
		t.Fatal("finished worker retained its cancel function")
	}
	for range 2 {
		response = inspectionPreconditionRequest(fixture.router, http.MethodPost, "stop", "")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"stopped"`) {
			t.Fatalf("idle stop = %d %s", response.Code, response.Body.String())
		}
	}
	if after := service.persistenceSnapshot(); !reflect.DeepEqual(state, after) ||
		fixture.probeCalls.Load() != 1 || cancelCalls.Load() != 1 {
		t.Fatalf(
			"idle stop changed inspection state: before=%+v after=%+v probes=%d cancels=%d",
			state, after, fixture.probeCalls.Load(), cancelCalls.Load(),
		)
	}
}

func TestAccountInspectionPreconditionsRestartRecoversPersistedRunningRun(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	old := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	t.Cleanup(
		func() {
			if err := old.Close(); err != nil {
				t.Errorf("close old handler: %v", err)
			}
		},
	)
	if old.stateStore == nil {
		t.Fatal("old handler did not open the temporary management SQLite store")
	}
	started := time.Now().UTC().Add(-time.Minute)
	seed := accountInspectionPersistedState{
		Schedule: defaultAccountInspectionSchedule(), Status: accountInspectionStatusRunning,
		CurrentRun: &accountInspectionRun{
			ID: "interrupted-inspection", Status: accountInspectionStatusRunning, StartedAt: started,
		},
		UpdatedAt: started,
	}
	// Seed crash-time storage while leaving graceful shutdown no active in-memory run to rewrite.
	if err := old.stateStore.saveAccountInspection(context.Background(), seed); err != nil {
		t.Fatalf("save interrupted inspection: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old handler and SQLite: %v", err)
	}

	restored := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	t.Cleanup(
		func() {
			if err := restored.Close(); err != nil {
				t.Errorf("close restored handler: %v", err)
			}
		},
	)
	response := inspectionPreconditionRequest(inspectionPreconditionRouter(restored), http.MethodGet, "status", "")
	if response.Code != http.StatusOK {
		t.Fatalf("restored inspection status = %d %s", response.Code, response.Body.String())
	}
	var status accountInspectionStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode restored status: %v", err)
	}
	if status.Status != accountInspectionStatusStopped || status.CurrentRun != nil || status.LastRun == nil ||
		status.LastRun.ID != seed.CurrentRun.ID || status.LastRun.Status != accountInspectionStatusStopped ||
		status.LastRun.Error != "interrupted by server restart" || status.LastRun.EndedAt == nil ||
		!status.LastRun.StartedAt.Equal(started) || status.LastRun.Duration <= 0 {
		t.Fatalf("restart did not expose the interrupted run: %+v", status)
	}
	if restored.stateStore == nil {
		t.Fatal("restored handler did not reopen the temporary management SQLite store")
	}
	persisted, err := restored.stateStore.loadAccountInspection(context.Background())
	if err != nil {
		t.Fatalf("load recovered inspection from SQLite: %v", err)
	}
	if persisted == nil || persisted.Status != accountInspectionStatusStopped || persisted.CurrentRun != nil ||
		persisted.LastRun == nil || persisted.LastRun.Error != "interrupted by server restart" ||
		len(persisted.Runs) != 1 || !reflect.DeepEqual(persisted.Runs[0], *persisted.LastRun) ||
		len(persisted.Logs) != 1 || !strings.Contains(persisted.Logs[0].Message, "interrupted by server restart") {
		t.Fatalf("restart recovery was not persisted: %+v", persisted)
	}
}
