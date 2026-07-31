package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestReadXAIResponseBodyRejectsOversizedBody(t *testing.T) {
	_, err := readXAIResponseBody(strings.NewReader("abcde"), 4)
	if err == nil {
		t.Fatal("readXAIResponseBody oversized body error = nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("readXAIResponseBody oversized body error = %v", err)
	}
}

func TestXAIExecutorAppliesThinkingSuffix(t *testing.T) {
	executor := NewXAIExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3(low)",
		Payload: []byte(`{"model":"grok-4.3(low)","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	prepared, err := executor.prepareResponsesRequest(req, opts, true)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	if got := gjson.GetBytes(prepared.body, "model").String(); got != "grok-4.3" {
		t.Fatalf("model = %q, want %q, body=%s", got, "grok-4.3", prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want %q, body=%s", got, "low", prepared.body)
	}
}

func TestXAIReasoningEffortUsesRegistryThinkingMetadata(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-xai-registry-thinking"
	reg.RegisterClient(
		clientID, "xai", []*registry.ModelInfo{
			{ID: "custom-xai-thinking", Type: "xai", Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}},
			{ID: "custom-xai-no-thinking", Type: "xai"},
		},
	)
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	if !xaiSupportsReasoningEffort("custom-xai-thinking") {
		t.Fatal("custom-xai-thinking should support reasoning effort from registry thinking metadata")
	}
	if xaiSupportsReasoningEffort("custom-xai-no-thinking") {
		t.Fatal("custom-xai-no-thinking should not support reasoning effort without registry thinking metadata")
	}
}

func TestSanitizeXAIResponsesBodyOnlyStripsReasoningEffort(t *testing.T) {
	body := sanitizeXAIResponsesBody(
		[]byte(`{"reasoning":{"effort":"low","summary":"auto"},"include":["reasoning.encrypted_content","web_search_call.action.sources"]}`),
		"custom-xai-no-thinking",
	)

	if gjson.GetBytes(body, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be removed: %s", body)
	}
	if got := gjson.GetBytes(body, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "include.0").String(); got != "web_search_call.action.sources" {
		t.Fatalf("include = %s, want only web_search_call.action.sources", gjson.GetBytes(body, "include").Raw)
	}
}

func TestXAIReasoningTextEventsNormalizeToReasoningSummary(t *testing.T) {
	events := xaiNormalizeReasoningSummaryDataEvents([]byte(`{"type":"response.reasoning_text.done","content_index":2,"text":"internal"}`))
	if len(events) != 2 {
		t.Fatalf("events length = %d, want 2", len(events))
	}
	if got := gjson.GetBytes(events[0], "type").String(); got != "response.reasoning_summary_text.done" {
		t.Fatalf("events[0].type = %q, want response.reasoning_summary_text.done", got)
	}
	if got := gjson.GetBytes(events[0], "summary_index").Int(); got != 2 {
		t.Fatalf("events[0].summary_index = %d, want 2", got)
	}
	if gjson.GetBytes(events[0], "content_index").Exists() {
		t.Fatalf("events[0].content_index should be removed: %s", string(events[0]))
	}
	if got := gjson.GetBytes(events[1], "type").String(); got != "response.reasoning_summary_part.done" {
		t.Fatalf("events[1].type = %q, want response.reasoning_summary_part.done", got)
	}
	if got := gjson.GetBytes(events[1], "part.type").String(); got != "summary_text" {
		t.Fatalf("events[1].part.type = %q, want summary_text", got)
	}
	if got := gjson.GetBytes(events[1], "part.text").String(); got != "internal" {
		t.Fatalf("events[1].part.text = %q, want internal", got)
	}
	if got := string(
		xaiNormalizeReasoningSummaryEventLine(
			[]byte("event: response.reasoning_text.delta"), "",
		),
	); got != "event: response.reasoning_summary_text.delta" {
		t.Fatalf("event line = %q", got)
	}
}

func TestXAIExecutorComposerSessionIsolation(t *testing.T) {
	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	tests := []struct {
		name          string
		model         string
		payload       []byte
		wantGenerated bool
		wantSession   string
	}{
		{
			name:          "composer generates fresh session",
			model:         "grok-composer-2.5-fast",
			payload:       []byte(`{"model":"grok-composer-2.5-fast","input":"hello"}`),
			wantGenerated: true,
		},
		{
			name:    "grok build stays stateless without session",
			model:   "grok-build-0.1",
			payload: []byte(`{"model":"grok-build-0.1","input":"hello"}`),
		},
		{
			name:        "explicit prompt cache key is preserved",
			model:       "grok-composer-2.5-fast",
			payload:     []byte(`{"model":"grok-composer-2.5-fast","prompt_cache_key":"client-session","input":"hello"}`),
			wantSession: "client-session",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				prepared, err := exec.prepareResponsesRequestWithContext(
					context.Background(), cliproxyexecutor.Request{
						Model:   tt.model,
						Payload: tt.payload,
					}, cliproxyexecutor.Options{
						SourceFormat: sdktranslator.FormatOpenAIResponse,
						Stream:       true,
					}, true, sdktranslator.FormatCodex,
				)
				if err != nil {
					t.Fatalf("prepareResponsesRequestWithContext() error = %v", err)
				}

				gotSession := prepared.sessionID
				gotPromptCacheKey := gjson.GetBytes(prepared.body, "prompt_cache_key").String()
				httpReq, errRequest := http.NewRequest(
					http.MethodPost, "https://example.test/responses", bytes.NewReader(prepared.body),
				)
				if errRequest != nil {
					t.Fatalf("NewRequest() error = %v", errRequest)
				}
				applyXAIHeaders(httpReq, auth, "xai-token", true, gotSession)
				gotGrokConvID := httpReq.Header.Get("x-grok-conv-id")

				if tt.wantGenerated {
					if _, errParse := uuid.Parse(gotSession); errParse != nil {
						t.Fatalf("generated sessionID = %q, want UUID; body=%s", gotSession, string(prepared.body))
					}
					if gotPromptCacheKey != gotSession {
						t.Fatalf(
							"prompt_cache_key = %q, want sessionID %q; body=%s", gotPromptCacheKey, gotSession,
							string(prepared.body),
						)
					}
					if gotGrokConvID != gotSession {
						t.Fatalf("x-grok-conv-id = %q, want sessionID %q", gotGrokConvID, gotSession)
					}
					return
				}

				if tt.wantSession != "" {
					if gotSession != tt.wantSession {
						t.Fatalf("sessionID = %q, want %q", gotSession, tt.wantSession)
					}
					if gotPromptCacheKey != tt.wantSession {
						t.Fatalf(
							"prompt_cache_key = %q, want %q; body=%s", gotPromptCacheKey, tt.wantSession,
							string(prepared.body),
						)
					}
					if gotGrokConvID != tt.wantSession {
						t.Fatalf("x-grok-conv-id = %q, want %q", gotGrokConvID, tt.wantSession)
					}
					return
				}

				if gotSession != "" {
					t.Fatalf("sessionID = %q, want empty", gotSession)
				}
				if gotPromptCacheKey != "" {
					t.Fatalf("prompt_cache_key = %q, want empty; body=%s", gotPromptCacheKey, string(prepared.body))
				}
				if gotGrokConvID != "" {
					t.Fatalf("x-grok-conv-id = %q, want empty", gotGrokConvID)
				}
			},
		)
	}
}

func TestXAIExecutorCompactUsesCompactEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotAccept string
	var gotBody []byte

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotAccept = r.Header.Get("Accept")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-out"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			},
		),
	)
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "xai-token",
		},
	}

	resp, err := exec.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","stream":true,"input":[{"type":"compaction","encrypted_content":"opaque-in"},{"role":"user","content":"hello"}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Alt:          "responses/compact",
			Stream:       false,
		},
	)
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	if gotAuth != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream exists in compact body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.0.role").String(); got != "user" {
		t.Fatalf("invalid compaction item reached xai compact body: %s", string(gotBody))
	}
	want := `{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-out"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	if string(resp.Payload) != want {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestXAIExecutorExecuteStreamCompactionTriggerUsesCompactEndpoint(t *testing.T) {
	var gotPath string
	var gotAccept string
	var gotBody []byte

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAccept = r.Header.Get("Accept")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_xai_1","model":"grok-4.3","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			},
		),
	)
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "xai-token",
		},
	}

	result, err := exec.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","stream":true,"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Stream:       true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream compaction trigger error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if xaiInputHasItemType(gotBody, "compaction_trigger") {
		t.Fatalf("compaction_trigger reached xai compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream exists in compact body: %s", string(gotBody))
	}

	var streamed bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	output := streamed.String()
	for _, eventName := range []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	} {
		if !strings.Contains(output, "event: "+eventName+"\n") {
			t.Fatalf("missing %s event in stream: %s", eventName, output)
		}
	}
	if !strings.Contains(output, `"type":"compaction"`) || !strings.Contains(output, `"encrypted_content":"opaque"`) {
		t.Fatalf("compaction output missing from stream: %s", output)
	}
	if !strings.Contains(output, `"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}`) {
		t.Fatalf("usage missing from completed stream: %s", output)
	}
}

func TestXAIChatBaseURL(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{name: "nil auth", want: xaiauth.DefaultAPIBaseURL},
		{name: "api key default", auth: &cliproxyauth.Auth{Provider: "xai"}, want: xaiauth.DefaultAPIBaseURL},
		{
			name: "OAuth default",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"auth_kind": "oauth",
				"base_url":  xaiauth.DefaultAPIBaseURL,
			}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "metadata OAuth default",
			auth: &cliproxyauth.Auth{Metadata: map[string]any{
				"auth_kind": "oauth",
				"base_url":  xaiauth.DefaultAPIBaseURL,
			}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "using api false",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{xaiUsingAPIAttr: "false"}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "metadata using api false",
			auth: &cliproxyauth.Auth{Metadata: map[string]any{xaiUsingAPIAttr: false}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "OAuth using api true",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"auth_kind":     "oauth",
				xaiUsingAPIAttr: "true",
			}},
			want: xaiauth.DefaultAPIBaseURL,
		},
		{
			name: "custom gateway",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"auth_kind":     "oauth",
				"base_url":      "https://gateway.example.com/v1",
				xaiUsingAPIAttr: "false",
			}},
			want: "https://gateway.example.com/v1",
		},
		{
			name: "invalid using api follows OAuth default",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"auth_kind":     "oauth",
				xaiUsingAPIAttr: "invalid",
			}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := xaiChatBaseURL(tt.auth); got != tt.want {
				t.Fatalf("xaiChatBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestXAIURLNormalization(t *testing.T) {
	for _, rawURL := range []string{
		xaiauth.CLIChatProxyBaseURL,
		xaiauth.CLIChatProxyBaseURL + "/",
		"https://CLI-CHAT-PROXY.GROK.COM/v1",
		"https://cli-chat-proxy.grok.com:443/v1",
	} {
		if !xaiIsCLIChatProxyBaseURL(rawURL) {
			t.Fatalf("xaiIsCLIChatProxyBaseURL(%q) = false", rawURL)
		}
	}
	if xaiIsCLIChatProxyBaseURL("https://cli-chat-proxy.grok.com/v2") {
		t.Fatal("xaiIsCLIChatProxyBaseURL accepted a different path")
	}
}

func TestApplyXAIRefreshTokenPreservesCustomGateway(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"source": "/tmp/xai.json",
		},
		Metadata: map[string]any{
			"auth_kind":     "oauth",
			"refresh_token": "refresh-custom-gateway",
			"base_url":      "https://gateway.example.com/v1",
			xaiUsingAPIAttr: false,
		},
	}

	refreshed := applyXAIRefreshToken(
		auth,
		&xaiauth.TokenData{AccessToken: "refreshed-token", ExpiresIn: 3600},
		"https://auth.x.ai/oauth2/token",
		"https://gateway.example.com/v1",
	)
	if got := refreshed.Attributes["base_url"]; got != "https://gateway.example.com/v1" {
		t.Fatalf("attribute base_url = %q, want custom gateway", got)
	}
	if got := xaiMetadataString(refreshed.Metadata, "base_url"); got != "https://gateway.example.com/v1" {
		t.Fatalf("metadata base_url = %q, want custom gateway", got)
	}
	if got := xaiChatBaseURL(refreshed); got != "https://gateway.example.com/v1" {
		t.Fatalf("xaiChatBaseURL() after refresh = %q, want custom gateway", got)
	}
}

func TestApplyXAIRefreshTokenSynchronizesTokenStorage(t *testing.T) {
	storage := &xaiauth.TokenStorage{
		Type:          "xai",
		AccessToken:   "old-access",
		RefreshToken:  "old-refresh",
		IDToken:       "old-id",
		TokenType:     "Bearer",
		ExpiresIn:     60,
		Expire:        "old-expiry",
		LastRefresh:   "old-refresh-time",
		Email:         "old@example.com",
		Subject:       "old-subject",
		BaseURL:       "https://gateway.example.com/v1",
		TokenEndpoint: "https://auth.x.ai/oauth2/token",
		AuthKind:      "oauth",
	}
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Storage:  storage,
		Metadata: map[string]any{
			"auth_kind":     "oauth",
			"refresh_token": "old-refresh",
			"base_url":      "https://gateway.example.com/v1",
		},
	}
	tokenData := &xaiauth.TokenData{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		IDToken:      "new-id",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		Expire:       "new-expiry",
		Email:        "new@example.com",
		Subject:      "new-subject",
	}

	applyXAIRefreshToken(
		auth,
		tokenData,
		"https://auth.x.ai/oauth2/token",
		"https://gateway.example.com/v1",
	)

	if storage.AccessToken != tokenData.AccessToken || storage.RefreshToken != tokenData.RefreshToken ||
		storage.IDToken != tokenData.IDToken || storage.ExpiresIn != tokenData.ExpiresIn ||
		storage.Expire != tokenData.Expire || storage.Email != tokenData.Email ||
		storage.Subject != tokenData.Subject {
		t.Fatalf("xAI token storage was not synchronized: %#v", storage)
	}
	if storage.LastRefresh == "" || storage.LastRefresh == "old-refresh-time" {
		t.Fatalf("xAI token storage LastRefresh = %q", storage.LastRefresh)
	}
	if storage.Metadata == nil || storage.Metadata["access_token"] != tokenData.AccessToken ||
		storage.Metadata["refresh_token"] != tokenData.RefreshToken {
		t.Fatalf("xAI token storage metadata = %#v", storage.Metadata)
	}
}

func TestXAIExecutorPrepareRequestAppliesChatProxyIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://CLI-CHAT-PROXY.GROK.COM:443/v1/responses", nil)
	auth := &cliproxyauth.Auth{
		Metadata: map[string]any{"access_token": "xai-token"},
		Attributes: map[string]string{
			"header:" + xaiClientVersionHeader: "custom-client-version",
		},
	}

	executor := NewXAIExecutor(&config.Config{})
	if err := executor.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", got)
	}
	if got := req.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
		t.Fatalf("%s = %q, want %q", xaiTokenAuthHeader, got, xaiTokenAuthValue)
	}
	if got := req.Header.Get(xaiClientVersionHeader); got != "custom-client-version" {
		t.Fatalf("%s = %q, want custom-client-version", xaiClientVersionHeader, got)
	}
}

func TestApplyXAIChatHeaders(t *testing.T) {
	t.Run("OAuth chat proxy", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"auth_kind": "oauth"}}
		applyXAIChatHeaders(req, auth, "xai-token", true, "conv-1")

		if got := req.Header.Get("Authorization"); got != "Bearer xai-token" {
			t.Fatalf("Authorization = %q, want Bearer xai-token", got)
		}
		if got := req.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
			t.Fatalf("%s = %q, want %q", xaiTokenAuthHeader, got, xaiTokenAuthValue)
		}
		if got := req.Header.Get(xaiClientVersionHeader); got != xaiClientVersionValue {
			t.Fatalf("%s = %q, want %q", xaiClientVersionHeader, got, xaiClientVersionValue)
		}
	})

	t.Run("custom headers override chat proxy", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{
			"auth_kind":                        "oauth",
			"header:Authorization":             "blocked-custom-auth",
			"header:" + xaiTokenAuthHeader:     "custom-token-auth",
			"header:" + xaiClientVersionHeader: "custom-client-version",
		}}
		applyXAIChatHeaders(req, auth, "xai-token", true, "")

		if got := req.Header.Get("Authorization"); got != "Bearer xai-token" {
			t.Fatalf("Authorization = %q, want protected Bearer token", got)
		}
		if got := req.Header.Get(xaiTokenAuthHeader); got != "custom-token-auth" {
			t.Fatalf("%s = %q, want custom-token-auth", xaiTokenAuthHeader, got)
		}
		if got := req.Header.Get(xaiClientVersionHeader); got != "custom-client-version" {
			t.Fatalf("%s = %q, want custom-client-version", xaiClientVersionHeader, got)
		}
	})

	t.Run("using API suppresses chat proxy identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "https://CLI-CHAT-PROXY.GROK.COM:443/v1/responses", nil)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{
			"auth_kind":     "oauth",
			xaiUsingAPIAttr: "true",
		}}
		applyXAIChatHeaders(req, auth, "xai-token", true, "")

		if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
			t.Fatalf("%s = %q, want empty", xaiTokenAuthHeader, got)
		}
		if got := req.Header.Get(xaiClientVersionHeader); got != "" {
			t.Fatalf("%s = %q, want empty", xaiClientVersionHeader, got)
		}
	})

	for _, tt := range []struct {
		name string
		auth *cliproxyauth.Auth
	}{
		{name: "official API", auth: &cliproxyauth.Auth{Attributes: map[string]string{xaiUsingAPIAttr: "true"}}},
		{name: "custom gateway", auth: &cliproxyauth.Auth{Attributes: map[string]string{
			"auth_kind": "oauth", "base_url": "https://gateway.example.com/v1",
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
			applyXAIChatHeaders(req, tt.auth, "xai-token", false, "")
			if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
				t.Fatalf("%s = %q, want empty", xaiTokenAuthHeader, got)
			}
			if got := req.Header.Get(xaiClientVersionHeader); got != "" {
				t.Fatalf("%s = %q, want empty", xaiClientVersionHeader, got)
			}
		})
	}
}

func TestNormalizeXAIToolsSimplifiesCodexAutomationSchema(t *testing.T) {
	largeParameters := `{"oneOf":[{"type":"object","properties":{"mode":{"type":"string"}}}],"$defs":{"mode":{"type":"string"}}}`
	body := []byte(`{"tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"automation_update","strict":true,"parameters":` + largeParameters + `}]},{"type":"function","name":"exec_command","strict":true,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`)

	tools := gjson.GetBytes(normalizeXAITools(body), "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2", len(tools))
	}
	for _, tool := range tools {
		switch tool.Get("name").String() {
		case "automation_update":
			if tool.Get("parameters.type").String() != "object" ||
				tool.Get("parameters.additionalProperties").Type != gjson.True {
				t.Fatalf("automation_update parameters not simplified: %s", tool.Raw)
			}
			if tool.Get("strict").Type != gjson.False {
				t.Fatalf("automation_update strict = %s, want false", tool.Get("strict").Raw)
			}
		case "exec_command":
			if got := tool.Get("parameters.properties.cmd.type").String(); got != "string" {
				t.Fatalf("exec_command schema changed: %s", tool.Raw)
			}
		}
	}
}

func TestNormalizeXAIToolsPreservesUnrelatedAutomationSchemas(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "top-level",
			body: []byte(`{"tools":[{"type":"function","name":"automation_update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}`),
		},
		{
			name: "different namespace",
			body: []byte(`{"tools":[{"type":"namespace","name":"calendar","tools":[{"type":"function","name":"automation_update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}]}`),
		},
		{
			name: "different namespace case",
			body: []byte(`{"tools":[{"type":"namespace","name":"Codex_App","tools":[{"type":"function","name":"automation_update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}]}`),
		},
		{
			name: "different function case",
			body: []byte(`{"tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"Automation_Update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}]}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := gjson.GetBytes(normalizeXAITools(tt.body), "tools.0")
			if tool.Get("strict").Type != gjson.True {
				t.Fatalf("strict changed: %s", tool.Raw)
			}
			if got := tool.Get("parameters.properties.cron.type").String(); got != "string" {
				t.Fatalf("parameters changed: %s", tool.Raw)
			}
			if tool.Get("parameters.additionalProperties").Type != gjson.False {
				t.Fatalf("additionalProperties changed: %s", tool.Raw)
			}
		})
	}
}

func TestXAIStatusErrSetsFreeUsageCooldown(t *testing.T) {
	err := xaiStatusErr(
		http.StatusTooManyRequests,
		[]byte(`{"code":"subscription:free-usage-exhausted","error":"included free usage exhausted"}`),
	)
	if err.retryAfter == nil || *err.retryAfter != xaiFreeUsageExhaustedCooldown {
		t.Fatalf("retryAfter = %v, want %v", err.retryAfter, xaiFreeUsageExhaustedCooldown)
	}

	generic := xaiStatusErr(http.StatusTooManyRequests, []byte(`{"error":"rate limited"}`))
	if generic.retryAfter != nil {
		t.Fatalf("generic retryAfter = %v, want nil", generic.retryAfter)
	}
}
