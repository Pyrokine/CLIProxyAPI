package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCacheHelper_OpenAIResponses_PreservesPromptCacheKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "test-api-key")

	ctx := util.WithGinContext(context.Background(), ginCtx)
	executor := &codexExecutor{}

	expectedKey := "my-test-cache-key-12345"
	rawJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","prompt_cache_key":"` + expectedKey + `"}`),
	}
	url := "https://example.com/responses"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey != expectedKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedKey)
	}
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != expectedKey {
		t.Fatalf("Conversation_id = %q, want %q", gotConversation, expectedKey)
	}
	if gotSession := httpReq.Header.Get("Session_id"); gotSession != expectedKey {
		t.Fatalf("Session_id = %q, want %q", gotSession, expectedKey)
	}
}

func TestCodexExecutorCacheHelper_Claude_GeneratesCacheKeyFromUserID(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	executor := &codexExecutor{}

	rawJSON := []byte(`{"model":"gpt-4.1","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-4.1",
		Payload: []byte(`{"model":"gpt-4.1","metadata":{"user_id":"test-user-123"}}`),
	}
	url := "https://example.com/messages"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey == "" {
		t.Fatal("expected prompt_cache_key to be set for claude format with user_id")
	}
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != gotKey {
		t.Fatalf("Conversation_id = %q, want %q", gotConversation, gotKey)
	}

	// Second call with same user_id should produce the same cache key
	rawJSON2 := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	httpReq2, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, req, rawJSON2)
	if err != nil {
		t.Fatalf("cacheHelper error (second call): %v", err)
	}
	body2, errRead2 := io.ReadAll(httpReq2.Body)
	if errRead2 != nil {
		t.Fatalf("read request body (second call): %v", errRead2)
	}
	gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String()
	if gotKey2 != gotKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q (should be stable)", gotKey2, gotKey)
	}
}

func TestCodexExecutorCacheHelper_Claude_GPT55DoesNotReuseUserCacheSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	executor := &codexExecutor{}

	rawJSON := []byte(`{"model":"gpt-5.5","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","metadata":{"user_id":"test-user-123"}}`),
	}
	url := "https://example.com/messages"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	if gjson.GetBytes(body, "prompt_cache_key").Exists() {
		t.Fatalf("expected no prompt_cache_key for gpt-5.5 Claude path")
	}
	if got := httpReq.Header.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	if got := httpReq.Header.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want empty", got)
	}
}

func TestCodexExecutorCacheHelper_OpenAIChatCompletions_NoCacheKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "test-api-key")

	ctx := util.WithGinContext(context.Background(), ginCtx)
	executor := &codexExecutor{}
	rawJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex"}`),
	}
	url := "https://example.com/chat/completions"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	if gjson.GetBytes(body, "prompt_cache_key").Exists() {
		t.Fatalf(
			"expected no prompt_cache_key for openai format, got %q", gjson.GetBytes(body, "prompt_cache_key").String(),
		)
	}
	if httpReq.Header.Get("Conversation_id") != "" {
		t.Fatal("expected no Conversation_id header for openai format")
	}
}

func TestApplyCodexHeadersRejectsCherryClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("User-Agent", "Mozilla/5.0 CherryStudio/1.9.6 Electron/41.2.1")
	ginCtx.Request.Header.Set("Version", "cherry-version")
	ginCtx.Request.Header.Set("Session_id", "cherry-session")
	ginCtx.Request.Header.Set("Sec-Fetch-Mode", "cors")
	ginCtx.Request.Header.Set("Http-Referer", "https://cherry-ai.com")

	req, err := http.NewRequestWithContext(
		util.WithGinContext(context.Background(), ginCtx), http.MethodPost, "https://example.com/responses", nil,
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("X-Forwarded-For", "203.0.113.1")

	applyCodexHeaders(req, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "token", true, nil)

	if got := req.Header.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := req.Header.Get("Version"); got != codexClientVersion {
		t.Fatalf("Version = %q, want %q", got, codexClientVersion)
	}
	if got := req.Header.Get("Session_id"); got == "" || got == "cherry-session" {
		t.Fatalf("Session_id = %q, want generated non-Cherry value", got)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	for _, header := range []string{"Sec-Fetch-Site", "X-Forwarded-For", "Http-Referer"} {
		if got := req.Header.Get(header); got != "" {
			t.Fatalf("%s = %q, want empty", header, got)
		}
	}
}

func TestApplyCodexHeadersUsesConfiguredOAuthDefaults(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{UserAgent: "codex_cli_rs/0.132.0 (Linux; x64)"},
	}

	applyCodexHeaders(req, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != cfg.CodexHeaderDefaults.UserAgent {
		t.Fatalf("User-Agent = %q, want configured default", got)
	}
	if got := req.Header.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want empty for non-Mac configured UA", got)
	}
}

func TestApplyCodexHeadersPreservesCodexClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	ginCtx.Request.Header.Set("Version", "0.131.0")
	ginCtx.Request.Header.Set("Session_id", "codex-session")
	ginCtx.Request.Header.Set("Originator", "codex_cli_rs")

	req, err := http.NewRequestWithContext(
		util.WithGinContext(context.Background(), ginCtx), http.MethodPost, "https://example.com/responses", nil,
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	applyCodexHeaders(req, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "token", true, nil)

	if got := req.Header.Get("User-Agent"); got != ginCtx.Request.Header.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want Codex client value", got)
	}
	if got := req.Header.Get("Version"); got != "0.131.0" {
		t.Fatalf("Version = %q, want Codex client value", got)
	}
	if got := req.Header.Get("Session_id"); got != "codex-session" {
		t.Fatalf("Session_id = %q, want Codex client value", got)
	}
}
