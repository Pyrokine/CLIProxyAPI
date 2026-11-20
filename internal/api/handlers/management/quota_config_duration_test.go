package management

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/gin-gonic/gin"
)

func TestPutQuotaConfigDurationBounds(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the duration boundary requires a 64-bit configuration integer")
	}
	maximum := quota.MaxRefreshIntervalSeconds
	for _, test := range []struct {
		name     string
		interval int64
		maximum  int64
		status   int
	}{
		{name: "interval overflow", interval: maximum + 1, maximum: maximum + 1, status: http.StatusBadRequest},
		{name: "maximum overflow", interval: 600, maximum: maximum + 1, status: http.StatusBadRequest},
		{name: "representable boundary", interval: maximum, maximum: maximum, status: http.StatusOK},
		{name: "zero disables polling", interval: 0, maximum: 1800, status: http.StatusOK},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				before := []byte("quota-refresh: {enabled: false, interval: 600, max-interval: 1800}\n")
				if err := os.WriteFile(path, before, 0o600); err != nil {
					t.Fatal(err)
				}
				initial := quota.DefaultConfig()
				cfg := &config.Config{
					QuotaRefresh: config.QuotaRefresh{
						Enabled: false, Interval: 600, MaxInterval: 1800,
					},
				}
				scheduler := quota.NewScheduler(initial, nil)
				handler := NewHandler(cfg, path, nil, withQuotaScheduler(scheduler))
				t.Cleanup(
					func() {
						if err := handler.Close(); err != nil {
							t.Errorf("close handler: %v", err)
						}
					},
				)
				response := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(response)
				body := fmt.Sprintf(
					`{"enabled":false,"interval":%d,"max-interval":%d}`,
					test.interval,
					test.maximum,
				)
				ctx.Request = httptest.NewRequest(
					http.MethodPut, "/v0/management/quota/config", bytes.NewBufferString(body),
				)
				ctx.Request.Header.Set("Content-Type", "application/json")
				handler.PutQuotaConfig(ctx)
				if response.Code != test.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if test.status == http.StatusBadRequest {
					data, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(data, before) || scheduler.GetConfig() != initial {
						t.Fatalf(
							"rejected interval changed config: data=%s err=%v config=%+v", data, err,
							scheduler.GetConfig(),
						)
					}
					if !bytes.Contains(response.Body.Bytes(), []byte(`"maximum_seconds"`)) {
						t.Fatalf("missing supported duration: %s", response.Body.String())
					}
				} else if got := scheduler.GetConfig(); got.Enabled ||
					int64(got.Interval) !=
						test.interval ||
					int64(got.MaxInterval) !=
						test.maximum {
					t.Fatalf("saved config=%+v", got)
				}
			},
		)
	}
}
