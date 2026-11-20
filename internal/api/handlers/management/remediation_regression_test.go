package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestPutImageArtifactCacheRejectsOverflowBeforeSave(t *testing.T) {
	for _, body := range []string{
		`{"retention_days":106752}`, `{"retention_days":213504}`,
		fmt.Sprintf(`{"max_total_size_mb":%d}`, config.MaxImageArtifactCacheSizeMB+1),
	} {
		h := NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil, withEnvSecret(""))
		c, rec := newArtifactCacheTestContext(http.MethodPut, "/image-artifact-cache", body)
		h.PutImageArtifactCache(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if h.configSnapshot().ImageArtifactCache != (config.ImageArtifactCache{}) {
			t.Fatal("rejected request changed configuration")
		}
	}
}

func TestTrimUsageStatisticsReportsStorageFailure(t *testing.T) {
	p := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: 1})
	h := &Handler{usagePersister: p}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/usage/trim", nil)
	h.TrimUsageStatistics(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSummaryResponseRetainsSubcentCosts(t *testing.T) {
	var qr usage.QueryResult
	qr.Totals.Cost = 0.213625
	if err := json.Unmarshal([]byte(`[{"time":"2026-04-27T10:00:00Z"}]`), &qr.TimeSeries); err != nil {
		t.Fatal(err)
	}
	qr.TimeSeries[0].Cost = 0.0003683
	resp := buildSummaryResponse(time.Now(), time.Now(), qr, usageCredentialMasker{}, usageAPIKeyPresenter{}, false)
	if resp.Totals.Cost != qr.Totals.Cost || resp.TimeSeries[0].Cost != qr.TimeSeries[0].Cost ||
		!resp.TimeSeries[0].HasCost {
		t.Fatalf("cost totals=%v series=%+v", resp.Totals.Cost, resp.TimeSeries)
	}
}

type quotaBindingHost struct {
	*managementQuotaHost
	entered chan struct{}
	release chan struct{}
}

func (h *quotaBindingHost) ResetQuota(
	ctx context.Context,
	req pluginapi.QuotaResetRequest,
) (pluginapi.QuotaResetResponse, bool, error) {
	close(h.entered)
	select {
	case <-h.release:
	case <-ctx.Done():
		return pluginapi.QuotaResetResponse{}, true, ctx.Err()
	}
	return h.managementQuotaHost.ResetQuota(ctx, req)
}
func (h *quotaBindingHost) ResetQuotaByPlugin(
	ctx context.Context,
	_ string,
	req pluginapi.QuotaResetRequest,
) (pluginapi.QuotaResetResponse, bool, error) {
	return h.ResetQuota(ctx, req)
}
func TestManagementQuotaResetPreservesReplacementCredential(t *testing.T) {
	for _, pluginSpecific := range []bool{false, true} {
		t.Run(
			fmt.Sprint(pluginSpecific), func(t *testing.T) {
				fake := &managementQuotaHost{
					pluginID: "test-quota", provider: "codex", supportsReset: true, resetHandled: true,
					resetResponse: pluginapi.QuotaResetResponse{Success: true},
				}
				handler, manager, original := newManagementQuotaHandler(t, fake)
				host := &quotaBindingHost{
					managementQuotaHost: fake, entered: make(chan struct{}), release: make(chan struct{}),
				}
				handler.pluginHost = host
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(
					http.MethodPost, "/credential-quota/reset",
					strings.NewReader(fmt.Sprintf(`{"auth_index":%q}`, original.Index)),
				)
				if pluginSpecific {
					c.Params = gin.Params{{Key: "id", Value: fake.pluginID}}
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					if pluginSpecific {
						handler.ResetPluginQuota(c)
					} else {
						handler.ResetCredentialQuota(c)
					}
				}()
				<-host.entered
				replacement := original.Clone()
				replacement.Metadata["account"] = "two"
				replacement.Metadata["access_token"] = "replacement-token"
				replacement.Quota.Exceeded = true
				_, updateErr := manager.Update(context.Background(), replacement)
				close(host.release)
				<-done
				if updateErr != nil {
					t.Fatal(updateErr)
				}
				current, found := manager.GetByID(original.ID)
				if !found || current.Metadata["account"] != "two" || !current.Quota.Exceeded {
					t.Fatalf("replacement=%+v", current)
				}
				if rec.Code != http.StatusInternalServerError || !strings.Contains(
					rec.Body.String(), "routing_quota_reset_failed",
				) {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			},
		)
	}
}

func TestEmptyOAuthCallbackLeavesSessionAvailable(t *testing.T) {
	state := "remediation-empty-callback"
	dir := t.TempDir()
	registerOAuthSessionForAuthDir(state, "codex", "", dir)
	if path, err := WriteOAuthCallbackFileForPendingSession("codex", state, "  ", "  "); err == nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if _, err := WriteOAuthCallbackFileForPendingSession("codex", state, "valid-code", ""); err != nil {
		t.Fatal(err)
	}
}

func TestGetMetaKeysPreservesStructureAndRedactsOnlySecrets(t *testing.T) {
	cooling := false
	retry := 0
	weight := 7
	cfg := &config.Config{
		MetaKey: []config.MetaKey{
			{
				APIKey: "synthetic-meta-secret", Prefix: "team", Weight: &weight, DisableCooling: &cooling,
				RequestRetry: &retry,
			},
		},
	}
	h := NewHandler(cfg, "", nil, withEnvSecret(""))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/meta-api-key", nil)
	h.GetMetaKeys(c)
	var response struct {
		Keys []config.MetaKey `json:"meta-api-key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK || len(response.Keys) != 1 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	key := response.Keys[0]
	if key.Prefix != "team" || key.Weight == nil || *key.Weight != 7 || key.DisableCooling == nil ||
		*key.DisableCooling ||
		key.RequestRetry == nil ||
		*key.RequestRetry != 0 ||
		key.APIKey == "synthetic-meta-secret" ||
		key.APIKey == "" {
		t.Fatalf("response=%+v", key)
	}
	if strings.Contains(rec.Body.String(), "synthetic-meta-secret") {
		t.Fatal("credential exposed")
	}
}
