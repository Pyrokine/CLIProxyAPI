package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func sumRecentRequestBuckets(buckets []coreauth.RecentRequestBucket) (int64, int64) {
	var success int64
	var failed int64
	for _, bucket := range buckets {
		success += bucket.Success
		failed += bucket.Failed
	}
	return success, failed
}

func firstAPIKeyUsageEntry(entries map[string]apiKeyUsageEntry) (apiKeyUsageEntry, string) {
	for key, entry := range entries {
		return entry, key
	}
	return apiKeyUsageEntry{}, ""
}

func TestGetAPIKeyUsage_GroupsByProviderAndAPIKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:       "codex-auth",
			Provider: "codex",
			Attributes: map[string]string{
				"api_key":  "codex-key",
				"base_url": "https://codex.example.com",
			},
		},
	); err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	if _, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:       "claude-auth",
			Provider: "claude",
			Attributes: map[string]string{
				"api_key":  "claude-key",
				"base_url": "https://claude.example.com",
			},
		},
	); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}

	manager.MarkResult(
		context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: true},
	)
	manager.MarkResult(
		context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: false},
	)
	manager.MarkResult(
		context.Background(),
		coreauth.Result{AuthID: "claude-auth", Provider: "claude", Model: "claude-4", Success: true},
	)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	codexEntry, codexKey := firstAPIKeyUsageEntry(payload["codex"])
	if strings.Contains(codexKey, "codex-key") {
		t.Fatalf("codex bucket key leaked raw API key: %q", codexKey)
	}
	if len(codexEntry.RecentRequests) != 24 {
		t.Fatalf("codex buckets len = %d, want 24", len(codexEntry.RecentRequests))
	}
	codexSuccess, codexFailed := sumRecentRequestBuckets(codexEntry.RecentRequests)
	if codexEntry.Success != 1 || codexEntry.Failed != 1 {
		t.Fatalf("codex totals = %d/%d, want 1/1", codexEntry.Success, codexEntry.Failed)
	}
	if codexSuccess != 1 || codexFailed != 1 {
		t.Fatalf("codex recent totals = %d/%d, want 1/1", codexSuccess, codexFailed)
	}

	claudeEntry, claudeKey := firstAPIKeyUsageEntry(payload["claude"])
	if strings.Contains(claudeKey, "claude-key") {
		t.Fatalf("claude bucket key leaked raw API key: %q", claudeKey)
	}
	if len(claudeEntry.RecentRequests) != 24 {
		t.Fatalf("claude buckets len = %d, want 24", len(claudeEntry.RecentRequests))
	}
	claudeSuccess, claudeFailed := sumRecentRequestBuckets(claudeEntry.RecentRequests)
	if claudeEntry.Success != 1 || claudeEntry.Failed != 0 {
		t.Fatalf("claude totals = %d/%d, want 1/0", claudeEntry.Success, claudeEntry.Failed)
	}
	if claudeSuccess != 1 || claudeFailed != 0 {
		t.Fatalf("claude recent totals = %d/%d, want 1/0", claudeSuccess, claudeFailed)
	}
}

func TestGetAPIKeyUsage_GroupsOpenAICompatibleByCompatName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "vast-auth", Provider: "openai-compatible-vast",
			Attributes: map[string]string{
				"api_key": "vast-key", "base_url": "https://www.vastnum.com/v1", "compat_name": "VAST",
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	manager.MarkResult(
		t.Context(),
		coreauth.Result{AuthID: "vast-auth", Provider: "openai-compatible-vast", Model: "gpt-5", Success: true},
	)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	h.GetAPIKeyUsage(c)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "vast-key") {
		t.Fatalf("usage status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["openai-compatible-vast"]; exists {
		t.Fatalf("namespaced provider bucket remains: %v", payload)
	}
	bucket, exists := payload["vast"]
	if !exists || len(bucket) != 1 {
		t.Fatalf("compat provider bucket = %v", payload)
	}
	entry, key := firstAPIKeyUsageEntry(bucket)
	if !strings.HasPrefix(key, "https://www.vastnum.com/v1|") || entry.Success != 1 || entry.Failed != 0 {
		t.Fatalf("compat usage key = %q, entry = %+v", key, entry)
	}
}
