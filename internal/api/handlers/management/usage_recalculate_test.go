package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestRecalculateCostsRejectsMissingPersisterAndPricing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name string
		h    *Handler
		want string
	}{
		{
			name: "missing persister",
			h:    &Handler{},
			want: "usage persister not configured",
		},
		{
			name: "missing pricing",
			h: &Handler{
				usagePersister: usage.NewPersister(t.TempDir(), config.UsageRetention{}),
			},
			want: "no model prices configured",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/usage/recalculate-costs", nil)

				tc.h.RecalculateCosts(ctx)

				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
				}
				var payload struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if payload.Error != tc.want {
					t.Fatalf("error = %q, want %q", payload.Error, tc.want)
				}
			},
		)
	}
}

func TestRecalculateCostsStartsOnceAndPublishesStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	baseDir := t.TempDir()
	persister := usage.NewPersister(baseDir, config.UsageRetention{})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("persister did not become ready")
	}
	defer persister.Stop()

	if _, err := persister.Import(
		[]byte(`[
		{"timestamp":"2026-07-31T00:00:00Z","model":"test-model","completed":true,
		 "tokens":{"input_tokens":1,"total_tokens":1}}
	]`),
	); err != nil {
		t.Fatalf("seed usage event: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	persister.SetPriceFunc(
		func(string) (float64, float64, float64, bool) {
			enteredOnce.Do(func() { close(entered) })
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

	h := &Handler{usagePersister: persister}
	started := callRecalculateCosts(t, h)
	if started.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want %d; body=%s", started.Code, http.StatusAccepted, started.Body.String())
	}
	assertRecalculationPayload(t, started, "started", true, false)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recalculation did not reach price lookup")
	}

	busy := callRecalculateCosts(t, h)
	if busy.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want %d; body=%s", busy.Code, http.StatusOK, busy.Body.String())
	}
	assertRecalculationPayload(t, busy, "busy", true, true)

	running := getRecalculateCostsStatus(t, h)
	if running.Status != "running" || !running.Running || running.Phase != "repricing" ||
		running.TotalEvents != 1 || running.Percent < 0 || running.Percent > 95 {
		t.Fatalf("running status = %+v", running)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := getRecalculateCostsStatus(t, h)
		if !status.Running {
			if status.Status != "ok" || status.Error != "" {
				t.Fatalf("completed status = %+v", status)
			}
			startedAt, errStart := time.Parse(time.RFC3339Nano, status.StartedAt)
			completedAt, errEnd := time.Parse(time.RFC3339Nano, status.CompletedAt)
			if errStart != nil || errEnd != nil || completedAt.Before(startedAt) || status.UpdatedAt != status.CompletedAt {
				t.Fatalf("completed times = %+v, parse errors=%v/%v", status, errStart, errEnd)
			}
			persister.Stop()
			reopened := usage.NewPersister(baseDir, config.UsageRetention{})
			reopened.Start(context.Background())
			defer reopened.Stop()
			restored := getRecalculateCostsStatus(t, &Handler{usagePersister: reopened})
			if restored.StartedAt != status.StartedAt || restored.CompletedAt != status.CompletedAt || restored.UpdatedAt != status.UpdatedAt {
				t.Fatalf("reopen changed recorded times: %+v -> %+v", status, restored)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recalculation did not finish: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUsageStorageUpgradeHTTPStatusIsReadOnlyAndReportsErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name       string
		h          *Handler
		wantCode   int
		wantStatus string
	}{
		{name: "unconfigured", h: &Handler{}, wantCode: http.StatusOK, wantStatus: "idle"},
		{
			name:     "uninitialized",
			h:        &Handler{usagePersister: usage.NewPersister(t.TempDir(), config.UsageRetention{})},
			wantCode: http.StatusInternalServerError, wantStatus: "error",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				response := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(response)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/usage/storage-upgrade", nil)
				test.h.GetUsageStorageUpgrade(ctx)
				if response.Code != test.wantCode {
					t.Fatalf("HTTP=%d want=%d body=%s", response.Code, test.wantCode, response.Body.String())
				}
				var status usage.StorageUpgradeStatus
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if status.Status != test.wantStatus || status.Running || test.wantStatus == "error" && status.Error == "" {
					t.Fatalf("status=%+v", status)
				}
			},
		)
	}
	p := usage.NewPersister(t.TempDir(), config.UsageRetention{})
	p.Start(t.Context())
	defer p.Stop()
	h := &Handler{usagePersister: p}
	for _, canceled := range []bool{false, true} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		requestCtx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		ctx.Request = httptest.NewRequestWithContext(requestCtx, http.MethodGet, "/usage/storage-upgrade", nil)
		h.GetUsageStorageUpgrade(ctx)
		cancel()
		var status usage.StorageUpgradeStatus
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if canceled {
			if response.Code != http.StatusInternalServerError || status.Status != "error" || status.Running || status.Error == "" {
				t.Fatalf("canceled status HTTP=%d %+v", response.Code, status)
			}
		} else if response.Code != http.StatusOK || status.Status != "complete" || status.Running || status.ProcessedEvents == nil || *status.ProcessedEvents != 0 || status.TotalEvents == nil || *status.TotalEvents != 0 || status.Percent == nil || *status.Percent != 100 || status.StartedAt != "" || status.CompletedAt != "" {
			t.Fatalf("complete empty status HTTP=%d %+v", response.Code, status)
		}
		if costs := p.RecalculateCostsStatus(); costs.Status != "idle" || costs.Running {
			t.Fatalf("GET started cost recalculation: %+v", costs)
		}
	}
}

func callRecalculateCosts(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/usage/recalculate-costs", nil)
	h.RecalculateCosts(ctx)
	return w
}

func assertRecalculationPayload(
	t *testing.T,
	w *httptest.ResponseRecorder,
	wantStatus string,
	wantRunning, wantAlreadyRunning bool,
) {
	t.Helper()
	var payload struct {
		Status         string `json:"status"`
		Running        bool   `json:"running"`
		AlreadyRunning bool   `json:"already_running"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Status != wantStatus || payload.Running != wantRunning ||
		payload.AlreadyRunning != wantAlreadyRunning {
		t.Fatalf(
			"response = %+v, want status=%q running=%t already_running=%t",
			payload,
			wantStatus,
			wantRunning,
			wantAlreadyRunning,
		)
	}
}

func getRecalculateCostsStatus(t *testing.T, h *Handler) usage.CostRecalculationStatus {
	t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/usage/recalculate-costs", nil)
	h.GetRecalculateCostsStatus(ctx)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var status usage.CostRecalculationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return status
}
