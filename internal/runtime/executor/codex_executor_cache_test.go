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
		sdktranslator.FromString("openai-response"), cliproxyexecutor.Options{}, req, true, false,
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

func TestCodexResponsesBodyInstructionFidelity(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		opts        cliproxyexecutor.Options
		wantNative  bool
		wantPresent bool
		wantType    gjson.Type
		wantValue   string
	}{
		{
			name:        "compatibility missing instructions",
			payload:     `{"model":"gpt-5.3-codex","input":[]}`,
			opts:        cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			wantPresent: true,
			wantType:    gjson.String,
		},
		{
			name:        "compatibility null instructions",
			payload:     `{"model":"gpt-5.3-codex","instructions":null,"input":[]}`,
			opts:        cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			wantPresent: true,
			wantType:    gjson.String,
		},
		{
			name:       "native header missing instructions",
			payload:    `{"model":"gpt-5.3-codex","input":[]}`,
			opts:       cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"x-openai-internal-codex-responses-lite": []string{"true"}}},
			wantNative: true,
		},
		{
			name:        "native header null instructions",
			payload:     `{"model":"gpt-5.3-codex","instructions":null,"input":[]}`,
			opts:        cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"}}},
			wantNative:  true,
			wantPresent: true,
			wantType:    gjson.Null,
		},
		{
			name:       "native metadata missing instructions",
			payload:    `{"model":"gpt-5.3-codex","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true},"input":[]}`,
			opts:       cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			wantNative: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: []byte(testCase.payload)}
			prepared, err := codexResponsesBody(
				context.Background(), &config.Config{}, nil, "gpt-5.3-codex", req.Model, "codex",
				sdktranslator.FromString("openai-response"), testCase.opts, req, true, false,
			)
			if err != nil {
				t.Fatalf("codexResponsesBody error: %v", err)
			}
			if prepared.Native != testCase.wantNative {
				t.Fatalf("Native = %t, want %t", prepared.Native, testCase.wantNative)
			}

			instructions := gjson.GetBytes(prepared.Body, "instructions")
			if instructions.Exists() != testCase.wantPresent {
				t.Fatalf("instructions present = %t, want %t in %s", instructions.Exists(), testCase.wantPresent, string(prepared.Body))
			}
			if !testCase.wantPresent {
				return
			}
			if instructions.Type != testCase.wantType || instructions.String() != testCase.wantValue {
				t.Fatalf("instructions = %s (%v), want %q (%v) in %s", instructions.Raw, instructions.Type, testCase.wantValue, testCase.wantType, string(prepared.Body))
			}
		})
	}
}

func TestCodexResponsesBodyResponsesLiteHeaderDoesNotInjectImageGenerationTool(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}
	headers := http.Header{}
	headers.Set(codexResponsesLiteHeader, "true")

	prepared, err := codexResponsesBody(
		context.Background(), &config.Config{}, nil, "gpt-5.6-sol", req.Model, "codex",
		sdktranslator.FromString("codex"), cliproxyexecutor.Options{Headers: headers}, req, true, false,
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
		sdktranslator.FromString("openai-response"), cliproxyexecutor.Options{}, req, true, false,
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
		ctx, sdktranslator.FromString("openai-response"), url, nil, req, req.Payload, rawJSON, nil,
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
	if gotSession := httpReq.Header.Get("Session-Id"); gotSession != expectedKey {
		t.Fatalf("Session-Id = %q, want %q", gotSession, expectedKey)
	}
}

func TestApplyModelHeaderOverridesUsesEmbeddedModelConfig(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)
	headers.Set("Version", codexClientVersion)

	applyModelHeaderOverrides(headers, "gpt-5.6-luna")

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	if got := headers.Get("Version"); got != codexClientVersion {
		t.Fatalf("Version = %q, want %q", got, codexClientVersion)
	}
	if got := headers.Get("Session-Id"); got == "" {
		t.Fatal("Session_id is empty after Mac OS User-Agent override")
	}
}

func TestApplyModelHeaderOverridesKeepsCurrentClientVersionForAstra(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)
	headers.Set("Version", codexClientVersion)

	applyModelHeaderOverrides(headers, "gpt-6-astra")

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	if got := headers.Get("Version"); got != codexClientVersion {
		t.Fatalf("Version = %q, want %q", got, codexClientVersion)
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
		[]byte(`{"model":"gpt-5.3-codex"}`), nil,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	applyCodexHeaders(httpReq, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "token", true, nil)

	if got := httpReq.Header.Get("Session-Id"); got != expectedKey {
		t.Fatalf("Session-Id = %q, want prompt cache key %q", got, expectedKey)
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
		req.Payload, rawJSON, nil,
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
	if got := httpReq.Header.Get("Session-Id"); got != identityState.promptCacheKey {
		t.Fatalf("Session-Id = %q, want %q", got, identityState.promptCacheKey)
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
		clientBody, clientBody, nil,
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
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON, nil,
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
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON, nil,
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
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, differentModelJSON, nil,
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
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON, nil,
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
	if got := httpReq.Header.Get("Session-Id"); got != gotKey {
		t.Fatalf("Session-Id = %q, want %q", got, gotKey)
	}

	rawJSON2 := []byte(`{"model":"gpt-5.5","stream":true}`)
	httpReq2, _, _, err := executor.cacheHelper(
		ctx, sdktranslator.FromString("claude"), url, nil, req, req.Payload, rawJSON2, nil,
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
		ctx, sdktranslator.FromString("openai"), url, nil, req, req.Payload, rawJSON, nil,
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
	if got := httpReq.Header.Get("Session-Id"); got != gotKey {
		t.Fatalf("Session-Id = %q, want %q", got, gotKey)
	}
}

func TestCodexExecutorGPT6SolDoesNotInjectVersionForClaudeCode(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
				))
			},
		),
	)
	defer server.Close()

	executor := newCodexExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "acct-1",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-6-sol",
		Payload: []byte(`{"model":"gpt-6-sol","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers: http.Header{
			"User-Agent": []string{"claude-cli/2.1.278 (external, cli)"},
			"Version":    []string{"0.154.0"},
		},
	}

	if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gotHeaders.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty for Claude Code gpt-6-sol request", got)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want Bearer oauth-token", got)
	}
	if got := headerValueCaseInsensitive(gotHeaders, "ChatGPT-Account-ID"); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
	}
	if got := gotHeaders.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := gotHeaders.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
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
	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty for non-Codex client", got)
	}
	if got := req.Header.Get("Session-Id"); got == "" || got == "cherry-session" {
		t.Fatalf("Session-Id = %q, want generated non-Cherry value", got)
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
	if got := req.Header.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %q, want empty for non-Mac configured UA", got)
	}
	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty without trusted client version", got)
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
	if got := req.Header.Get("Session-Id"); got != "codex-session" {
		t.Fatalf("Session-Id = %q, want Codex client value", got)
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
	if got := req.Header.Get("Session-Id"); got != "codex-session" {
		t.Fatalf("Session-Id = %q, want legacy Codex client value", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want legacy Codex client value", got)
	}
}

type capturedCodexRequest struct {
	path        string
	model       gjson.Result
	routingHint string
	hasHint     bool
	serviceTier gjson.Result
}

func newCodexRoutingHintServer(t *testing.T, captured *capturedCodexRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.path = r.URL.Path
		captured.model = gjson.GetBytes(body, "model")
		_, captured.hasHint = r.Header[http.CanonicalHeaderKey(codexRoutingHintHeader)]
		captured.routingHint = r.Header.Get(codexRoutingHintHeader)
		captured.serviceTier = gjson.GetBytes(body, "service_tier")
		if r.URL.Path == "/responses/compact" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmp_1","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
}

func codexOAuthTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"base_url": baseURL},
		Metadata:   map[string]any{"type": "codex", "access_token": "oauth-token", "account_id": "acct"},
	}
}

func codexAPIKeyTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"base_url": baseURL, "api_key": "sk-test"},
	}
}

// A Claude Messages request with speed=fast (Claude Code Fast Mode) must reach
// the ChatGPT Codex backend the way native Codex 0.155 sends Fast: the body
// carries service_tier=priority and the routing hint names the same tier.
func TestCodexExecutorRoutingHintCarriesRequestedTier(t *testing.T) {
	const fastClaude = `{"model":"gpt-5.5","max_tokens":64,"speed":"fast","messages":[{"role":"user","content":"hi"}]}`
	const plainClaude = `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	const aliasedClaude = `{"model":"client-alias","max_tokens":64,"speed":"fast","messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name     string
		apiKey   bool
		stream   bool
		model    string
		payload  string
		wantHint string
		wantTier string
	}{
		{name: "oauth stream fast", stream: true, payload: fastClaude, wantHint: "model=gpt-5.5;tier=priority", wantTier: "priority"},
		{name: "oauth non-stream fast", stream: false, payload: fastClaude, wantHint: "model=gpt-5.5;tier=priority", wantTier: "priority"},
		{name: "oauth stream alias and thinking suffix", stream: true, model: "gpt-5.5(low)", payload: aliasedClaude, wantHint: "model=gpt-5.5;tier=priority", wantTier: "priority"},
		{name: "oauth non-stream alias and thinking suffix", model: "gpt-5.5(low)", payload: aliasedClaude, wantHint: "model=gpt-5.5;tier=priority", wantTier: "priority"},
		{name: "oauth stream standard", stream: true, payload: plainClaude, wantHint: "model=gpt-5.5"},
		{name: "api key keeps body tier without hint", apiKey: true, stream: true, payload: fastClaude, wantTier: "priority"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured capturedCodexRequest
			server := newCodexRoutingHintServer(t, &captured)
			defer server.Close()

			auth := codexOAuthTestAuth(server.URL)
			if tc.apiKey {
				auth = codexAPIKeyTestAuth(server.URL)
			}
			executor := newCodexExecutor(&config.Config{})
			model := tc.model
			if model == "" {
				model = "gpt-5.5"
			}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(tc.payload)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), Stream: tc.stream}

			if tc.stream {
				result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream error: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error: %v", chunk.Err)
					}
				}
			} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
				t.Fatalf("Execute error: %v", err)
			}

			if captured.model.Type != gjson.String || captured.model.String() != "gpt-5.5" {
				t.Fatalf("body model = %s, want gpt-5.5", captured.model.Raw)
			}
			if tc.wantHint == "" {
				if captured.hasHint {
					t.Fatalf("routing hint = %q, want header absent", captured.routingHint)
				}
			} else if captured.routingHint != tc.wantHint {
				t.Fatalf("routing hint = %q, want %q", captured.routingHint, tc.wantHint)
			}
			if tc.wantTier == "" {
				if captured.serviceTier.Exists() {
					t.Fatalf("body service_tier = %s, want absent", captured.serviceTier.Raw)
				}
			} else if captured.serviceTier.String() != tc.wantTier {
				t.Fatalf("body service_tier = %q, want %q", captured.serviceTier.String(), tc.wantTier)
			}
		})
	}
}

func TestCodexExecutorCompactRoutingHintCarriesRequestedTier(t *testing.T) {
	var captured capturedCodexRequest
	server := newCodexRoutingHintServer(t, &captured)
	defer server.Close()

	executor := newCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), codexOAuthTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.5(low)",
		Payload: []byte(`{"model":"client-alias","service_tier":"priority","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
	})
	if err != nil {
		t.Fatalf("compact Execute error: %v", err)
	}
	if captured.path != "/responses/compact" {
		t.Fatalf("upstream path = %q, want /responses/compact", captured.path)
	}
	if captured.model.Type != gjson.String || captured.model.String() != "gpt-5.5" {
		t.Fatalf("body model = %s, want gpt-5.5", captured.model.Raw)
	}
	if captured.serviceTier.String() != "priority" {
		t.Fatalf("body service_tier = %q, want priority", captured.serviceTier.String())
	}
	if captured.routingHint != "model="+captured.model.String()+";tier="+captured.serviceTier.String() {
		t.Fatalf("routing hint = %q, does not match body model %q and tier %q", captured.routingHint, captured.model.String(), captured.serviceTier.String())
	}
}

func TestCodexExecutorOperatorRoutingHintRuleWins(t *testing.T) {
	var captured capturedCodexRequest
	server := newCodexRoutingHintServer(t, &captured)
	defer server.Close()

	auth := codexOAuthTestAuth(server.URL)
	auth.Attributes["header:X-Codex-Routing-Hint"] = "model=operator-pinned"
	clientHeaders := http.Header{}
	clientHeaders.Set(codexRoutingHintHeader, "model=gpt-5.4-client")
	executor := newCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","max_tokens":64,"speed":"fast","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), Stream: true, Headers: clientHeaders})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	if captured.routingHint != "model=operator-pinned" {
		t.Fatalf("routing hint = %q, want operator rule value", captured.routingHint)
	}
}

func TestApplyCodexRoutingHint(t *testing.T) {
	oauth := codexOAuthTestAuth("")

	t.Run("replaces a hint that did not come from an operator rule", func(t *testing.T) {
		headers := http.Header{}
		headers.Set(codexRoutingHintHeader, "model=gpt-5.4-client")
		applyCodexRoutingHint(context.Background(), headers, oauth, "gpt-5.5", []byte(`{"model":"gpt-5.5","service_tier":"priority"}`), nil)
		if got := headers.Get(codexRoutingHintHeader); got != "model=gpt-5.5;tier=priority" {
			t.Fatalf("routing hint = %q, want %q", got, "model=gpt-5.5;tier=priority")
		}
	})

	t.Run("keeps the value from an operator auth header rule", func(t *testing.T) {
		operator := codexOAuthTestAuth("")
		operator.Attributes["header:x-codex-routing-hint"] = "model=operator"
		headers := http.Header{}
		headers.Set(codexRoutingHintHeader, "model=operator")
		applyCodexRoutingHint(context.Background(), headers, operator, "gpt-5.5", []byte(`{"model":"gpt-5.5","service_tier":"priority"}`), nil)
		if got := headers.Get(codexRoutingHintHeader); got != "model=operator" {
			t.Fatalf("routing hint = %q, want operator value kept", got)
		}
	})

	t.Run("derives the hint when a dynamic operator rule resolves to nothing", func(t *testing.T) {
		operator := codexOAuthTestAuth("")
		operator.Attributes["header:X-Codex-Routing-Hint"] = "$X-Operator-Hint"
		headers := http.Header{}
		headers.Set(codexRoutingHintHeader, "model=gpt-5.4-client")
		applyCodexRoutingHint(context.Background(), headers, operator, "gpt-5.5", []byte(`{"model":"gpt-5.5","service_tier":"priority"}`), nil)
		if got := headers.Get(codexRoutingHintHeader); got != "model=gpt-5.5;tier=priority" {
			t.Fatalf("routing hint = %q, want %q", got, "model=gpt-5.5;tier=priority")
		}
	})

	t.Run("leaves API-key requests untouched", func(t *testing.T) {
		headers := http.Header{}
		headers.Set(codexRoutingHintHeader, "model=client")
		applyCodexRoutingHint(context.Background(), headers, codexAPIKeyTestAuth(""), "gpt-5.5", []byte(`{"model":"gpt-5.5","service_tier":"priority"}`), nil)
		if got := headers.Get(codexRoutingHintHeader); got != "model=client" {
			t.Fatalf("routing hint = %q, want API-key headers unchanged", got)
		}
	})

	t.Run("drops a forwarded hint when the resolved model is empty", func(t *testing.T) {
		headers := http.Header{}
		headers.Set(codexRoutingHintHeader, "model=gpt-5.4-client")
		applyCodexRoutingHint(context.Background(), headers, oauth, "", []byte(`{"model":"gpt-5.5","service_tier":"priority"}`), nil)
		if _, ok := headers[http.CanonicalHeaderKey(codexRoutingHintHeader)]; ok {
			t.Fatalf("routing hint = %q, want header absent", headers.Get(codexRoutingHintHeader))
		}
	})

	t.Run("ignores a non-string tier", func(t *testing.T) {
		headers := http.Header{}
		applyCodexRoutingHint(context.Background(), headers, oauth, "gpt-5.5", []byte(`{"model":"gpt-5.5","service_tier":null}`), nil)
		if got := headers.Get(codexRoutingHintHeader); got != "model=gpt-5.5" {
			t.Fatalf("routing hint = %q, want %q", got, "model=gpt-5.5")
		}
	})
}
