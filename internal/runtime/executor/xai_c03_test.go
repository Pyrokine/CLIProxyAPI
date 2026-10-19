package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var xaiReplayTestAuth = &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "offline-replay-credential"}}

func xaiC03ContextWithAPIKey(apiKey string) context.Context {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("apiKey", apiKey)
	return util.WithGinContext(context.Background(), ginCtx)
}

func TestXAIC03ReplayScopeIsolatesCallerAPIKeys(t *testing.T) {
	payload := []byte(`{"model":"grok-4.3","prompt_cache_key":"shared-session","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}

	scopeA := xaiReasoningReplayScopeFromRequest(
		xaiC03ContextWithAPIKey("caller-a"), xaiReplayTestAuth, opts.SourceFormat, req, opts, payload,
	)
	scopeB := xaiReasoningReplayScopeFromRequest(
		xaiC03ContextWithAPIKey("caller-b"), xaiReplayTestAuth, opts.SourceFormat, req, opts, payload,
	)
	if !scopeA.valid() || !scopeB.valid() {
		t.Fatalf("caller-isolated scopes must be valid: A=%+v B=%+v", scopeA, scopeB)
	}
	if scopeA.sessionKey == scopeB.sessionKey {
		t.Fatalf("different caller API keys share replay scope %q", scopeA.sessionKey)
	}
	if !strings.Contains(scopeA.sessionKey, "prompt-cache:shared-session") ||
		!strings.Contains(scopeB.sessionKey, "prompt-cache:shared-session") {
		t.Fatalf("isolated scopes lost session identity: A=%q B=%q", scopeA.sessionKey, scopeB.sessionKey)
	}
}

func TestXAIC03ChatReplayRequiresExplicitConversationIdentity(t *testing.T) {
	ctx := xaiC03ContextWithAPIKey("caller-a")
	body := []byte(`{"input":[{"role":"user","content":"hello"}]}`)
	request := cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	if scope := xaiReasoningReplayScopeFromRequest(ctx, xaiReplayTestAuth, options.SourceFormat, request, options, body); scope.valid() {
		t.Fatalf("chat without conversation identity has replay scope %+v", scope)
	}
	request.Payload = []byte(`{"prompt_cache_key":"conversation-1","messages":[{"role":"user","content":"hello"}]}`)
	scope := xaiReasoningReplayScopeFromRequest(ctx, xaiReplayTestAuth, options.SourceFormat, request, options, body)
	if !scope.valid() || !strings.Contains(scope.sessionKey, "conversation-1") {
		t.Fatalf("chat with explicit conversation identity has scope %+v", scope)
	}
	other := xaiReasoningReplayScopeFromRequest(xaiC03ContextWithAPIKey("caller-b"), xaiReplayTestAuth, options.SourceFormat, request, options, body)
	if !other.valid() || other.sessionKey == scope.sessionKey {
		t.Fatalf("chat callers share replay scope: A=%+v B=%+v", scope, other)
	}
}

func TestXAIC03ReplayCacheDoesNotCrossCallerAPIKeys(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	payload := []byte(`{"model":"grok-4.3","prompt_cache_key":"shared-session","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	scopeA := xaiReasoningReplayScopeFromRequest(
		xaiC03ContextWithAPIKey("caller-a"), xaiReplayTestAuth, opts.SourceFormat, req, opts, payload,
	)
	seedXAIReasoningReplay(t, scopeA)

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{name: "same caller", ctx: xaiC03ContextWithAPIKey("caller-a"), want: true},
		{name: "different caller", ctx: xaiC03ContextWithAPIKey("caller-b"), want: false},
		{name: "missing caller key", ctx: context.Background(), want: false},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				updated, _ := applyXAIReasoningReplayCache(tc.ctx, xaiReplayTestAuth, opts.SourceFormat, req, opts, payload)
				got := gjson.GetBytes(updated, "input.0.type").String() == "reasoning"
				if got != tc.want {
					t.Fatalf("reasoning replay injected = %v, want %v; body=%s", got, tc.want, updated)
				}
			},
		)
	}
}

func TestXAIC03ReplaySeparatesSelectedCredentialsAndPlanes(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	ctx := xaiC03ContextWithAPIKey("shared-caller")
	payload := []byte(`{"prompt_cache_key":"shared-session","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"},{"role":"user","content":"continue"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "credential-a", "base_url": "https://gateway.example/v1"}}
	scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, payload)
	if !scope.valid() || strings.Contains(scope.sessionKey, "credential-a") {
		t.Fatalf("invalid or unhashed credential scope: %+v", scope)
	}
	proof := validGrokEncryptedContentForExecutorTest()
	items := [][]byte{
		[]byte(`{"type":"reasoning","summary":[],"encrypted_content":""}`),
		[]byte(`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}`),
	}
	items[0], _ = sjson.SetBytes(items[0], "encrypted_content", proof)
	if !internalcache.XAIReasoningReplayItems(scope.modelName, scope.sessionKey, items) {
		t.Fatal("failed to seed credential-scoped replay")
	}

	for _, tt := range []struct {
		name string
		auth *cliproxyauth.Auth
		want bool
	}{
		{"same credential", auth, true},
		{"different credential", &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "credential-b", "base_url": "https://gateway.example/v1"}}, false},
		{"different endpoint", &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "credential-a", "base_url": "https://other.example/v1"}}, false},
		{"different plane", &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "credential-a", "base_url": "https://gateway.example/v1", "using_api": "false"}}, false},
		{"missing credential", &cliproxyauth.Auth{Provider: "xai"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequestWithContext(ctx, tt.auth, req, opts, false, sdktranslator.FormatCodex)
			if err != nil {
				t.Fatal(err)
			}
			got := gjson.GetBytes(prepared.body, "input.0.encrypted_content").String() == proof
			if got != tt.want {
				t.Fatalf("replay injected = %t, want %t; body=%s", got, tt.want, prepared.body)
			}
			if tt.want && prepared.reasoningReplayScope.sessionKey != scope.sessionKey {
				t.Fatalf("request scope = %q, want %q", prepared.reasoningReplayScope.sessionKey, scope.sessionKey)
			}
		})
	}
}

func TestXAIC03ReplaySeparatesPhysicalTransports(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	ctx := xaiC03ContextWithAPIKey("shared-caller")
	payload := []byte(`{"model":"grok-4.6","prompt_cache_key":"shared-session","input":[{"role":"user","content":"continue"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	for _, tt := range []struct {
		name string
		auth *cliproxyauth.Auth
	}{
		{"CLI chat versus API WebSocket", &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{
			"auth_kind": "oauth", "api_key": "offline-replay-token",
		}}},
		{"same custom endpoint across transports", &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{
			"auth_kind": "oauth", "api_key": "offline-replay-token", "base_url": "https://gateway.example/v1",
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, websocketBaseURL := xaiCreds(tt.auth)
			if websocketBaseURL == "" {
				websocketBaseURL = xaiauth.DefaultAPIBaseURL
			}
			httpScope := xaiReasoningReplayScopeFromRequest(ctx, tt.auth, opts.SourceFormat, req, opts, payload)
			websocketScope := xaiReasoningReplayScopeFromRequest(ctx, tt.auth, opts.SourceFormat, req, opts, payload, websocketBaseURL)
			compactOpts := opts
			compactOpts.Alt = "responses/compact"
			compactScope := xaiReasoningReplayScopeFromRequest(ctx, tt.auth, opts.SourceFormat, req, compactOpts, payload)
			if !httpScope.valid() || !websocketScope.valid() || !compactScope.valid() ||
				httpScope.sessionKey == websocketScope.sessionKey || httpScope.sessionKey == compactScope.sessionKey ||
				websocketScope.sessionKey == compactScope.sessionKey {
				t.Fatalf("physical replay scopes overlap: HTTP=%+v WebSocket=%+v compact=%+v", httpScope, websocketScope, compactScope)
			}
			seedXAIReasoningReplay(t, httpScope)
			httpPrepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequestWithContext(
				ctx, tt.auth, req, opts, false, sdktranslator.FormatCodex,
			)
			if err != nil {
				t.Fatal(err)
			}
			if httpPrepared.reasoningReplayScope.sessionKey != httpScope.sessionKey ||
				gjson.GetBytes(httpPrepared.body, "input.0.type").String() != "reasoning" {
				t.Fatalf("HTTP replay not isolated to its route: scope=%+v body=%s", httpPrepared.reasoningReplayScope, httpPrepared.body)
			}
			websocketPrepared, err := newXAIWebsocketsExecutor(&config.Config{}).prepareResponsesWebsocketRequest(ctx, tt.auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			if websocketPrepared.reasoningReplayScope.sessionKey != websocketScope.sessionKey ||
				gjson.GetBytes(websocketPrepared.body, "input.0.type").String() == "reasoning" {
				t.Fatalf("WebSocket reused HTTP proof: scope=%+v body=%s", websocketPrepared.reasoningReplayScope, websocketPrepared.body)
			}
			compactPrepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequestWithContext(
				ctx, tt.auth, req, compactOpts, false, sdktranslator.FormatOpenAIResponse,
			)
			if err != nil {
				t.Fatal(err)
			}
			if compactPrepared.reasoningReplayScope.sessionKey != compactScope.sessionKey ||
				gjson.GetBytes(compactPrepared.body, "input.0.type").String() == "reasoning" {
				t.Fatalf("compact reused HTTP proof: scope=%+v body=%s", compactPrepared.reasoningReplayScope, compactPrepared.body)
			}
			internalcache.ClearXAIReasoningReplayCache()
		})
	}
}

func TestXAIC03ReplayScopeDisablesUnisolatedSessions(t *testing.T) {
	payload := []byte(`{"model":"grok-4.3","prompt_cache_key":"shared-session","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	for _, source := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse} {
		scope := xaiReasoningReplayScopeFromRequest(
			context.Background(), xaiReplayTestAuth, source, req, cliproxyexecutor.Options{SourceFormat: source}, payload,
		)
		if scope.valid() {
			t.Fatalf("source %s without caller API key has replay scope %+v", source.String(), scope)
		}
	}

	trusted := xaiReasoningReplayScopeFromRequest(
		context.Background(), xaiReplayTestAuth, sdktranslator.FormatClaude, req, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatClaude,
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "trusted-session",
			},
		}, payload,
	)
	if !strings.HasSuffix(trusted.sessionKey, ":execution:trusted-session") {
		t.Fatalf("trusted execution scope = %q, want credential-isolated execution:trusted-session", trusted.sessionKey)
	}
}

func TestXAIC03ReplayScopeSkipsIncrementalWebsocketHistory(t *testing.T) {
	ctx := cliproxyexecutor.WithDownstreamWebsocket(xaiC03ContextWithAPIKey("caller-a"))
	payload := []byte(`{"model":"grok-4.3","prompt_cache_key":"session","previous_response_id":"resp_1","input":[{"role":"user","content":"next"}]}`)
	scope := xaiReasoningReplayScopeFromRequest(
		ctx,
		xaiReplayTestAuth,
		sdktranslator.FormatOpenAIResponse,
		cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		payload,
	)
	if scope.valid() {
		t.Fatalf("incremental websocket request unexpectedly enabled replay: %+v", scope)
	}
}

func TestXAIC03ReplayRejectsAmbiguousAssistantHistory(t *testing.T) {
	body := []byte(`{"input":[{"role":"assistant","content":"current answer"},{"role":"user","content":"next"}]}`)
	items := [][]byte{
		[]byte(`{"type":"reasoning","summary":[],"encrypted_content":"opaque"}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"cached answer"}]}`),
	}
	if filtered := filterXAIReasoningReplayItemsForInput(body, items); len(filtered) != 0 {
		t.Fatalf("ambiguous assistant history replayed %q", filtered)
	}
}

func TestXAIC03ReplayMatchesRoleOnlyAssistantHistory(t *testing.T) {
	body := []byte(`{"input":[{"role":"assistant","content":"same answer"},{"role":"user","content":"next"}]}`)
	items := [][]byte{
		[]byte(`{"type":"reasoning","summary":[],"encrypted_content":"opaque"}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"same answer"}]}`),
	}
	filtered := filterXAIReasoningReplayItemsForInput(body, items)
	if len(filtered) != 1 || gjson.GetBytes(filtered[0], "type").String() != "reasoning" {
		t.Fatalf("matching role-only assistant filtered replay = %q, want reasoning only", filtered)
	}
	updated, ok := insertCodexReasoningReplayItems(body, filtered)
	if !ok {
		t.Fatal("insertCodexReasoningReplayItems failed")
	}
	if got := gjson.GetBytes(updated, "input.0.type").String(); got != "reasoning" {
		t.Fatalf("replay inserted after role-only assistant: %s", updated)
	}
	if got := gjson.GetBytes(updated, "input.1.role").String(); got != "assistant" {
		t.Fatalf("role-only assistant moved unexpectedly: %s", updated)
	}
}

func TestXAIC03CompactClearsOnlyCorrespondingReplay(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	var gotPath string
	var gotTokenAuth string
	var gotClientVersion string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotTokenAuth = r.Header.Get(xaiTokenAuthHeader)
				gotClientVersion = r.Header.Get(xaiClientVersionHeader)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_compact","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-out"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			},
		),
	)
	defer server.Close()

	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai", Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  server.URL,
			"api_key":   "oauth-token",
		},
	}
	ctx := xaiC03ContextWithAPIKey("compact-caller")
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: "responses/compact"}
	payload := []byte(`{"model":"grok-4.3","prompt_cache_key":"compact-session","input":[{"role":"user","content":"compact"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, payload)
	otherPayload := []byte(`{"model":"grok-4.3","prompt_cache_key":"other-session","input":[{"role":"user","content":"keep"}]}`)
	otherReq := cliproxyexecutor.Request{Model: "grok-4.3", Payload: otherPayload}
	otherScope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, otherReq, opts, otherPayload)

	seedXAIReasoningReplay(t, scope)
	seedXAIReasoningReplay(t, otherScope)
	if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
		t.Fatalf("executeCompactRequest error: %v", err)
	}
	if _, ok := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); ok {
		t.Fatal("successful compact retained corresponding replay")
	}
	if _, ok := internalcache.GetXAIReasoningReplayItems(otherScope.modelName, otherScope.sessionKey); !ok {
		t.Fatal("successful compact deleted unrelated replay")
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("compact path = %q, want /responses/compact", gotPath)
	}
	if gotTokenAuth != "" || gotClientVersion != "" {
		t.Fatalf("compact sent CLI identity headers: token-auth=%q client-version=%q", gotTokenAuth, gotClientVersion)
	}
}

func TestXAIC03CompactFailureRetainsReplay(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"compact failed"}}`))
			},
		),
	)
	defer server.Close()

	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai", Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "xai-token",
		},
	}
	ctx := xaiC03ContextWithAPIKey("compact-caller")
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: "responses/compact"}
	payload := []byte(`{"model":"grok-4.3","prompt_cache_key":"compact-session","input":[{"role":"user","content":"compact"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, payload)
	seedXAIReasoningReplay(t, scope)

	if _, err := exec.Execute(ctx, auth, req, opts); err == nil {
		t.Fatal("executeCompactRequest error = nil, want upstream failure")
	}
	if _, ok := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); !ok {
		t.Fatal("failed compact deleted replay")
	}
}

func TestXAIC03CompactBaseURL(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{name: "empty", auth: &cliproxyauth.Auth{Provider: "xai"}, want: xaiauth.DefaultAPIBaseURL},
		{
			name: "official",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": xaiauth.DefaultAPIBaseURL}},
			want: xaiauth.DefaultAPIBaseURL,
		},
		{
			name: "cli proxy",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": xaiauth.CLIChatProxyBaseURL + "/"}},
			want: xaiauth.DefaultAPIBaseURL,
		},
		{
			name: "custom",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://gateway.example/v1"}},
			want: "https://gateway.example/v1",
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := xaiCompactBaseURL(tt.auth); got != tt.want {
					t.Fatalf("xaiCompactBaseURL() = %q, want %q", got, tt.want)
				}
			},
		)
	}
}

func seedXAIReasoningReplay(t *testing.T, scope xaiReasoningReplayScope) {
	t.Helper()
	if !scope.valid() {
		t.Fatal("cannot seed invalid xAI replay scope")
	}
	reasoning := []byte(`{"type":"reasoning","summary":[],"encrypted_content":""}`)
	reasoning, _ = sjson.SetBytes(reasoning, "encrypted_content", validGrokEncryptedContentForExecutorTest())
	if !internalcache.XAIReasoningReplayItems(scope.modelName, scope.sessionKey, [][]byte{reasoning}) {
		t.Fatal("failed to seed xAI replay cache")
	}
}
