package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/gin-gonic/gin"
)

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
