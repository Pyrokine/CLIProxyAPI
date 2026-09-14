package executor

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func validGPTReasoningSignatureForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestCodexExecutorCompactDropsInvalidReasoningEncryptedContent(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				captured = append([]byte(nil), body...)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","output":[]}`))
			},
		),
	)
	defer server.Close()

	executor := &codexExecutor{}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model: "gpt-5.3-codex", Payload: []byte(`{
		"model":"gpt-5.3-codex",
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"not-a-gpt-signature","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`),
	}
	opts := cliproxyexecutor.Options{
		Alt: "responses/compact", SourceFormat: sdktranslator.FromString("openai-response"),
	}

	_, err := executor.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	if gjson.GetBytes(captured, "input.0.encrypted_content").Exists() {
		t.Fatalf("compact upstream body kept invalid encrypted_content: %s", captured)
	}
}

func TestCodexExecutorCompactNormalizesInstructions(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name: "missing",
			payload: `{
				"model":"gpt-5.3-codex",
				"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]
			}`,
		},
		{
			name: "null",
			payload: `{
				"model":"gpt-5.3-codex",
				"instructions": null,
				"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				var captured []byte
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Fatalf("read request body: %v", err)
							}
							captured = append([]byte(nil), body...)
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(`{"id":"resp_1","output":[]}`))
						},
					),
				)
				defer server.Close()

				executor := &codexExecutor{}
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: []byte(tt.payload)}
				opts := cliproxyexecutor.Options{
					Alt: "responses/compact", SourceFormat: sdktranslator.FromString("openai-response"),
				}

				_, err := executor.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("Execute compact error: %v", err)
				}
				instructions := gjson.GetBytes(captured, "instructions")
				if !instructions.Exists() || instructions.Type != gjson.String || instructions.String() != "" {
					t.Fatalf("instructions = %s, want empty string in %s", instructions.Raw, string(captured))
				}
			},
		)
	}
}

func TestCodexExecutorCompactDoesNotInjectImageToolOrReorderInput(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				captured = append([]byte(nil), body...)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","output":[]}`))
			},
		),
	)
	defer server.Close()

	executor := &codexExecutor{cfg: &config.Config{}}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":"history"},
				{"type":"compaction_trigger"}
			]
		}`),
	}
	opts := cliproxyexecutor.Options{
		Alt: "responses/compact", SourceFormat: sdktranslator.FromString("openai-response"),
	}

	if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	if gjson.GetBytes(captured, "tools").Exists() {
		t.Fatalf("compact request injected tools: %s", captured)
	}
	input := gjson.GetBytes(captured, "input").Array()
	if len(input) != 2 {
		t.Fatalf("compact input len = %d, want 2: %s", len(input), captured)
	}
	if input[0].Get("type").String() != "message" || input[1].Get("type").String() != "compaction_trigger" {
		t.Fatalf("compact input order changed: %s", captured)
	}
}

func TestCodexResponsesBodyDropsInvalidReasoningEncryptedContent(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.3-codex",
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"not-a-gpt-signature","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}

	prepared, err := codexResponsesBody(
		context.Background(), &config.Config{}, nil, "gpt-5.3-codex", req.Model, "codex",
		sdktranslator.FromString("openai-response"), cliproxyexecutor.Options{}, req, true,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody error: %v", err)
	}

	if gjson.GetBytes(prepared.Body, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content was not removed: %s", string(prepared.Body))
	}
	if got := gjson.GetBytes(prepared.Body, "input.1.content.0.text").String(); got != "continue" {
		t.Fatalf("message text = %q, want continue", got)
	}
}

func TestCodexResponsesBodyNormalizesNullInstructions(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.3-codex",
		"instructions": null,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]
	}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}

	prepared, err := codexResponsesBody(
		context.Background(), &config.Config{}, nil, "gpt-5.3-codex", req.Model, "codex",
		sdktranslator.FromString("openai-response"), cliproxyexecutor.Options{}, req, true,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody error: %v", err)
	}

	instructions := gjson.GetBytes(prepared.Body, "instructions")
	if !instructions.Exists() || instructions.Type != gjson.String || instructions.String() != "" {
		t.Fatalf("instructions = %s, want empty string in %s", instructions.Raw, string(prepared.Body))
	}
}

func TestCodexResponsesBodyResponsesLiteHeaderDoesNotInjectImageGenerationTool(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}
	headers := http.Header{}
	headers.Set(codexResponsesLiteHeader, "true")

	prepared, err := codexResponsesBody(
		context.Background(), &config.Config{}, nil, "gpt-5.6-sol", req.Model, "codex",
		sdktranslator.FromString("codex"), cliproxyexecutor.Options{Headers: headers}, req, true,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody error: %v", err)
	}

	if gjson.GetBytes(prepared.Body, "tools").Exists() {
		t.Fatalf("expected responses-lite body without injected tools, got %s", string(prepared.Body))
	}
}

func TestCodexResponsesBodyPreservesValidReasoningEncryptedContent(t *testing.T) {
	signature := validGPTReasoningSignatureForTest()
	payload := []byte(`{
		"model":"gpt-5.3-codex",
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"` + signature + `","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}

	prepared, err := codexResponsesBody(
		context.Background(), &config.Config{}, nil, "gpt-5.3-codex", req.Model, "codex",
		sdktranslator.FromString("openai-response"), cliproxyexecutor.Options{}, req, true,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody error: %v", err)
	}

	if got := gjson.GetBytes(prepared.Body, "input.0.encrypted_content").String(); got != signature {
		t.Fatalf("encrypted_content = %q, want valid signature", got)
	}
}

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

	httpReq, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("openai-response"), url, nil, req, req.Payload, rawJSON,
	)
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

func TestApplyModelHeaderOverridesUsesEmbeddedModelConfig(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)

	applyModelHeaderOverrides(headers, "gpt-5.6-luna")

	if got := headers.Get("User-Agent"); got != "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Originator"); got != "codex-tui" {
		t.Fatalf("Originator = %q, want codex-tui", got)
	}
	if got := headers.Get("Version"); got != "0.144.0" {
		t.Fatalf("Version = %q, want 0.144.0", got)
	}
	if got := headers.Get("Session_id"); got == "" {
		t.Fatal("Session_id is empty after Mac OS User-Agent override")
	}
}

func TestApplyModelHeaderOverridesUsesAstraMinimumClientVersion(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)

	applyModelHeaderOverrides(headers, "gpt-6-astra")

	if got := headers.Get("User-Agent"); got != "codex-tui/0.153.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.153.0)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Originator"); got != "codex-tui" {
		t.Fatalf("Originator = %q, want codex-tui", got)
	}
	if got := headers.Get("Version"); got != "0.153.0" {
		t.Fatalf("Version = %q, want 0.153.0", got)
	}
	if got := headers.Get("Session_id"); got == "" {
		t.Fatal("Session_id is empty after Astra User-Agent override")
	}
}

func TestApplyCodexHeadersKeepsPromptCacheSessionOverCodexClientSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	ginCtx.Request.Header.Set("Version", "0.131.0")
	ginCtx.Request.Header.Set("Session_id", "codex-client-session")

	ctx := util.WithGinContext(context.Background(), ginCtx)
	executor := &codexExecutor{}
	expectedKey := "prompt-cache-session"
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","prompt_cache_key":"` + expectedKey + `"}`),
	}

	httpReq, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", nil, req, req.Payload,
		[]byte(`{"model":"gpt-5.3-codex"}`),
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	applyCodexHeaders(httpReq, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "token", true, nil)

	if got := httpReq.Header.Get("Session_id"); got != expectedKey {
		t.Fatalf("Session_id = %q, want prompt cache key %q", got, expectedKey)
	}
	if got := httpReq.Header.Get("Conversation_id"); got != expectedKey {
		t.Fatalf("Conversation_id = %q, want prompt cache key %q", got, expectedKey)
	}
	if got := httpReq.Header.Get("User-Agent"); got != ginCtx.Request.Header.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want Codex client value", got)
	}
}

func TestCodexExecutorCacheHelper_IdentityConfuseRemapsBodyAndHeaders(t *testing.T) {
	cfg, err := config.ParseConfigBytes(
		[]byte(`routing:
  strategy: fill-first
codex:
  identity-confuse: true
`),
	)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	executor := &codexExecutor{cfg: cfg}
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","prompt_cache_key":"client-cache","client_metadata":{"x-codex-installation-id":"client-installation","x-codex-turn-metadata":"{\"prompt_cache_key\":\"client-cache\",\"turn_id\":\"turn-1\",\"window_id\":\"client-cache:0\"}"}}`),
	}
	rawJSON := []byte(`{"model":"gpt-5.3-codex","prompt_cache_key":"client-cache","client_metadata":{"x-codex-installation-id":"client-installation","x-codex-turn-metadata":"{\"prompt_cache_key\":\"client-cache\",\"turn_id\":\"turn-1\",\"window_id\":\"client-cache:0\"}"}}`)

	httpReq, upstreamBody, identityState, err := executor.cacheHelper(
		context.Background(), sdktranslator.FromString("openai-response"), "https://example.com/responses", auth, req,
		req.Payload, rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	if string(body) != string(upstreamBody) {
		t.Fatalf("request body and returned upstream body differ")
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got == "client-cache" || got == "" {
		t.Fatalf("prompt_cache_key = %q, want remapped non-empty value", got)
	}
	if strings.Contains(string(body), "client-installation") {
		t.Fatalf("upstream body leaked original installation id: %s", string(body))
	}
	if got := httpReq.Header.Get("Session_id"); got != identityState.promptCacheKey {
		t.Fatalf("Session_id = %q, want %q", got, identityState.promptCacheKey)
	}

	upstreamPayload := []byte(`{"prompt_cache_key":"client-cache","turn_id":"turn-1"}`)
	confusedPayload := applyCodexIdentityConfuseResponsePayload(upstreamPayload, identityState)
	if strings.Contains(string(confusedPayload), "client-cache") || strings.Contains(
		string(confusedPayload), "turn-1",
	) {
		t.Fatalf("confused payload leaked original identity: %s", string(confusedPayload))
	}
	exposedPayload := applyCodexIdentityExposeResponsePayload(confusedPayload, identityState)
	if !strings.Contains(string(exposedPayload), "client-cache") || !strings.Contains(
		string(exposedPayload), "turn-1",
	) {
		t.Fatalf("exposed payload did not restore client identity: %s", string(exposedPayload))
	}
}

func TestCodexIdentityConfuseKeepsClientBodySeparateFromUpstreamBody(t *testing.T) {
	cfg, err := config.ParseConfigBytes(
		[]byte(`routing:
  session-affinity: true
codex:
  identity-confuse: true
`),
	)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	executor := &codexExecutor{cfg: cfg}
	auth := &cliproxyauth.Auth{ID: "auth-2"}
	clientBody := []byte(`{"model":"gpt-5.3-codex","prompt_cache_key":"client-cache"}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: clientBody}

	_, upstreamBody, identityState, err := executor.cacheHelper(
		context.Background(), sdktranslator.FromString("openai-response"), "https://example.com/responses", auth, req,
		clientBody, clientBody,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	if got := gjson.GetBytes(clientBody, "prompt_cache_key").String(); got != "client-cache" {
		t.Fatalf("client body prompt_cache_key = %q, want original", got)
	}
	if got := gjson.GetBytes(upstreamBody, "prompt_cache_key").String(); got == "client-cache" || got == "" {
		t.Fatalf("upstream prompt_cache_key = %q, want remapped non-empty value", got)
	}

	exposed := applyCodexIdentityExposeResponsePayload(
		[]byte(`{"prompt_cache_key":"`+identityState.promptCacheKey+`"}`), identityState,
	)
	if got := gjson.GetBytes(exposed, "prompt_cache_key").String(); got != "client-cache" {
		t.Fatalf("exposed prompt_cache_key = %q, want original", got)
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

	httpReq, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON,
	)
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

	// A repeated request for the same resolved model must keep the cache key stable.
	httpReq2, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON,
	)
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

	// A different provider-facing model must not share the same prompt cache.
	differentModelJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	differentModelReq, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, differentModelJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper error (different model): %v", err)
	}
	differentModelBody, errReadDifferent := io.ReadAll(differentModelReq.Body)
	if errReadDifferent != nil {
		t.Fatalf("read request body (different model): %v", errReadDifferent)
	}
	if differentModelKey := gjson.GetBytes(
		differentModelBody, "prompt_cache_key",
	).String(); differentModelKey == gotKey {
		t.Fatalf("different resolved models shared prompt_cache_key %q", gotKey)
	}
}

func TestCodexExecutorCacheHelper_Claude_GPT5UsesStablePromptCache(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	executor := &codexExecutor{}

	rawJSON := []byte(`{"model":"gpt-5.5","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","metadata":{"user_id":"{\"session_id\":\"claude-session-1\"}"}}`),
	}
	url := "https://example.com/messages"

	httpReq, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey == "" {
		t.Fatalf("expected prompt_cache_key for gpt-5.5 Claude path")
	}
	if got := httpReq.Header.Get("Conversation_id"); got != gotKey {
		t.Fatalf("Conversation_id = %q, want %q", got, gotKey)
	}
	if got := httpReq.Header.Get("Session_id"); got != gotKey {
		t.Fatalf("Session_id = %q, want %q", got, gotKey)
	}

	rawJSON2 := []byte(`{"model":"gpt-5.5","stream":true}`)
	httpReq2, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON2,
	)
	if err != nil {
		t.Fatalf("cacheHelper error (second call): %v", err)
	}
	body2, errRead2 := io.ReadAll(httpReq2.Body)
	if errRead2 != nil {
		t.Fatalf("read request body (second call): %v", errRead2)
	}
	if gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String(); gotKey2 != gotKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, gotKey)
	}
}

func TestCodexExecutorCacheHelper_OpenAIChatCompletions_UsesAPIKeyPromptCache(t *testing.T) {
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

	httpReq, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("openai"), url, nil, req, req.Payload, rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey == "" {
		t.Fatalf("expected prompt_cache_key for openai format")
	}
	if got := httpReq.Header.Get("Conversation_id"); got != gotKey {
		t.Fatalf("Conversation_id = %q, want %q", got, gotKey)
	}
	if got := httpReq.Header.Get("Session_id"); got != gotKey {
		t.Fatalf("Session_id = %q, want %q", got, gotKey)
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
		Codex:               config.CodexConfig{DisableCodexCloaking: true},
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

func TestApplyCodexHeadersUsesAccountHeaderForOAuth(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1"}}

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	accountHeader := "ChatGPT-Account-ID"
	if got := headerValueCaseInsensitive(req.Header, accountHeader); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
	}
	values, ok := req.Header[accountHeader]
	if !ok {
		t.Fatalf("expected exact ChatGPT-Account-ID key, got %#v", req.Header)
	}
	if len(values) != 1 || values[0] != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID values = %#v, want [acct-1]", values)
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

func TestApplyCodexHeadersPreservesLegacyOriginatorOnlyClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
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

	if got := req.Header.Get("Version"); got != "0.131.0" {
		t.Fatalf("Version = %q, want legacy Codex client value", got)
	}
	if got := req.Header.Get("Session_id"); got != "codex-session" {
		t.Fatalf("Session_id = %q, want legacy Codex client value", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want legacy Codex client value", got)
	}
}
