package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type apiKeyCompatRoundTripper func(*http.Request) (*http.Response, error)

func (f apiKeyCompatRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAICompatExecutorUsesResolvedModelIsCompat(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(),
		apiKeyCompatRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatalf("read upstream body: %v", errRead)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
						),
					),
					Request: req,
				}, nil
			},
		),
	)

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": "https://openai-compatible.example.com",
			"api_key":  "test-key",
		},
	}
	request := cliproxyexecutor.Request{
		Model: "deepseek-v4-flash",
		Payload: []byte(
			`{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior reasoning","signature":"opaque-signature"},{"type":"text","text":"Reading files."},{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`,
		),
		Metadata: map[string]any{
			"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
		},
	}
	options := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
	}

	if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}

	assistant := gjson.GetBytes(upstreamBody, "messages.0")
	if got := assistant.Get("reasoning_content").String(); got != "prior reasoning" {
		t.Fatalf("reasoning_content = %q, want prior reasoning; body=%s", got, upstreamBody)
	}
	if got := assistant.Get("content.0.text").String(); got != "Reading files." {
		t.Fatalf("assistant content = %q, want Reading files.; body=%s", got, upstreamBody)
	}
	if !assistant.Get("tool_calls").Exists() {
		t.Fatalf("tool_calls missing from upstream request: %s", upstreamBody)
	}
}

func TestCodexExecutorUsesResolvedModelIsCompat(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(),
		apiKeyCompatRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatalf("read upstream body: %v", errRead)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(
						strings.NewReader(
							"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n",
						),
					),
					Request: req,
				}, nil
			},
		),
	)

	executor := NewCodexAutoExecutor(&config.Config{Codex: config.CodexConfig{OptimizeMultiAgentV2: true}})
	request := cliproxyexecutor.Request{
		Model: "deepseek-v4-flash",
		Payload: []byte(
			`{"model":"deepseek-v4-flash","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"delegated task"}]}]}`,
		),
		Metadata: map[string]any{
			"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
		},
	}
	options := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"User-Agent": []string{"codex-tui/0.145.0"}},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://codex-compatible.example.com",
			"api_key":  "test-key",
		},
	}

	if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}

	message := gjson.GetBytes(upstreamBody, "input.0")
	if got := message.Get("type").String(); got != "message" {
		t.Fatalf("input.0.type = %q, want message; body=%s", got, upstreamBody)
	}
	if got := message.Get("role").String(); got != "user" {
		t.Fatalf("input.0.role = %q, want user; body=%s", got, upstreamBody)
	}
	if got := message.Get("content.0.text").String(); got != "delegated task" {
		t.Fatalf("input.0.content.0.text = %q, want delegated task; body=%s", got, upstreamBody)
	}
}

func TestClaudeExecutorUsesResolvedModelIsCompat(t *testing.T) {
	tests := []struct {
		name                 string
		baseURL              string
		wantAuthorization    string
		wantAPIKey           string
		wantAnthropicVersion string
	}{
		{
			name:                 "DeepSeek Anthropic endpoint",
			baseURL:              "https://api.deepseek.com/anthropic",
			wantAPIKey:           "test-key",
			wantAnthropicVersion: "",
		},
		{
			name:                 "other Claude-compatible endpoint",
			baseURL:              "https://claude-compatible.example.com",
			wantAuthorization:    "Bearer test-key",
			wantAnthropicVersion: "2023-06-01",
		},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				executor := NewClaudeExecutor(&config.Config{})
				attributes := map[string]string{
					"base_url": testCase.baseURL,
					"api_key":  "test-key",
				}
				if testCase.wantAnthropicVersion == "" {
					attributes["header:Anthropic-Version"] = "custom-version"
					attributes["header:X-Api-Key"] = "custom-key"
				}
				prepared, err := executor.prepareClaudeRequest(
					context.Background(),
					&cliproxyauth.Auth{Attributes: attributes},
					cliproxyexecutor.Request{
						Model: "deepseek-v4",
						Payload: []byte(
							`{"model":"deepseek-v4","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"prior reasoning"}],"encrypted_content":"opaque-signature"}]}`,
						),
						Metadata: map[string]any{
							"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
						},
					},
					cliproxyexecutor.Options{
						SourceFormat:   sdktranslator.FormatOpenAIResponse,
						ResponseFormat: sdktranslator.FormatClaude,
					},
					false,
				)
				if err != nil {
					t.Fatalf("prepareClaudeRequest error: %v", err)
				}
				t.Cleanup(func() {
					if errClose := prepared.httpReq.Body.Close(); errClose != nil {
						t.Errorf("close request body: %v", errClose)
					}
				})
				if got := prepared.httpReq.Header.Get("Authorization"); got != testCase.wantAuthorization {
					t.Fatalf("Authorization = %q, want %q", got, testCase.wantAuthorization)
				}
				if got := prepared.httpReq.Header.Get("x-api-key"); got != testCase.wantAPIKey {
					t.Fatalf("x-api-key = %q, want %q", got, testCase.wantAPIKey)
				}
				if got := prepared.httpReq.Header.Get("Anthropic-Version"); got != testCase.wantAnthropicVersion {
					t.Fatalf("Anthropic-Version = %q, want %q", got, testCase.wantAnthropicVersion)
				}
				body, errRead := io.ReadAll(prepared.httpReq.Body)
				if errRead != nil {
					t.Fatalf("read Claude request body: %v", errRead)
				}

				thinking := gjson.GetBytes(body, "messages.0.content.0")
				if got := thinking.Get("type").String(); got != "thinking" {
					t.Fatalf("thinking type = %q, want thinking; body=%s", got, body)
				}
				if got := thinking.Get("thinking").String(); got != "prior reasoning" {
					t.Fatalf("thinking = %q, want prior reasoning; body=%s", got, body)
				}
				if got := thinking.Get("signature").String(); got != "opaque-signature" {
					t.Fatalf("signature = %q, want opaque-signature; body=%s", got, body)
				}
			},
		)
	}
}

func TestDeepSeekAnthropicCompatibilityRequest(t *testing.T) {
	modelInfo := registry.LookupStaticModelInfo("deepseek-flash")
	if modelInfo == nil {
		t.Fatal("deepseek-flash static metadata is missing")
	}
	tests := []struct {
		name         string
		thinking     string
		wantThinking string
		wantBudget   bool
	}{
		{
			name:         "thinking enabled",
			thinking:     `{"type":"enabled","budget_tokens":4096}`,
			wantThinking: "enabled",
			wantBudget:   true,
		},
		{
			name:         "thinking disabled",
			thinking:     `{"type":"disabled"}`,
			wantThinking: "disabled",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var upstreamBody []byte
			var upstreamHeaders http.Header
			var upstreamURL string
			ctx := util.WithRoundTripper(
				t.Context(),
				apiKeyCompatRoundTripper(func(req *http.Request) (*http.Response, error) {
					var errRead error
					upstreamBody, errRead = io.ReadAll(req.Body)
					if errRead != nil {
						t.Fatalf("read DeepSeek upstream body: %v", errRead)
					}
					upstreamHeaders = req.Header.Clone()
					upstreamURL = req.URL.String()
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(
							`{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
						)),
						Request: req,
					}, nil
				}),
			)
			payload := []byte(`{
				"model":"deepseek-flash",
				"max_tokens":128,
				"thinking":` + testCase.thinking + `,
				"messages":[
					{"role":"user","content":[
						{"type":"text","text":"Describe this image."},
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
					]},
					{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"lookup","input":{"id":"42"}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"found"}]}
				],
				"tools":[{"name":"lookup","description":"Look up an item","input_schema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}]
			}`)
			_, errExecute := NewClaudeExecutor(&config.Config{}).Execute(
				ctx,
				&cliproxyauth.Auth{
					Provider: "claude",
					Attributes: map[string]string{
						"base_url": "https://api.deepseek.com/anthropic",
						"api_key":  "test-key",
					},
				},
				cliproxyexecutor.Request{
					Model:   "deepseek-flash",
					Payload: payload,
					Metadata: map[string]any{
						"cliproxy.resolved_api_key_model_info": modelInfo,
					},
				},
				cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatClaude,
					ResponseFormat: sdktranslator.FormatClaude,
				},
			)
			if errExecute != nil {
				t.Fatalf("Execute DeepSeek request: %v", errExecute)
			}
			if upstreamURL != "https://api.deepseek.com/anthropic/v1/messages?beta=true" {
				t.Fatalf("DeepSeek upstream URL = %q", upstreamURL)
			}
			if got := upstreamHeaders.Get("x-api-key"); got != "test-key" {
				t.Fatalf("DeepSeek x-api-key = %q, want test-key", got)
			}
			if got := upstreamHeaders.Get("Authorization"); got != "" {
				t.Fatalf("DeepSeek Authorization = %q, want empty", got)
			}
			if got := upstreamHeaders.Get("Anthropic-Version"); got != "" {
				t.Fatalf("DeepSeek Anthropic-Version = %q, want empty", got)
			}
			if got := gjson.GetBytes(upstreamBody, "thinking.type").String(); got != testCase.wantThinking {
				t.Fatalf("DeepSeek thinking.type = %q, want %q; body=%s", got, testCase.wantThinking, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "thinking.budget_tokens").Exists(); got != testCase.wantBudget {
				t.Fatalf("DeepSeek thinking budget presence = %t, want %t; body=%s", got, testCase.wantBudget, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "messages.0.content.1.source.data").String(); got != "iVBORw0KGgo=" {
				t.Fatalf("DeepSeek image payload = %q; body=%s", got, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "tools.0.name").String(); got != "lookup" {
				t.Fatalf("DeepSeek tool definition = %q; body=%s", got, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "messages.1.content.0.type").String(); got != "tool_use" {
				t.Fatalf("DeepSeek tool_use block = %q; body=%s", got, upstreamBody)
			}
			if got := gjson.GetBytes(upstreamBody, "messages.2.content.0.type").String(); got != "tool_result" {
				t.Fatalf("DeepSeek tool_result block = %q; body=%s", got, upstreamBody)
			}
		})
	}
}

func TestGeminiExecutorUsesResolvedModelIsCompat(t *testing.T) {
	executor := NewGeminiExecutor(&config.Config{})
	prepared, err := executor.prepareGeminiRequest(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{
			Model: "deepseek-v4",
			Payload: []byte(
				`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior reasoning","signature":""}]}]}`,
			),
			Metadata: map[string]any{
				"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
			},
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatClaude,
			ResponseFormat: sdktranslator.FormatGemini,
		},
		false,
	)
	if err != nil {
		t.Fatalf("prepareGeminiRequest error: %v", err)
	}

	part := gjson.GetBytes(prepared.body, `contents.#(role=="model").parts.0`)
	if !part.Get("thought").Bool() || part.Get("text").String() != "prior reasoning" {
		t.Fatalf("Gemini thought part missing: %s", prepared.body)
	}
	if !part.Get("thoughtSignature").Exists() || part.Get("thoughtSignature").String() != "" {
		t.Fatalf("empty thoughtSignature missing: %s", prepared.body)
	}
}

func TestGeminiInteractionsExecutorUsesResolvedModelIsCompat(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(),
		apiKeyCompatRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatalf("read upstream body: %v", errRead)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"interaction":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
						),
					),
					Request: req,
				}, nil
			},
		),
	)

	executor := NewGeminiInteractionsExecutor(&config.Config{})
	_, err := executor.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "gemini-interactions",
			Attributes: map[string]string{
				"base_url": "https://interactions-compatible.example.com",
				"api_key":  "test-key",
			},
		},
		cliproxyexecutor.Request{
			Model: "deepseek-v4",
			Payload: []byte(
				`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":""}]}]}`,
			),
			Metadata: map[string]any{
				"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
			},
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatClaude,
			ResponseFormat: sdktranslator.FormatInteractions,
		},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := gjson.GetBytes(upstreamBody, "input.0.type").String(); got != "thought" {
		t.Fatalf("input.0.type = %q, want thought; body=%s", got, upstreamBody)
	}
}

func TestXAIExecutorUsesResolvedModelIsCompat(t *testing.T) {
	executor := newXAIExecutor(&config.Config{})
	payload := []byte(
		`{"model":"deepseek-v4","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"prior reasoning"}],"encrypted_content":""}]}`,
	)
	prepare := func(isCompat bool) *xaiPreparedRequest {
		t.Helper()
		request := cliproxyexecutor.Request{Model: "deepseek-v4", Payload: payload}
		if isCompat {
			request.Metadata = map[string]any{
				"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
			}
		}
		prepared, err := executor.prepareResponsesRequestWithContext(
			context.Background(),
			nil,
			request,
			cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			},
			true,
			sdktranslator.FormatCodex,
		)
		if err != nil {
			t.Fatalf("prepareResponsesRequestWithContext error: %v", err)
		}
		return prepared
	}

	withoutCompat := prepare(false)
	if gjson.GetBytes(withoutCompat.body, "input.0.encrypted_content").Exists() {
		t.Fatalf("non-compat request kept empty encrypted_content: %s", withoutCompat.body)
	}
	withCompat := prepare(true)
	encryptedContent := gjson.GetBytes(withCompat.body, "input.0.encrypted_content")
	if !encryptedContent.Exists() || encryptedContent.String() != "" {
		t.Fatalf("compat request dropped empty encrypted_content: %s", withCompat.body)
	}
}
