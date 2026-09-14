package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestPreserveCodexPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"function_call_output","call_id":"call-1"}]}`)
	requestPayload := []byte(`{"previous_response_id":"resp-1"}`)

	restored, err := preserveCodexPreviousResponseID(body, requestPayload)
	if err != nil {
		t.Fatalf("preserveCodexPreviousResponseID() error = %v", err)
	}

	if got := gjson.GetBytes(restored, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Fatalf("input body was mutated: %s", body)
	}
}

func TestPreserveCodexPreviousResponseIDRejectsInvalidBody(t *testing.T) {
	_, err := preserveCodexPreviousResponseID([]byte(`{"model":`), []byte(`{"previous_response_id":"resp-1"}`))
	if err == nil {
		t.Fatal("preserveCodexPreviousResponseID() error = nil, want malformed body error")
	}
}

func TestCodexResponsesBodyPreservesPreviousResponseIDForHTTPFallback(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		prepared, err := codexResponsesBody(
			context.Background(),
			&config.Config{},
			&cliproxyauth.Auth{},
			"gpt-5-codex",
			"gpt-5-codex",
			"codex",
			sdktranslator.FormatOpenAIResponse,
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			cliproxyexecutor.Request{
				Model: "gpt-5-codex",
				Payload: []byte(
					`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":"next"}]}`,
				),
			},
			streaming,
		)
		if err != nil {
			t.Fatalf("codexResponsesBody(streaming=%t) error = %v", streaming, err)
		}
		if got := gjson.GetBytes(prepared.Body, "previous_response_id").String(); got != "resp-1" {
			t.Fatalf("codexResponsesBody(streaming=%t) previous_response_id = %q", streaming, got)
		}
	}
}

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

func TestCodexWebsocketsExecuteStreamRejectsUpgradeRequiredWithoutHTTPFallback(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					t.Errorf("unexpected HTTP fallback request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(http.StatusUpgradeRequired)
			},
		),
	)
	defer server.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		&cliproxyauth.Auth{
			ID:         "auth-upgrade-required",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{
			Model: "gpt-5-codex",
			Payload: []byte(
				`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"next"}]}`,
			),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if stream != nil {
		t.Fatalf("ExecuteStream() result = %#v, want nil", stream)
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusUpgradeRequired {
		t.Fatalf("ExecuteStream() error = %T %v, want status 426", err, err)
	}
}

func TestCodexWebsocketSessionRejectsStaleActiveConnection(t *testing.T) {
	sess := &codexWebsocketSession{sessionID: "session-1"}
	connA := &websocket.Conn{}
	connB := &websocket.Conn{}
	readCh := make(chan codexWebsocketRead, 1)

	sess.setActive(connA, readCh)
	sess.setActive(connB, readCh)
	sess.clearActive(connA, readCh)

	if _, _, ok := sess.activeForConn(connA); ok {
		t.Fatal("stale connection remained active")
	}
	if gotCh, _, ok := sess.activeForConn(connB); !ok || gotCh != readCh {
		t.Fatal("current connection was cleared by stale connection")
	}
}

func TestDeliverCodexWebsocketTerminalReadWaitsForConsumer(t *testing.T) {
	readCh := make(chan codexWebsocketRead, 1)
	readCh <- codexWebsocketRead{payload: []byte("queued")}
	done := make(chan struct{})
	delivered := make(chan struct{})
	terminalErr := errors.New("upstream disconnected")

	go func() {
		deliverCodexWebsocketTerminalRead(readCh, done, codexWebsocketRead{err: terminalErr})
		close(delivered)
	}()

	select {
	case <-delivered:
		t.Fatal("terminal read returned before the full channel had space")
	case <-time.After(50 * time.Millisecond):
	}

	<-readCh
	select {
	case event := <-readCh:
		if !errors.Is(event.err, terminalErr) {
			t.Fatalf("terminal event error = %v, want %v", event.err, terminalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal websocket error")
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("terminal delivery did not return after the consumer drained the channel")
	}
}

func TestExistingCodexWebsocketSessionRejectsRotatedCredential(t *testing.T) {
	conn := &websocket.Conn{}
	sess := &codexWebsocketSession{
		conn:             conn,
		authID:           "auth-a",
		wsURL:            "wss://example.com/responses",
		credentialSHA256: "credential-a",
	}
	if got := existingCodexWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-a"); got != conn {
		t.Fatal("matching credential did not reuse the connection")
	}
	if got := existingCodexWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-b"); got != nil {
		t.Fatal("rotated credential reused the old connection")
	}
}

func TestEnsureCodexUpstreamConnReconnectsWhenIdentityChanges(t *testing.T) {
	connectedA := make(chan struct{}, 1)
	connectedB := make(chan struct{}, 1)
	newServer := func(connected chan<- struct{}) *httptest.Server {
		return httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(
						w, r, nil,
					)
					if err != nil {
						return
					}
					connected <- struct{}{}
					defer func() { _ = conn.Close() }()
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
					}
				},
			),
		)
	}
	serverA := newServer(connectedA)
	defer serverA.Close()
	serverB := newServer(connectedB)
	defer serverB.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	sess := exec.getOrCreateSession("session-1")
	if sess == nil {
		t.Fatal("websocket session is nil")
	}
	wsURLA := "ws" + strings.TrimPrefix(serverA.URL, "http")
	wsURLB := "ws" + strings.TrimPrefix(serverB.URL, "http")
	connA, _, err := exec.ensureUpstreamConn(
		context.Background(), &cliproxyauth.Auth{ID: "auth-a"}, sess, "auth-a", wsURLA, http.Header{},
	)
	if err != nil {
		t.Fatalf("ensureUpstreamConn(auth-a) error = %v", err)
	}
	<-connectedA

	connB, _, err := exec.ensureUpstreamConn(
		context.Background(), &cliproxyauth.Auth{ID: "auth-b"}, sess, "auth-b", wsURLB, http.Header{},
	)
	if err != nil {
		t.Fatalf("ensureUpstreamConn(auth-b) error = %v", err)
	}
	<-connectedB
	defer func() { _ = connB.Close() }()

	if connA == connB {
		t.Fatal("connection was reused across auth and upstream URL change")
	}
	sess.connMu.Lock()
	gotAuthID := sess.authID
	gotWSURL := sess.wsURL
	sess.connMu.Unlock()
	if gotAuthID != "auth-b" || gotWSURL != wsURLB {
		t.Fatalf("session identity = %q %q, want auth-b %q", gotAuthID, gotWSURL, wsURLB)
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
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfiguredOAuthDefaults(t *testing.T) {
	cfg := &config.Config{
		Codex: config.CodexConfig{DisableCodexCloaking: true},
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

func TestApplyCodexWebsocketHeadersGPT56ModelOverrideUsesCurrentCodexVersion(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	applyModelHeaderOverrides(headers, "gpt-5.6-sol")

	if got := headers.Get("User-Agent"); got != "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Version"); got != "0.144.0" {
		t.Fatalf("Version = %q, want 0.144.0", got)
	}
	if got := headers.Get("Originator"); got != "codex-tui" {
		t.Fatalf("Originator = %q, want codex-tui", got)
	}
}

func TestApplyCodexWebsocketHeadersUsesCanonicalAccountHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1"}}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", nil)

	accountHeader := "ChatGPT-Account-ID"
	if got := headerValueCaseInsensitive(headers, accountHeader); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
	}
	values, ok := headers[accountHeader]
	if !ok {
		t.Fatalf("expected exact ChatGPT-Account-ID key, got %#v", headers)
	}
	if len(values) != 1 || values[0] != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID values = %#v, want [acct-1]", values)
	}
}

func TestApplyCodexPromptCacheHeadersSetsSessionAndConversation(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}

	_, headers := applyCodexPromptCacheHeaders(
		sdktranslator.FormatOpenAIResponse, req, []byte(`{"model":"gpt-5-codex"}`),
	)

	if got := headers.Get("Session_id"); got != "cache-1" {
		t.Fatalf("Session_id = %q, want cache-1", got)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %q, want cache-1", got)
	}
}

func TestApplyCodexPromptCacheHeadersClaudeGPT5UsesStableCache(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude-ws-session-1\"}"}}`),
	}

	body, headers := applyCodexPromptCacheHeaders(
		sdktranslator.FormatClaude, req, []byte(`{"model":"gpt-5.5"}`),
	)

	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey == "" {
		t.Fatalf("expected prompt_cache_key for gpt-5.5 Claude websocket path")
	}
	if got := headers.Get("Session_id"); got != gotKey {
		t.Fatalf("Session_id = %q, want %q", got, gotKey)
	}
	if got := headers.Get("Conversation_id"); got != gotKey {
		t.Fatalf("Conversation_id = %q, want %q", got, gotKey)
	}

	body2, _ := applyCodexPromptCacheHeaders(
		sdktranslator.FormatClaude, req, []byte(`{"model":"gpt-5.5"}`),
	)
	if gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String(); gotKey2 != gotKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, gotKey)
	}
}

func TestApplyCodexWebsocketHeadersKeepsPromptCacheSessionOverCodexClientSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	ginCtx.Request.Header.Set("Version", "0.131.0")
	ginCtx.Request.Header.Set("Session_id", "codex-client-session")

	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}
	_, headers := applyCodexPromptCacheHeaders(
		sdktranslator.FormatOpenAIResponse, req, []byte(`{"model":"gpt-5-codex"}`),
	)

	headers = applyCodexWebsocketHeaders(util.WithGinContext(context.Background(), ginCtx), headers, nil, "", nil)

	if got := headers.Get("Session_id"); got != "cache-1" {
		t.Fatalf("Session_id = %q, want prompt cache key", got)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %q, want prompt cache key", got)
	}
	if got := headers.Get("User-Agent"); got != ginCtx.Request.Header.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want Codex client value", got)
	}
}

func TestCodexWebsocketLoggingStoresTimelineAndUpgradeRejection(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	cfg := &config.Config{RequestLog: true}
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
	timeline := timelineValue.(*logging.BoundedLogBuffer).String()
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
	requestLog := requestValue.(*logging.BoundedLogBuffer).String()
	if !strings.Contains(requestLog, "HTTP Method: GET") || !strings.Contains(
		requestLog, "Upstream URL: https://api.openai.example/v1/responses",
	) {
		t.Fatalf("upgrade rejection request log mismatch: %s", requestLog)
	}
	responseValue, ok := ginCtx.Get(apiResponseKey)
	if !ok {
		t.Fatal("missing API_RESPONSE")
	}
	responseLog := responseValue.(*logging.BoundedLogBuffer).String()
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
		&config.Config{ProxyURL: "http://global-proxy.example.com:8080"},
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

func TestMapCodexWebsocketReadErrorMapsMessageTooBig(t *testing.T) {
	err := mapCodexWebsocketReadError(&websocket.CloseError{Code: websocket.CloseMessageTooBig})
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("mapped error = %#v, want 413 status", err)
	}
	if !strings.Contains(err.Error(), `"code":"message_too_big"`) {
		t.Fatalf("mapped error = %q, want message_too_big code", err.Error())
	}
}

func TestCodexWebsocketsCloseAllMarkerClosesSessions(t *testing.T) {
	exec := newCodexWebsocketsExecutor(&config.Config{})
	exec.sessions["session-1"] = &codexWebsocketSession{sessionID: "session-1"}

	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)

	if exec.sessions["session-1"] != nil {
		t.Fatal("CloseAllExecutionSessionsID kept active session")
	}
}

func TestCodexWebsocketsCloseExecutionSessionsForAuth(t *testing.T) {
	exec := newCodexWebsocketsExecutor(&config.Config{})
	exec.sessions["session-a"] = &codexWebsocketSession{sessionID: "session-a", authID: "auth-a"}
	exec.sessions["session-b"] = &codexWebsocketSession{sessionID: "session-b", authID: "auth-b"}

	exec.CloseExecutionSessionsForAuth("auth-a")

	if exec.sessions["session-a"] != nil || exec.sessions["session-b"] == nil {
		t.Fatalf("sessions after auth close = %#v", exec.sessions)
	}
}

func TestCodexWebsocketsExecuteStreamDownstreamWebsocketForwardsRawResponseDone(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Fatalf("Upgrade() error = %v", err)
				}
				defer func() { _ = conn.Close() }()

				_, _, err = conn.ReadMessage()
				if err != nil {
					t.Fatalf("ReadMessage() error = %v", err)
				}
				err = conn.WriteMessage(
					websocket.TextMessage,
					[]byte(`{"type":"response.done","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`),
				)
				if err != nil {
					t.Fatalf("WriteMessage() error = %v", err)
				}
			},
		),
	)
	defer server.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:         "auth-raw-ws",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var got [][]byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		got = append(got, bytes.Clone(chunk.Payload))
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(nil)
		}
	}
	if len(got) != 1 {
		t.Fatalf("chunks length = %d, want 1; chunks=%q", len(got), got)
	}
	if bytes.HasPrefix(bytes.TrimSpace(got[0]), []byte("data:")) {
		t.Fatalf("chunk = %s, want raw JSON without SSE data prefix", got[0])
	}
	if gotType := gjson.GetBytes(got[0], "type").String(); gotType != "response.done" {
		t.Fatalf("type = %q, want response.done; chunk=%s", gotType, got[0])
	}

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("usage record Failed = true, want false: %+v", record)
		}
		if record.Detail.TotalTokens != 3 {
			t.Fatalf("usage total tokens = %d, want 3", record.Detail.TotalTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestCodexWebsocketsExecuteStreamTerminalDeliveryFailureReportsFailedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)
	written := make(chan struct{})

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("Upgrade error: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Errorf("ReadMessage error: %v", err)
					return
				}
				err = conn.WriteMessage(
					websocket.TextMessage,
					[]byte(`{"type":"response.done","response":{"id":"resp-cancel","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`),
				)
				if err != nil {
					t.Errorf("WriteMessage error: %v", err)
					return
				}
				close(written)
				<-r.Context().Done()
			},
		),
	)
	defer server.Close()

	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := cliproxyexecutor.WithDownstreamWebsocket(baseCtx)
	exec := newCodexWebsocketsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:         "auth-cancel-ws",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	<-written

	select {
	case record := <-plugin.records:
		t.Fatalf("usage published before terminal delivery: %+v", record)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	for range stream.Chunks {
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestCodexWebsocketsExecuteStreamRejectsMalformedEvent(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("Upgrade error: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Errorf("ReadMessage error: %v", err)
					return
				}
				if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":`)); err != nil {
					t.Errorf("WriteMessage error: %v", err)
				}
			},
		),
	)
	defer server.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		&cliproxyauth.Auth{
			ID:         "auth-malformed-ws",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || statusErrCode(streamErr) != http.StatusBadGateway {
		t.Fatalf("stream error = %v, want 502", streamErr)
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}
