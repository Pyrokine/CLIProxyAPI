package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestImageGenerationModesReachProviderTransports(t *testing.T) {
	for _, provider := range []string{
		"gemini", "vertex", "antigravity", "gemini-cli", "aistudio", "claude", "kimi", "openai-compat", "qwen", "iflow",
		"meta", "xai",
	} {
		model, format, root := "gemini-2.5-flash", sdktranslator.FormatGemini, ""
		payload := `{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}],"tools":[` +
			`{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"typ` +
			`e":"object"}}]},{"type":"image_generation","quality":"low"}],"tool_cho` +
			`ice":{"type":"image_generation"}}`
		switch provider {
		case "antigravity", "gemini-cli":
			format = sdktranslator.FromString(provider)
			payload, root = `{"request":`+payload+`}`, "request."
		case "claude":
			model, format = "claude-synthetic-4772", sdktranslator.FormatClaude
			payload = `{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"` +
				`user","content":"synthetic"}],"tools":[{"name":"lookup","input_schema"` +
				`:{"type":"object"}},{"type":"image_generation","quality":"low"}],"tool` +
				`_choice":{"type":"image_generation"}}`
		case "kimi", "openai-compat", "qwen", "iflow":
			model, format = "synthetic-image-policy", sdktranslator.FormatOpenAI
			payload = `{"model":"synthetic-image-policy","messages":[{"role":"user","content"` +
				`:"synthetic"}],"tools":[{"type":"function","function":{"name":"lookup"` +
				`,"parameters":{"type":"object"}}},{"type":"image_generation","quality"` +
				`:"low"}],"tool_choice":{"type":"image_generation"}}`
		case "meta", "xai":
			model, format = "muse-spark-1.3", sdktranslator.FormatCodex
			if provider == "xai" {
				model = "grok-4.6"
			}
			payload = `{"input":"synthetic","tools":[{"type":"function","name":"lookup","para` +
				`meters":{"type":"object"}},{"type":"image_generation","quality":"low"}` +
				`],"tool_choice":{"type":"image_generation"}}`
		}
		registration := t.Name() + "/models/" + provider
		modelInfo := registry.LookupModelInfo(model, provider)
		if modelInfo == nil {
			modelInfo = &registry.ModelInfo{ID: model}
		}
		registry.GetGlobalRegistry().RegisterClient(registration, provider, []*registry.ModelInfo{modelInfo})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(registration) })
		for _, mode := range []config.DisableImageGenerationMode{
			config.DisableImageGenerationOff, config.DisableImageGenerationAll,
			config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough,
		} {
			for _, stream := range []bool{false, true} {
				for _, explicitAlt := range []bool{false, true} {
					for _, imageEndpoint := range []bool{false, true} {
						for _, override := range []bool{false, true} {
							name := fmt.Sprintf(
								"%s/%s/stream=%t/alt=%t/images=%t/override=%t",
								provider,
								mode,
								stream,
								explicitAlt,
								imageEndpoint,
								override,
							)
							t.Run(
								name, func(t *testing.T) {
									cfg := &config.Config{DisableImageGeneration: mode}
									if override {
										cfg.Payload.Override = []config.PayloadRule{
											{
												Models: []config.PayloadModelRule{
													{Name: "*"},
												}, Params: map[string]any{
													"tools": []any{
														map[string]any{
															"type": "image_generation", "quality": "high",
														},
													}, "tool_choice": map[string]any{"type": "image_generation"},
												},
											},
										}
									}
									path := "/v1/responses"
									if imageEndpoint {
										path = "/v1/images/generations"
									}
									opts := cliproxyexecutor.Options{
										SourceFormat: format, ResponseFormat: format, Stream: stream,
										Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: path},
									}
									if explicitAlt {
										opts.Alt = "json"
										if stream {
											opts.Alt = "sse"
										}
									}
									body := imageModeProviderCall(
										t,
										provider,
										model,
										cfg,
										[]byte(payload),
										opts,
									)
									wantImage := override || mode == config.DisableImageGenerationOff ||
										mode == config.DisableImageGenerationPassthrough ||
										mode == config.DisableImageGenerationChat && imageEndpoint
									image := gjson.GetBytes(body, root+`tools.#(type=="image_generation")`)
									choice := gjson.GetBytes(body, root+"tool_choice")
									if image.Exists() != wantImage || choice.Exists() != wantImage {
										t.Fatalf(
											"final image=%t choice=%s want=%t body=%s",
											image.Exists(),
											choice.Raw,
											wantImage,
											body,
										)
									}
									forcedImageOnly := provider == "xai" && wantImage
									if forcedImageOnly && (choice.String() != "required" || len(
										gjson.GetBytes(
											body, "tools",
										).Array(),
									) != 1) {
										t.Fatalf(
											"xAI forced image choice lost native required semantics: %s",
											body,
										)
									}
									if override {
										if image.Get("quality").String() != "high" ||
											!forcedImageOnly &&
												choice.Get("type").String() != "image_generation" ||
											len(
												gjson.GetBytes(
													body, root+"tools",
												).Array(),
											) != 1 {
											t.Fatalf("explicit payload override lost final authority: %s", body)
										}
									} else if !forcedImageOnly && !strings.Contains(
										gjson.GetBytes(
											body, root+"tools",
										).Raw, "lookup",
									) {
										t.Fatalf("ordinary tool was removed: %s", body)
									}
								},
							)
						}
					}
				}
			}
		}
	}
}

func imageModeProviderCall(
	t *testing.T,
	provider, model string,
	cfg *config.Config,
	payload []byte,
	opts cliproxyexecutor.Options,
) []byte {
	t.Helper()
	if provider == "gemini" || provider == "vertex" || provider == "antigravity" {
		route := "generate"
		if opts.Stream {
			route = "stream"
		}
		return googlePayloadCall(t, provider, route, model, cfg, payload, opts)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credential := googlePayloadAuth(provider)
	credential.ID = t.Name()
	credential.Metadata["expiry"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	if provider == "qwen" {
		t.Cleanup(
			func() {
				qwenRateLimiter.Lock()
				delete(qwenRateLimiter.requests, credential.ID)
				qwenRateLimiter.Unlock()
			},
		)
	}
	var captured []byte
	var relayPayload <-chan []byte
	calls := 0
	var executor cliproxyauth.ProviderExecutor
	if provider == "aistudio" {
		connected := make(chan struct{})
		manager := wsrelay.NewManager(
			wsrelay.Options{
				ProviderFactory: func(*http.Request) (
					string,
					error,
				) {
					return credential.ID, nil
				},
				OnConnected: func(string) { close(connected) },
			},
		)
		server := httptest.NewServer(manager.Handler())
		t.Cleanup(server.Close)
		t.Cleanup(func() { _ = manager.Stop(context.Background()) })
		peer, response, err := websocket.DefaultDialer.Dial(
			"ws"+strings.TrimPrefix(server.URL, "http")+manager.Path(), nil,
		)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = peer.Close() })
		select {
		case <-connected:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		wire, done := make(chan []byte, 1), make(chan error, 1)
		go func() {
			var envelope json.RawMessage
			if err := peer.ReadJSON(&envelope); err != nil {
				done <- err
				return
			}
			wire <- []byte(gjson.GetBytes(envelope, "payload.body").String())
			done <- peer.WriteJSON(
				map[string]any{
					"id": gjson.GetBytes(envelope, "id").String(), "type": wsrelay.MessageTypeHTTPResp,
					"payload": map[string]any{
						"status": http.StatusOK,
						"body": `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},` +
							`"finishReason":` +
							`"STOP"}]}`,
					},
				},
			)
		}()
		executor = NewAIStudioExecutor(cfg, provider, manager)
		t.Cleanup(
			func() {
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(time.Second):
					t.Error("synthetic AIStudio response did not complete")
				}
			},
		)
		relayPayload = wire
	} else {
		ctx = util.WithRoundTripper(
			ctx, googlePayloadTransport(
				func(request *http.Request) (*http.Response, error) {
					calls++
					var err error
					captured, err = io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					body := `{"id":"synthetic","choices":[{"index":0,"message":{"role":"assistant",` +
						`"content":"ok"},"delta":{"content":"ok"},"finish_reason":"stop"}],"usa` +
						`ge":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
					if provider == "claude" {
						body = claudeReplayTextResponse("synthetic-image-mode")
						if opts.Stream {
							body = claudeReplayTextStream("synthetic-image-mode")
						}
					} else if provider == "gemini-cli" {
						body = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text"` +
							`:"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1` +
							`,"candidatesTokenCount":1,"totalTokenCount":2}}}`
						if opts.Stream {
							body = "data: " + body + "\n\n"
						}
					} else if provider == "meta" || provider == "xai" {
						body = "data: " + (`{"type":"response.completed","response":{"id":"resp_synthetic_image_mo` +
							`de","status":"completed","output":[],"usage":{"input_tokens":1,"output` +
							`_tokens":1,"total_tokens":2}}}`) + "\n\n"
					} else if opts.Stream {
						body = "data: " + body + "\n\ndata: [DONE]\n\n"
					}
					response := googlePayloadResponse(request, body)
					response.Header.Set("Content-Type", "application/json")
					if opts.Stream {
						response.Header.Set("Content-Type", "text/event-stream")
					}
					return response, nil
				},
			),
		)
		switch provider {
		case "claude":
			executor = NewClaudeExecutor(cfg)
		case "gemini-cli":
			executor = NewGeminiCLIExecutor(cfg)
		case "kimi":
			executor = NewKimiExecutor(cfg)
		case "openai-compat":
			executor = NewOpenAICompatExecutor(provider, cfg)
		case "qwen":
			executor = NewQwenExecutor(cfg)
		case "iflow":
			executor = NewIFlowExecutor(cfg)
		case "meta":
			executor = NewMetaExecutor(cfg)
		case "xai":
			executor = newXAIExecutor(cfg)
		default:
			t.Fatalf("unexpected provider %q", provider)
		}
	}
	original := bytes.Clone(payload)
	request := cliproxyexecutor.Request{Model: model, Payload: payload}
	var err error
	if opts.Stream {
		var result *cliproxyexecutor.StreamResult
		result, err = executor.ExecuteStream(ctx, credential, request, opts)
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		}
	} else {
		_, err = executor.Execute(ctx, credential, request, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	if relayPayload != nil {
		select {
		case captured = <-relayPayload:
		case <-ctx.Done():
			t.Fatal("synthetic AIStudio request did not reach the relay")
		}
	}
	if !bytes.Equal(payload, original) || provider != "aistudio" && calls != 1 {
		t.Fatalf("caller payload retained=%t dispatches=%d", bytes.Equal(payload, original), calls)
	}
	return captured
}
