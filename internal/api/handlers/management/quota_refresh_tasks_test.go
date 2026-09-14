package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/gin-gonic/gin"
)

func TestQuotaRefreshTaskPersistsTargetResults(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(),
		func(context.Context, *quota.Entry) ([]byte, error) { return []byte(`{"remaining":10}`), nil },
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	h := NewHandler(&config.Config{}, configPath, nil, withQuotaScheduler(scheduler))

	task, err := h.quotaRefreshService().createTask([]string{"claude.json", "missing.json"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	completed := waitQuotaRefreshTask(t, h.quotaRefreshService(), task.ID, quotaRefreshTaskStatusSucceeded)
	if completed.Summary.Succeeded != 1 || completed.Summary.Skipped != 1 {
		t.Fatalf("task summary = %+v", completed.Summary)
	}
	if completed.Targets[0].Status != quotaRefreshTargetStatusSucceeded ||
		completed.Targets[1].Status != quotaRefreshTargetStatusSkipped {
		t.Fatalf("task targets = %+v", completed.Targets)
	}
	if err = h.Close(); err != nil {
		t.Fatalf("close first handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil)
	defer func() { _ = h2.Close() }()
	restored, err := h2.quotaRefreshService().taskSnapshot(task.ID)
	if err != nil {
		t.Fatalf("restore task: %v", err)
	}
	if restored.Status != quotaRefreshTaskStatusSucceeded || restored.Summary != completed.Summary {
		t.Fatalf("restored task = %+v, want %+v", restored, completed)
	}
}

func TestQuotaRefreshTaskAppliesGlobalAndProviderConcurrencyLimits(t *testing.T) {
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	var mu sync.Mutex
	active := 0
	maxActive := 0
	activeByProvider := make(map[quota.CredentialType]int)
	maxByProvider := make(map[quota.CredentialType]int)
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(ctx context.Context, entry *quota.Entry) ([]byte, error) {
			mu.Lock()
			active++
			activeByProvider[entry.Type]++
			if active > maxActive {
				maxActive = active
			}
			if activeByProvider[entry.Type] > maxByProvider[entry.Type] {
				maxByProvider[entry.Type] = activeByProvider[entry.Type]
			}
			mu.Unlock()
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			mu.Lock()
			active--
			activeByProvider[entry.Type]--
			mu.Unlock()
			return []byte(`{}`), nil
		},
	)
	for _, suffix := range []string{"a", "b", "c"} {
		scheduler.Register("claude-"+suffix+".json", quota.TypeClaude, "claude-auth-"+suffix)
		scheduler.Register("codex-"+suffix+".json", quota.TypeCodex, "codex-auth-"+suffix)
	}
	h := newQuotaRefreshTaskTestHandler(t, scheduler)
	task, err := h.quotaRefreshService().createTask(nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	for range quotaRefreshTaskGlobalConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("global concurrency was not reached")
		}
	}
	select {
	case <-started:
		t.Fatal("refresh exceeded the global concurrency limit")
	case <-time.After(50 * time.Millisecond):
	}
	mu.Lock()
	if maxActive != quotaRefreshTaskGlobalConcurrency {
		t.Fatalf("max active = %d, want %d", maxActive, quotaRefreshTaskGlobalConcurrency)
	}
	for provider, maximum := range maxByProvider {
		if maximum > quotaRefreshTaskProviderConcurrency {
			t.Fatalf("provider %s max active = %d, limit %d", provider, maximum, quotaRefreshTaskProviderConcurrency)
		}
	}
	mu.Unlock()
	close(release)
	completed := waitQuotaRefreshTask(t, h.quotaRefreshService(), task.ID, quotaRefreshTaskStatusSucceeded)
	if completed.Summary.Succeeded != 6 {
		t.Fatalf("task summary = %+v", completed.Summary)
	}
}

func TestQuotaRefreshTaskPauseStopsQueuedTargetsUntilResume(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(ctx context.Context, entry *quota.Entry) ([]byte, error) {
			started <- entry.FileName
			select {
			case <-release:
				return []byte(`{}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	)
	scheduler.Register("one.json", quota.TypeClaude, "auth-1")
	scheduler.Register("two.json", quota.TypeClaude, "auth-2")
	h := newQuotaRefreshTaskTestHandler(t, scheduler)
	service := h.quotaRefreshService()
	service.mu.Lock()
	service.globalLimit = 1
	service.providerLimit = 1
	service.globalSemaphore = make(chan struct{}, 1)
	service.providerSemaphores = make(map[string]chan struct{})
	service.mu.Unlock()

	task, err := service.createTask(nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first target did not start")
	}
	if _, err = service.pauseTask(task.ID); err != nil {
		t.Fatalf("pause task: %v", err)
	}
	release <- struct{}{}
	select {
	case name := <-started:
		t.Fatalf("target %s started while task was paused", name)
	case <-time.After(50 * time.Millisecond):
	}
	paused, err := service.taskSnapshot(task.ID)
	if err != nil || paused.Status != quotaRefreshTaskStatusPaused || paused.Summary.Queued != 1 {
		t.Fatalf("paused task = %+v error=%v", paused, err)
	}
	if _, err = service.resumeTask(task.ID); err != nil {
		t.Fatalf("resume task: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued target did not start after resume")
	}
	release <- struct{}{}
	waitQuotaRefreshTask(t, service, task.ID, quotaRefreshTaskStatusSucceeded)
}

func TestQuotaRefreshTaskCancelPreservesQuotaState(t *testing.T) {
	started := make(chan struct{}, 1)
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(ctx context.Context, _ *quota.Entry) ([]byte, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	h := newQuotaRefreshTaskTestHandler(t, scheduler)
	service := h.quotaRefreshService()
	task, err := service.createTask([]string{"claude.json"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("target did not start")
	}
	if _, err = service.cancelTask(task.ID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	cancelled := waitQuotaRefreshTask(t, service, task.ID, quotaRefreshTaskStatusCancelled)
	if cancelled.Summary.Cancelled != 1 || cancelled.Targets[0].Status != quotaRefreshTargetStatusCancelled {
		t.Fatalf("cancelled task = %+v", cancelled)
	}
	entry := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeClaude, "claude.json")]
	if entry == nil || entry.Status != quota.StatusIdle || entry.LastRefresh != nil || entry.FailureCount != 0 {
		t.Fatalf("quota state after cancellation = %+v", entry)
	}
}

func TestQuotaRefreshTaskRestartMarksActiveTargetsInterrupted(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute)
	state := quotaRefreshTasksPersistedState{
		Tasks: []quotaRefreshTask{
			{
				ID:        "active-task",
				Status:    quotaRefreshTaskStatusRunning,
				CreatedAt: startedAt,
				StartedAt: &startedAt,
				Targets: []quotaRefreshTaskTarget{
					{ID: "claude::one.json", EntryKey: "claude::one.json", Status: quotaRefreshTargetStatusRunning},
					{ID: "claude::two.json", EntryKey: "claude::two.json", Status: quotaRefreshTargetStatusQueued},
				},
			},
		},
		UpdatedAt: startedAt,
	}
	if err = store.saveQuotaRefreshTasks(context.Background(), state); err != nil {
		t.Fatalf("save active task: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close state store: %v", err)
	}

	h := NewHandler(&config.Config{}, configPath, nil)
	restored, err := h.quotaRefreshService().taskSnapshot("active-task")
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if restored.Status != quotaRefreshTaskStatusInterrupted || restored.Summary.Interrupted != 2 {
		t.Fatalf("restored task = %+v", restored)
	}
	if err = h.Close(); err != nil {
		t.Fatalf("close handler: %v", err)
	}

	h2 := NewHandler(&config.Config{}, configPath, nil)
	defer func() { _ = h2.Close() }()
	reloaded, err := h2.quotaRefreshService().taskSnapshot("active-task")
	if err != nil || reloaded.Status != quotaRefreshTaskStatusInterrupted || reloaded.Summary.Interrupted != 2 {
		t.Fatalf("reloaded task = %+v error=%v", reloaded, err)
	}
}

func TestQuotaRefreshTaskPersistenceFailureCannotPublishSuccess(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(), func(context.Context, *quota.Entry) ([]byte, error) {
			started <- struct{}{}
			<-release
			return []byte(`{}`), nil
		},
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	h := NewHandler(&config.Config{}, configPath, nil, withQuotaScheduler(scheduler))
	service := h.quotaRefreshService()
	task, err := service.createTask([]string{"claude.json"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("target did not start")
	}
	if err = h.stateStore.Close(); err != nil {
		t.Fatalf("close state store: %v", err)
	}
	close(release)
	result := waitQuotaRefreshTask(t, service, task.ID, quotaRefreshTaskStatusInconclusive)
	if result.PersistenceError == "" || result.Status == quotaRefreshTaskStatusSucceeded {
		t.Fatalf("task after persistence failure = %+v", result)
	}
}

func TestPostQuotaRefreshReturnsPersistentTaskIdentity(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(),
		func(context.Context, *quota.Entry) ([]byte, error) { return []byte(`{}`), nil },
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	h := NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil, withQuotaScheduler(scheduler))
	defer func() { _ = h.Close() }()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/quota/refresh",
		bytes.NewBufferString(`{"credentials":["claude.json"]}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.PostQuotaRefresh(ginCtx)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var task quotaRefreshTask
	if err := json.Unmarshal(recorder.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.ID == "" || task.Status == "" || len(task.Targets) != 1 {
		t.Fatalf("task response = %+v", task)
	}
	waitQuotaRefreshTask(t, h.quotaRefreshService(), task.ID, quotaRefreshTaskStatusSucceeded)
}

func TestPostQuotaRefreshRejectsUnavailableTaskPersistence(t *testing.T) {
	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) { return []byte(`{}`), nil },
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	h := NewHandler(&config.Config{}, "", nil, withQuotaScheduler(scheduler))
	defer func() { _ = h.Close() }()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", bytes.NewBufferString(`{}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.PostQuotaRefresh(ginCtx)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
}

func TestQuotaRefreshTaskControlHandlersExposePersistentState(t *testing.T) {
	started := make(chan struct{}, 1)
	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(),
		func(ctx context.Context, _ *quota.Entry) ([]byte, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	h := newQuotaRefreshTaskTestHandler(t, scheduler)
	task, err := h.quotaRefreshService().createTask([]string{"claude.json"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("target did not start")
	}

	callControl := func(
		name string,
		handler func(*gin.Context),
		wantStatus int,
		wantTaskStatuses ...string,
	) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Params = gin.Params{{Key: "task_id", Value: task.ID}}
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		handler(ginCtx)
		if recorder.Code != wantStatus {
			t.Fatalf("%s status = %d, want %d body=%s", name, recorder.Code, wantStatus, recorder.Body.String())
		}
		var response quotaRefreshTask
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
			t.Fatalf("decode %s response: %v", name, errDecode)
		}
		statusMatched := slices.Contains(wantTaskStatuses, response.Status)
		if response.ID != task.ID || !statusMatched {
			t.Fatalf("%s response = %+v, want status in %v", name, response, wantTaskStatuses)
		}
	}

	callControl("pause", h.PostQuotaRefreshTaskPause, http.StatusOK, quotaRefreshTaskStatusPaused)
	callControl("resume", h.PostQuotaRefreshTaskResume, http.StatusOK, quotaRefreshTaskStatusRunning)
	callControl(
		"cancel",
		h.PostQuotaRefreshTaskCancel,
		http.StatusAccepted,
		quotaRefreshTaskStatusRunning,
		quotaRefreshTaskStatusCancelled,
	)
	cancelled := waitQuotaRefreshTask(t, h.quotaRefreshService(), task.ID, quotaRefreshTaskStatusCancelled)

	getRecorder := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRecorder)
	getCtx.Params = gin.Params{{Key: "task_id", Value: task.ID}}
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetQuotaRefreshTask(getCtx)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	var restored quotaRefreshTask
	if err = json.Unmarshal(getRecorder.Body.Bytes(), &restored); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if restored.ID != task.ID || restored.Status != cancelled.Status || restored.Summary != cancelled.Summary {
		t.Fatalf("get response = %+v, want %+v", restored, cancelled)
	}

	listRecorder := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRecorder)
	listCtx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetQuotaRefreshTasks(listCtx)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listResponse struct {
		Tasks []quotaRefreshTask `json:"tasks"`
		Total int                `json:"total"`
	}
	if err = json.Unmarshal(listRecorder.Body.Bytes(), &listResponse); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if listResponse.Total != 1 || len(listResponse.Tasks) != 1 || listResponse.Tasks[0].ID != task.ID {
		t.Fatalf("list response = %+v", listResponse)
	}
}

func TestQuotaRefreshTaskHandlersReturnNotFoundAndConflict(t *testing.T) {
	h := newQuotaRefreshTaskTestHandler(t, quota.NewScheduler(quota.DefaultConfig(), nil))
	for _, testCase := range []struct {
		name    string
		method  string
		handler func(*gin.Context)
	}{
		{name: "get", method: http.MethodGet, handler: h.GetQuotaRefreshTask},
		{name: "pause", method: http.MethodPost, handler: h.PostQuotaRefreshTaskPause},
		{name: "resume", method: http.MethodPost, handler: h.PostQuotaRefreshTaskResume},
		{name: "cancel", method: http.MethodPost, handler: h.PostQuotaRefreshTaskCancel},
	} {
		t.Run(
			testCase.name+"-not-found", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ginCtx, _ := gin.CreateTestContext(recorder)
				ginCtx.Params = gin.Params{{Key: "task_id", Value: "missing-task"}}
				ginCtx.Request = httptest.NewRequest(testCase.method, "/", nil)
				testCase.handler(ginCtx)
				if recorder.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
				}
			},
		)
	}

	scheduler := quota.NewSchedulerWithContext(
		quota.DefaultConfig(),
		func(context.Context, *quota.Entry) ([]byte, error) { return []byte(`{}`), nil },
	)
	scheduler.Register("claude.json", quota.TypeClaude, "auth-1")
	h2 := newQuotaRefreshTaskTestHandler(t, scheduler)
	task, err := h2.quotaRefreshService().createTask([]string{"claude.json"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	waitQuotaRefreshTask(t, h2.quotaRefreshService(), task.ID, quotaRefreshTaskStatusSucceeded)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "task_id", Value: task.ID}}
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	h2.PostQuotaRefreshTaskPause(ginCtx)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func newQuotaRefreshTaskTestHandler(t *testing.T, scheduler *quota.Scheduler) *Handler {
	t.Helper()
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	h := NewHandler(
		&config.Config{},
		filepath.Join(t.TempDir(), "config.yaml"),
		nil,
		withQuotaScheduler(scheduler),
	)
	t.Cleanup(
		func() {
			if err := h.Close(); err != nil && !errors.Is(err, errQuotaRefreshTaskPersistenceUnavailable) {
				t.Errorf("close handler: %v", err)
			}
		},
	)
	return h
}

func waitQuotaRefreshTask(
	t *testing.T,
	service *quotaRefreshTaskService,
	taskID string,
	status string,
) *quotaRefreshTask {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		task, err := service.taskSnapshot(taskID)
		if err != nil {
			t.Fatalf("get task %s: %v", taskID, err)
		}
		if task.Status == status {
			return task
		}
		if quotaRefreshTaskTerminal(task.Status) && task.Status != status {
			t.Fatalf("task %s reached status %s, want %s: %+v", taskID, task.Status, status, task)
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s status = %s, want %s", taskID, task.Status, status)
		}
		time.Sleep(time.Millisecond)
	}
}
