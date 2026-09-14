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
		wantAnthropicVersion string
	}{
		{
			name:                 "DeepSeek Anthropic endpoint",
			baseURL:              "https://api.deepseek.com/anthropic",
			wantAnthropicVersion: "",
		},
		{
			name:                 "other Claude-compatible endpoint",
			baseURL:              "https://claude-compatible.example.com",
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
				if got := prepared.httpReq.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Fatalf("Authorization = %q, want Bearer test-key", got)
				}
				if got := prepared.httpReq.Header.Get("x-api-key"); got != "" {
					t.Fatalf("x-api-key = %q, want empty for custom base", got)
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

	part := gjson.GetBytes(prepared.body, "contents.0.parts.0")
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
