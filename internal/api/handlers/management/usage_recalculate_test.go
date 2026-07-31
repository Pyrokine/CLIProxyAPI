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
		t.Run(tc.name, func(t *testing.T) {
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
		})
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

	if _, err := persister.Import([]byte(`[
		{"timestamp":"2026-07-31T00:00:00Z","model":"test-model","completed":true,
		 "tokens":{"input_tokens":1,"total_tokens":1}}
	]`)); err != nil {
		t.Fatalf("seed usage event: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	persister.SetPriceFunc(func(string) (float64, float64, float64, bool) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return 1, 0, 0, true
	})
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
	if running.Status != "running" || !running.Running {
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recalculation did not finish: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
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
