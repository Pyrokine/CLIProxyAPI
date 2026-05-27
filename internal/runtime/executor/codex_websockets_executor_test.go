package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body)

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfiguredOAuthDefaults(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "codex_cli_rs/0.132.0 (Linux; x64)",
			BetaFeatures: "feature-a,feature-b",
		},
	}

	headers := applyCodexWebsocketHeaders(
		context.Background(), http.Header{}, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "",
		cfg,
	)

	if got := headers.Get("User-Agent"); got != cfg.CodexHeaderDefaults.UserAgent {
		t.Fatalf("User-Agent = %q, want configured default", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != cfg.CodexHeaderDefaults.BetaFeatures {
		t.Fatalf("x-codex-beta-features = %q, want configured default", got)
	}
	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want empty for non-Mac configured UA", got)
	}
}

func TestApplyCodexWebsocketHeadersRejectsCherryClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "Mozilla/5.0 CherryStudio/1.9.6 Electron/41.2.1")
	ginCtx.Request.Header.Set("Version", "cherry-version")
	ginCtx.Request.Header.Set("Session_id", "cherry-session")

	headers := applyCodexWebsocketHeaders(
		util.WithGinContext(context.Background(), ginCtx), http.Header{}, nil, "", nil,
	)

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := headers.Get("Version"); got != codexClientVersion {
		t.Fatalf("Version = %q, want %q", got, codexClientVersion)
	}
	if got := headers.Get("Session_id"); got == "" || got == "cherry-session" {
		t.Fatalf("Session_id = %q, want generated non-Cherry value", got)
	}
}

func TestApplyCodexWebsocketHeadersPreservesCodexClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	ginCtx.Request.Header.Set("Version", "0.131.0")
	ginCtx.Request.Header.Set("Session_id", "codex-session")

	headers := applyCodexWebsocketHeaders(
		util.WithGinContext(context.Background(), ginCtx), http.Header{}, nil, "", nil,
	)

	if got := headers.Get("User-Agent"); got != ginCtx.Request.Header.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want Codex client value", got)
	}
	if got := headers.Get("Version"); got != "0.131.0" {
		t.Fatalf("Version = %q, want Codex client value", got)
	}
	if got := headers.Get("Session_id"); got != "codex-session" {
		t.Fatalf("Session_id = %q, want Codex client value", got)
	}
}

func TestCodexWebsocketLoggingStoresTimelineAndUpgradeRejection(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{RequestLog: true}}
	headers := http.Header{
		"Authorization": {"Bearer sk-test-secret"},
	}
	info := codexWebsocketRequestLog(
		"wss://api.openai.example/v1/responses",
		headers,
		[]byte(`{"type":"response.create"}`),
		"codex",
		&cliproxyauth.Auth{ID: "auth-1", Label: "Codex"},
	)

	recordAPIWebsocketRequest(ctx, cfg, info)
	recordAPIWebsocketHandshake(ctx, cfg, http.StatusSwitchingProtocols, http.Header{"Upgrade": {"websocket"}})
	appendAPIWebsocketResponse(ctx, cfg, []byte(`{"type":"response.completed"}`))
	recordAPIWebsocketError(ctx, cfg, "read", errCodexWebsocketTest)
	recordAPIWebsocketUpgradeRejection(
		ctx, cfg, websocketUpgradeRequestLog(info), http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}},
		[]byte(`{"error":"quota"}`),
	)

	timelineValue, ok := ginCtx.Get(apiWebsocketTimelineKey)
	if !ok {
		t.Fatal("missing API_WEBSOCKET_TIMELINE")
	}
	timeline := string(timelineValue.([]byte))
	for _, want := range []string{
		"Event: api.websocket.request",
		"Event: api.websocket.handshake",
		"Event: api.websocket.response",
		"Event: api.websocket.error",
		"Stage: read",
	} {
		if !strings.Contains(timeline, want) {
			t.Fatalf("timeline missing %q: %s", want, timeline)
		}
	}
	if strings.Contains(timeline, "sk-test-secret") {
		t.Fatalf("timeline leaked authorization header: %s", timeline)
	}

	requestValue, ok := ginCtx.Get(apiRequestKey)
	if !ok {
		t.Fatal("missing API_REQUEST")
	}
	requestLog := string(requestValue.([]byte))
	if !strings.Contains(requestLog, "HTTP Method: GET") || !strings.Contains(
		requestLog, "Upstream URL: https://api.openai.example/v1/responses",
	) {
		t.Fatalf("upgrade rejection request log mismatch: %s", requestLog)
	}
	responseValue, ok := ginCtx.Get(apiResponseKey)
	if !ok {
		t.Fatal("missing API_RESPONSE")
	}
	responseLog := string(responseValue.([]byte))
	if !strings.Contains(responseLog, "Status: 429") || !strings.Contains(responseLog, `{"error":"quota"}`) {
		t.Fatalf("upgrade rejection response log mismatch: %s", responseLog)
	}
}

var errCodexWebsocketTest = errString("codex websocket test error")

type errString string

func (e errString) Error() string { return string(e) }

func TestNewProxyAwareWebsocketDialerUnrecognizedSchemeKeepsDefault(t *testing.T) {
	t.Parallel()

	// "direct" has no URL scheme, so it falls through to the default case in the
	// switch and the dialer retains its initial Proxy: http.ProxyFromEnvironment.
	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy == nil {
		t.Fatal("expected proxy function to remain set (default http.ProxyFromEnvironment) for unrecognized scheme")
	}
}

func TestNewProxyAwareWebsocketDialerHTTPProxySetsProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "http://proxy.example.com:3128"},
	)

	if dialer.Proxy == nil {
		t.Fatal("expected proxy function to be set for HTTP proxy URL")
	}
}

func TestNormalizeCodexCompletionEventMapsResponseDone(t *testing.T) {
	payload := []byte(`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	normalized := normalizeCodexCompletionEvent(payload)

	if got := gjson.GetBytes(normalized, "type").String(); got != "response.completed" {
		t.Fatalf("type = %q, want %q", got, "response.completed")
	}
	if got := gjson.GetBytes(normalized, "response.status").String(); got != "completed" {
		t.Fatalf("response.status = %q, want %q", got, "completed")
	}
}
