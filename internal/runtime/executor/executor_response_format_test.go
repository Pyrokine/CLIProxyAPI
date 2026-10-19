package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestExecutorResponseFormatIndependentFromRequest(t *testing.T) {
	const prompt = "offline-format-prompt"
	const answer = "offline-format-answer"
	const chatResponse = `{"id":"offline-response","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"` + answer + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
	const chatStream = `data: {"id":"offline-response","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"` + answer + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}` + "\n\ndata: [DONE]\n\n"
	const geminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"` + answer + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`
	const payload = `{"model":"offline-model","messages":[{"role":"user","content":"` + prompt + `"}]}`
	cfg := &config.Config{}
	for _, test := range []struct {
		name       string
		create     func() cliproxyauth.ProviderExecutor
		model      string
		response   string
		localCount bool
	}{
		{name: "openai-compatible", create: func() cliproxyauth.ProviderExecutor { return NewOpenAICompatExecutor("openai-compatibility", cfg) }, model: "gpt-4o", response: chatResponse, localCount: true},
		{name: "gemini", create: func() cliproxyauth.ProviderExecutor { return NewGeminiExecutor(cfg) }, model: "gemini-3.7-flash", response: geminiResponse},
		{name: "vertex", create: func() cliproxyauth.ProviderExecutor { return NewGeminiVertexExecutor(cfg) }, model: "gemini-3.7-flash", response: geminiResponse},
		{name: "qwen", create: func() cliproxyauth.ProviderExecutor { return NewQwenExecutor(cfg) }, model: "gpt-4o", response: chatResponse, localCount: true},
		{name: "iflow", create: func() cliproxyauth.ProviderExecutor { return NewIFlowExecutor(cfg) }, model: "gpt-4o", response: chatResponse, localCount: true},
		{name: "kimi", create: func() cliproxyauth.ProviderExecutor { return NewKimiExecutor(cfg) }, model: "gpt-4o", response: chatResponse},
		{name: "gemini-cli", create: func() cliproxyauth.ProviderExecutor { return NewGeminiCLIExecutor(cfg) }, model: "gemini-3.7-flash", response: `{"response":` + geminiResponse + `}`},
		{name: "antigravity", create: func() cliproxyauth.ProviderExecutor { return NewAntigravityExecutor(cfg) }, model: "gemini-3.6-flash-high", response: `{"response":` + geminiResponse + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, method := range []string{"execute", "stream", "count"} {
				for _, target := range []struct {
					name   string
					format sdktranslator.Format
				}{
					{name: "explicit-claude", format: sdktranslator.FormatClaude},
					{name: "default"},
				} {
					t.Run(method+"/"+target.name, func(t *testing.T) {
						executor := test.create()
						auth := &cliproxyauth.Auth{
							ID: "offline-response-format-" + test.name, Provider: executor.Identifier(),
							Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://offline-format.invalid"},
							Metadata: map[string]any{
								"api_key": "synthetic-key", "access_token": "synthetic-token", "project_id": "synthetic-project",
								"resource_url": "https://offline-format.invalid", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
								"token": map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)},
							},
						}
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						requests := 0
						ctx = util.WithRoundTripper(ctx, apiKeyCompatRoundTripper(func(request *http.Request) (*http.Response, error) {
							requests++
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							translatedPrompt := false
							for _, path := range []string{`messages.#(role=="user").content`, `messages.#(role=="user").content.0.text`, `contents.#(role=="user").parts.0.text`, `request.contents.#(role=="user").parts.0.text`} {
								if gjson.GetBytes(body, path).String() == prompt {
									translatedPrompt = true
								}
							}
							if !translatedPrompt {
								return nil, fmt.Errorf("request source format was not translated: %s", body)
							}
							response := test.response
							contentType := "application/json"
							switch method {
							case "stream":
								contentType = "text/event-stream"
								response = "data: " + response + "\n\n"
								if test.response == chatResponse {
									response = chatStream
								}
							case "count":
								response = `{"totalTokens":7}`
								if test.name == "kimi" {
									response = `{"input_tokens":7}`
								}
							}
							return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
						}))
						req := cliproxyexecutor.Request{Model: test.model, Payload: []byte(payload)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: target.format, OriginalRequest: []byte(payload)}
						if method == "stream" {
							opts.Stream = true
							req.Payload = []byte(strings.Replace(payload, `"model":`, `"stream":true,"model":`, 1))
							opts.OriginalRequest = req.Payload
							stream, err := executor.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							var output strings.Builder
							var streamErr error
							for chunk := range stream.Chunks {
								output.Write(chunk.Payload)
								if chunk.Err != nil {
									streamErr = chunk.Err
								}
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
							if streamErr != nil {
								t.Fatal(streamErr)
							}
							text := strings.ReplaceAll(output.String(), ": ", ":")
							if !strings.Contains(text, answer) || (target.format == sdktranslator.FormatClaude &&
								(strings.Count(text, `"type":"message_stop"`) != 1 || !strings.Contains(text, `"type":"message_start"`))) ||
								(target.format == "" && (!strings.Contains(text, `"chat.completion.chunk"`) || strings.Contains(text, `"type":"message_stop"`))) {
								t.Fatalf("stream did not use the selected response format: %s", text)
							}
						} else {
							var response cliproxyexecutor.Response
							var err error
							if method == "count" {
								opts.CountTokens = true
								response, err = executor.CountTokens(ctx, auth, req, opts)
							} else {
								response, err = executor.Execute(ctx, auth, req, opts)
							}
							if err != nil {
								t.Fatal(err)
							}
							path, want := "choices.0.message.content", answer
							if target.format == sdktranslator.FormatClaude {
								path = "content.0.text"
								if gjson.GetBytes(response.Payload, "type").String() != "message" && method != "count" {
									t.Fatalf("response was not a Claude message: %s", response.Payload)
								}
							}
							if method == "count" {
								path = "input_tokens"
								if target.format == "" && test.name != "kimi" {
									path = "totalTokens"
									if test.localCount {
										path = "usage.prompt_tokens"
									}
								}
								if gjson.GetBytes(response.Payload, path).Int() <= 0 || (!test.localCount && gjson.GetBytes(response.Payload, path).Int() != 7) {
									t.Fatalf("token count did not use the selected response format: %s", response.Payload)
								}
							} else if gjson.GetBytes(response.Payload, path).String() != want {
								t.Fatalf("response did not use the selected response format: %s", response.Payload)
							}
						}
						wantRequests := 1
						if method == "count" && test.localCount {
							wantRequests = 0
						}
						if requests != wantRequests {
							t.Fatalf("upstream requests = %d, want %d", requests, wantRequests)
						}
					})
				}
			}
		})
	}
}

func TestCodexCountTokensUsesResponseFormat(t *testing.T) {
	for _, target := range []sdktranslator.Format{sdktranslator.FormatClaude, ""} {
		t.Run(target.String(), func(t *testing.T) {
			exec := newCodexExecutor(&config.Config{})
			ctx := util.WithRoundTripper(context.Background(), apiKeyCompatRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("local Codex token counting attempted an upstream request")
			}))
			resp, err := exec.CountTokens(ctx, &cliproxyauth.Auth{}, cliproxyexecutor.Request{
				Model: "gpt-4o", Payload: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"offline-format-prompt"}]}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: target, CountTokens: true})
			if err != nil {
				t.Fatal(err)
			}
			path := "response.usage.input_tokens"
			if target == sdktranslator.FormatClaude {
				path = "input_tokens"
			}
			if gjson.GetBytes(resp.Payload, path).Int() <= 0 {
				t.Fatalf("token count did not use the selected response format: %s", resp.Payload)
			}
		})
	}
}

func TestExecutorEmptyNonstreamTranslationFailsWithObservedUsage(t *testing.T) {
	const chat = `{"id":"empty-chat","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	const completed = `{"type":"response.completed","response":{"id":"empty-response","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`
	const compact = `{"id":"empty-compaction","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-state"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	const gemini = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}}`
	var usageWire []byte
	usageWire = protowire.AppendTag(usageWire, 2, protowire.VarintType)
	usageWire = protowire.AppendVarint(usageWire, 7)
	usageWire = protowire.AppendTag(usageWire, 3, protowire.VarintType)
	usageWire = protowire.AppendVarint(usageWire, 3)
	var devinWire []byte
	devinWire = protowire.AppendTag(devinWire, 1, protowire.BytesType)
	devinWire = protowire.AppendString(devinWire, "empty-devin-output")
	devinWire = protowire.AppendTag(devinWire, 3, protowire.BytesType)
	devinWire = protowire.AppendString(devinWire, "ok")
	devinWire = protowire.AppendTag(devinWire, 5, protowire.VarintType)
	devinWire = protowire.AppendVarint(devinWire, 1)
	devinWire = protowire.AppendTag(devinWire, 7, protowire.BytesType)
	devinWire = protowire.AppendBytes(devinWire, usageWire)
	devinBody := append(helps.WrapConnectEnvelope(devinWire), helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`))...)
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
	for _, tc := range []struct {
		name        string
		create      func() cliproxyauth.ProviderExecutor
		model       string
		from, to    sdktranslator.Format
		body        []byte
		ws, compact bool
	}{
		{"codex", func() cliproxyauth.ProviderExecutor { return newCodexExecutor(cfg) }, "gpt-5.4", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, []byte("data: " + completed + "\n\n"), false, false},
		{"codex-compact", func() cliproxyauth.ProviderExecutor { return newCodexExecutor(cfg) }, "gpt-5.4", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, []byte(compact), false, true},
		{"xai-compact", func() cliproxyauth.ProviderExecutor { return newXAIExecutor(cfg) }, "grok-4.3", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, []byte(compact), false, true},
		{"codex-websocket", func() cliproxyauth.ProviderExecutor { return newCodexWebsocketsExecutor(cfg) }, "gpt-5.4", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, []byte(completed), true, false},
		{"openai-compatible", func() cliproxyauth.ProviderExecutor { return NewOpenAICompatExecutor("openai-compatibility", cfg) }, "gpt-4o", sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, []byte(chat), false, false},
		{"qwen", func() cliproxyauth.ProviderExecutor { return NewQwenExecutor(cfg) }, "gpt-4o", sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, []byte(chat), false, false},
		{"iflow", func() cliproxyauth.ProviderExecutor { return NewIFlowExecutor(cfg) }, "gpt-4o", sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, []byte(chat), false, false},
		{"gemini-cli", func() cliproxyauth.ProviderExecutor { return NewGeminiCLIExecutor(cfg) }, "gemini-3.7-flash", sdktranslator.FormatOpenAI, sdktranslator.FromString("gemini-cli"), []byte(gemini), false, false},
		{"devin", func() cliproxyauth.ProviderExecutor { return NewDevinExecutor(cfg) }, "devin/swe-2", sdktranslator.FormatInteractions, sdktranslator.FormatInteractions, devinBody, false, false},
	} {
		for _, empty := range []bool{false, true} {
			name := "success"
			if empty {
				name = "empty"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				format := sdktranslator.Format("formal-empty-" + tc.name + "-" + name)
				sdktranslator.Register(format, tc.to, nil, sdktranslator.ResponseTransform{
					NonStream: func(_ context.Context, _ string, _, _, _ []byte, _ *any) []byte {
						if empty {
							return nil
						}
						return []byte(`{"result":"ok"}`)
					},
				})
				t.Cleanup(func() { sdktranslator.Register(format, tc.to, nil, sdktranslator.ResponseTransform{}) })
				ctx, finish := xaiOutcomeContext(t)
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				var calls atomic.Int32
				baseURL := "https://empty-response.invalid"
				if tc.ws {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer peer.Close()
						if _, _, err = peer.ReadMessage(); err != nil {
							t.Error(err)
							return
						}
						calls.Add(1)
						if err = peer.WriteMessage(websocket.TextMessage, tc.body); err != nil {
							t.Error(err)
						}
					}))
					defer server.Close()
					baseURL = server.URL
				} else {
					ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
						calls.Add(1)
						if !strings.HasSuffix(req.URL.Host, ".invalid") && !(tc.name == "gemini-cli" && req.URL.Host == "cloudcode-pa.googleapis.com" && req.URL.Path == "/v1internal:generateContent") {
							return nil, fmt.Errorf("unexpected synthetic target %s", req.URL)
						}
						if req.Body != nil {
							if _, err := io.ReadAll(req.Body); err != nil {
								return nil, err
							}
						}
						contentType := "application/json"
						if tc.name == "codex" {
							contentType = "text/event-stream"
						}
						if tc.name == "devin" {
							contentType = "application/connect+proto"
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(tc.body)), Request: req}, nil
					}))
				}
				exec := tc.create()
				auth := &cliproxyauth.Auth{ID: "empty-response-synthetic-" + tc.name, Provider: exec.Identifier(), Attributes: map[string]string{"api_key": "synthetic-key", "base_url": baseURL}, Metadata: map[string]any{
					"api_key": "synthetic-key", "access_token": "synthetic-token", "project_id": "synthetic-project", "resource_url": baseURL, "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
					"token": map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)},
				}}
				payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
				if tc.from == sdktranslator.FormatOpenAIResponse {
					payload = []byte(`{"input":"hello"}`)
				}
				if tc.from == sdktranslator.FormatInteractions {
					payload = []byte(`{"input":[{"type":"user_input","content":[{"type":"text","text":"hello"}]}]}`)
				}
				opts := cliproxyexecutor.Options{SourceFormat: tc.from, ResponseFormat: format, OriginalRequest: bytes.Clone(payload)}
				if tc.compact {
					opts.Alt = "responses/compact"
				}
				response, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: tc.model, Payload: payload}, opts)
				if empty {
					if statusErrCode(err) != http.StatusBadGateway || len(response.Payload) != 0 {
						t.Errorf("empty translation accepted: error=%v output=%s", err, response.Payload)
					}
				} else if err != nil || string(response.Payload) != `{"result":"ok"}` {
					t.Errorf("success changed: error=%v output=%s", err, response.Payload)
				}
				if count := calls.Load(); count != 1 {
					t.Errorf("calls=%d, want 1", count)
				}
				records := finish()
				if len(records) != 1 {
					t.Fatalf("usage records=%d, want 1", len(records))
				}
				record := records[0]
				if record.Failed != empty || record.Completed == empty || record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10 {
					t.Errorf("outcome or observed usage lost: %+v", record)
				}
			})
		}
	}
}
