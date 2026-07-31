package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestCodexQuotaAvailability(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		available bool
		known     bool
	}{
		{name: "available", data: `{"rate_limit":{"allowed":true,"limit_reached":false}}`, available: true, known: true},
		{name: "exhausted by allowed", data: `{"rate_limit":{"allowed":false}}`, known: true},
		{name: "exhausted by limit", data: `{"rateLimit":{"allowed":true,"limitReached":true}}`, known: true},
		{name: "unknown", data: `{"rate_limit":{}}`},
		{name: "malformed", data: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			available, known := codexQuotaAvailability([]byte(tt.data))
			if available != tt.available || known != tt.known {
				t.Fatalf("availability = (%v, %v), want (%v, %v)", available, known, tt.available, tt.known)
			}
		})
	}
}

func TestManualCodexQuotaRefreshRestoresRuntimeQuota(t *testing.T) {
	var resetCreditCalls atomic.Int32
	resetCreditServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		resetCreditCalls.Add(1)
	}))
	t.Cleanup(resetCreditServer.Close)
	previousResetCreditURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsConsumeURL = resetCreditServer.URL
	t.Cleanup(func() { codexRateLimitResetCreditsConsumeURL = previousResetCreditURL })

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:             "codex-recovered",
			Provider:       "codex",
			Status:         coreauth.StatusError,
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5": {
					Status:         coreauth.StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
					QuotaEpoch:     2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	scheduler := quota.NewScheduler(quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
		return []byte(`{"rate_limit":{"allowed":true,"limit_reached":false}}`), nil
	})
	scheduler.Register("codex.json", quota.TypeCodex, registered.Index)
	h := NewHandler(&config.Config{}, "", manager, WithQuotaScheduler(scheduler))

	scheduler.RefreshNowWithFinalizer([]string{"codex.json"}, h.finalizeManualQuotaRefresh)
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("restored auth missing")
	}
	state := current.ModelStates["gpt-5"]
	if state == nil || state.Quota.Exceeded || state.Unavailable || state.QuotaEpoch != 3 {
		t.Fatalf("restored model state = %#v", state)
	}
	if current.Generation == registered.Generation {
		t.Fatalf("auth generation did not advance: %d", current.Generation)
	}
	entry := scheduler.GetStatus().Credentials[quota.EntryKey(quota.TypeCodex, "codex.json")]
	if entry == nil || entry.Status != quota.StatusSuccess || entry.Error != "" {
		t.Fatalf("quota entry after recovery = %#v", entry)
	}
	if calls := resetCreditCalls.Load(); calls != 0 {
		t.Fatalf("ordinary quota refresh consumed reset credit %d times", calls)
	}
}

func TestManualCodexQuotaRefreshKeepsExhaustedRuntimeState(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:          "codex-exhausted",
			Provider:    "codex",
			Status:      coreauth.StatusError,
			Unavailable: true,
			Quota:       coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			ModelStates: map[string]*coreauth.ModelState{
				"gpt-5": {
					Status:         coreauth.StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
					QuotaEpoch:     2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	scheduler := quota.NewScheduler(quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
		return []byte(`{"rate_limit":{"allowed":false,"limit_reached":true}}`), nil
	})
	scheduler.Register("codex.json", quota.TypeCodex, registered.Index)
	h := NewHandler(&config.Config{}, "", manager, WithQuotaScheduler(scheduler))

	scheduler.RefreshNowWithFinalizer([]string{"codex.json"}, h.finalizeManualQuotaRefresh)
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("exhausted auth missing")
	}
	state := current.ModelStates["gpt-5"]
	if state == nil || !state.Quota.Exceeded || !state.Unavailable || state.QuotaEpoch != 2 {
		t.Fatalf("exhausted model state changed: %#v", state)
	}
	if current.Generation != registered.Generation {
		t.Fatalf("exhausted auth generation changed: %d", current.Generation)
	}
}

func TestManualQuotaRefreshIgnoresNonCodexCredential(t *testing.T) {
	h := &Handler{}
	err := h.finalizeManualQuotaRefresh(quota.RefreshAttempt{
		Type:      quota.TypeClaude,
		Data:      []byte(`{"rate_limit":{"allowed":true}}`),
		AuthIndex: "claude-auth",
	})
	if err != nil {
		t.Fatalf("finalizeManualQuotaRefresh() error = %v", err)
	}
}

func TestPostQuotaRefreshRejectsMalformedBody(t *testing.T) {
	scheduler := quota.NewScheduler(quota.DefaultConfig(), func(*quota.Entry) ([]byte, error) {
		return []byte(`{}`), nil
	})
	h := NewHandler(&config.Config{}, "", nil, WithQuotaScheduler(scheduler))
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/quota/refresh",
		bytes.NewBufferString(`{`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.PostQuotaRefresh(ginCtx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestPutQuotaConfigSaveFailureKeepsMemoryConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("quota-refresh: [\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	previousConfig := config.QuotaRefresh{Enabled: false, Interval: 600, MaxInterval: 1800}
	cfg := &config.Config{QuotaRefresh: previousConfig}
	schedulerConfig := quota.Config{
		Enabled: previousConfig.Enabled, Interval: previousConfig.Interval, MaxInterval: previousConfig.MaxInterval,
	}
	scheduler := quota.NewScheduler(schedulerConfig, nil)
	h := NewHandler(cfg, configPath, nil, WithQuotaScheduler(scheduler))

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPut,
		"/v0/management/quota/config",
		bytes.NewBufferString(`{"enabled":true,"interval":120,"max-interval":600}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.PutQuotaConfig(ginCtx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if cfg.QuotaRefresh != previousConfig {
		t.Fatalf("cfg.QuotaRefresh = %+v, want %+v", cfg.QuotaRefresh, previousConfig)
	}
	if got := scheduler.GetConfig(); got != schedulerConfig {
		t.Fatalf("scheduler config = %+v, want %+v", got, schedulerConfig)
	}
}
