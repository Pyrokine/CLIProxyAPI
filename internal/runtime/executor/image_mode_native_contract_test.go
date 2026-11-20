package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestImageGenerationModesReachXAIWebsocket(t *testing.T) {
	for _, mode := range []config.DisableImageGenerationMode{
		config.DisableImageGenerationOff, config.DisableImageGenerationAll,
		config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough,
	} {
		for _, explicitAlt := range []bool{false, true} {
			for _, imageEndpoint := range []bool{false, true} {
				for _, override := range []bool{false, true} {
					t.Run(
						fmt.Sprintf(
							"%s/alt=%t/images=%t/override=%t",
							mode,
							explicitAlt,
							imageEndpoint,
							override,
						),
						func(t *testing.T) {
							cfg := &config.Config{DisableImageGeneration: mode}
							if override {
								cfg.Payload.Override = []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{
											"tools": []any{
												map[string]any{
													"type": "image_generation", "quality": "high",
												},
											},
											"tool_choice": map[string]any{"type": "image_generation"},
										},
									},
								}
							}
							received := make(chan []byte, 1)
							server := httptest.NewServer(
								http.HandlerFunc(
									func(writer http.ResponseWriter, request *http.Request) {
										peer, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
										if err != nil {
											t.Error(err)
											return
										}
										defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
										if err = peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
											t.Error(err)
											return
										}
										_, payload, err := peer.ReadMessage()
										if err != nil {
											t.Error(err)
											return
										}
										received <- payload
										if err = peer.WriteMessage(
											websocket.TextMessage,
											[]byte((`{"type":"response.completed","response":{"id":`+
												`"resp_synthetic_image_mo`)+
												(`de_ws","status":"completed","output":[],"usage":`+
													`{"input_tokens":1,`+
													`"out`)+
												`put_tokens":1,"total_tokens":2}}}`),
										); err != nil {
											t.Error(err)
										}
									},
								),
							)
							defer server.Close()
							executor := newXAIWebsocketsExecutor(cfg)
							defer executor.CloseExecutionSession(t.Name())
							credential := googlePayloadAuth("xai")
							credential.ID, credential.Attributes["base_url"] = t.Name(), server.URL
							path := "/v1/responses"
							if imageEndpoint {
								path = "/v1/images/generations"
							}
							opts := cliproxyexecutor.Options{
								SourceFormat: sdktranslator.FormatCodex, ResponseFormat: sdktranslator.FormatCodex,
								Metadata: map[string]any{
									cliproxyexecutor.RequestPathMetadataKey:      path,
									cliproxyexecutor.ExecutionSessionMetadataKey: t.Name(),
								},
							}
							if explicitAlt {
								opts.Alt = "sse"
							}
							payload := []byte(
								`{"input":"synthetic","tools":[{"type":"function","name":"lookup","para` +
									`meters":{"type":"object"}},{"type":"image_generation","quality":"low"}` +
									`],"tool_choice":{"type":"image_generation"}}`,
							)
							original := bytes.Clone(payload)
							ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
							defer cancel()
							stream, err := executor.ExecuteStream(
								ctx,
								credential,
								cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload},
								opts,
							)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
							var wire []byte
							select {
							case wire = <-received:
							case <-ctx.Done():
								t.Fatal(ctx.Err())
							}
							wantImage := override || mode == config.DisableImageGenerationOff ||
								mode == config.DisableImageGenerationPassthrough ||
								mode == config.DisableImageGenerationChat && imageEndpoint
							image, choice := gjson.GetBytes(
								wire, `tools.#(type=="image_generation")`,
							), gjson.GetBytes(wire, "tool_choice")
							if image.Exists() != wantImage || choice.Exists() != wantImage || !bytes.Equal(
								payload, original,
							) || gjson.GetBytes(wire, "type").String() != "response.create" {
								t.Fatalf("websocket image mode or caller ownership changed: %s", wire)
							}
							if wantImage && (choice.String() != "required" || len(
								gjson.GetBytes(
									wire, "tools",
								).Array(),
							) != 1) {
								t.Fatalf("forced image choice lost native required semantics: %s", wire)
							}
							if override && image.Get("quality").String() != "high" {
								t.Fatalf("explicit image override was lost: %s", wire)
							}
							if !wantImage && gjson.GetBytes(
								wire, `tools.#(name=="lookup")`,
							).Get("type").String() != "function" {
								t.Fatalf("ordinary lookup tool was removed: %s", wire)
							}
						},
					)
				}
			}
		}
	}
}

func TestImageGenerationModesReachCompactTransports(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, mode := range []config.DisableImageGenerationMode{
			config.DisableImageGenerationOff, config.DisableImageGenerationAll,
			config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough,
		} {
			for _, clientTool := range []bool{false, true} {
				for _, override := range []bool{false, true} {
					t.Run(
						fmt.Sprintf("%s/%s/client=%t/override=%t", provider, mode, clientTool, override),
						func(t *testing.T) {
							cfg := &config.Config{DisableImageGeneration: mode}
							if override {
								cfg.Payload.Override = []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{
											"tools": []any{
												map[string]any{
													"type": "image_generation", "quality": "high",
												},
											},
											"tool_choice": map[string]any{"type": "image_generation"},
										},
									},
								}
							}
							payload := []byte(
								(`{"input":[{"role":"user","content":"synthetic"}],"tools":[{"type":"function"` +
									`,"name":"lookup","parameters":{"type":"object"}}]}`),
							)
							if clientTool {
								payload = []byte(
									(`{"input":[{"role":"user","content":"synthetic"}],"tools":[{"type":"` +
										`function","name":"lookup","parameters":{"type":"object"}},{"type":"` +
										`image_generation","quality":"low"}],"tool_choice":{"type":"` +
										`image_generation"}}`),
								)
							}
							original := bytes.Clone(payload)
							var captured []byte
							calls := 0
							ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
							defer cancel()
							ctx = util.WithRoundTripper(
								ctx, googlePayloadTransport(
									func(request *http.Request) (*http.Response, error) {
										calls++
										if request.URL.Path != "/responses/compact" {
											return nil, fmt.Errorf("compact request used %s", request.URL.Path)
										}
										var err error
										captured, err = io.ReadAll(request.Body)
										if err != nil {
											return nil, err
										}
										response := googlePayloadResponse(
											request,
											`{"id":"resp_image_mode_compact","output":[{"type":"compaction","`+
												`encrypted_content":"opaque-state"}],"usage":{"input_tokens":`+
												`1,"output_tokens":1,"total_tokens":2}}`,
										)
										response.Header.Set("Content-Type", "application/json")
										return response, nil
									},
								),
							)
							model := "gpt-5.4"
							execute := NewCodexAutoExecutor(cfg).Execute
							if provider == "xai" {
								model, execute = "grok-4.6", newXAIExecutor(cfg).Execute
							}
							opts := cliproxyexecutor.Options{
								Alt: "responses/compact", SourceFormat: sdktranslator.FormatOpenAIResponse,
								ResponseFormat: sdktranslator.FormatOpenAIResponse,
								Metadata: map[string]any{
									cliproxyexecutor.RequestPathMetadataKey: "/v1/responses/compact",
								},
							}
							if _, err := execute(
								ctx, googlePayloadAuth(provider),
								cliproxyexecutor.Request{Model: model, Payload: payload}, opts,
							); err != nil {
								t.Fatal(err)
							}
							wantImage := override ||
								provider == "codex" && clientTool &&
									(mode ==
										config.DisableImageGenerationOff ||
										mode ==
											config.DisableImageGenerationPassthrough)
							image, choice := gjson.GetBytes(
								captured, `tools.#(type=="image_generation")`,
							), gjson.GetBytes(captured, "tool_choice")
							if image.Exists() != wantImage || choice.Exists() != wantImage || calls != 1 ||
								!bytes.Equal(
									payload, original,
								) {
								t.Fatalf(
									"compact image mode or caller ownership changed: calls=%d body=%s",
									calls,
									captured,
								)
							}
							if override && (image.Get("quality").String() != "high" || len(
								gjson.GetBytes(
									captured, "tools",
								).Array(),
							) != 1) {
								t.Fatalf("compact override lost final authority: %s", captured)
							}
							if provider == "xai" && wantImage && choice.String() != "required" {
								t.Fatalf(
									"compact forced image choice lost native required semantics: %s",
									captured,
								)
							}
						},
					)
				}
			}
		}
	}
}

func TestImageGenerationModesReachDevinConnectTools(t *testing.T) {
	for _, mode := range []config.DisableImageGenerationMode{
		config.DisableImageGenerationOff, config.DisableImageGenerationAll,
		config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough,
	} {
		for _, stream := range []bool{false, true} {
			for _, explicitAlt := range []bool{false, true} {
				for _, imageEndpoint := range []bool{false, true} {
					for _, override := range []bool{false, true} {
						t.Run(
							fmt.Sprintf(
								"%s/stream=%t/alt=%t/images=%t/override=%t",
								mode,
								stream,
								explicitAlt,
								imageEndpoint,
								override,
							), func(t *testing.T) {
								cfg := &config.Config{DisableImageGeneration: mode}
								if override {
									for _, protocol := range []string{"interactions", "devin"} {
										cfg.Payload.Override = append(
											cfg.Payload.Override, config.PayloadRule{
												Models: []config.PayloadModelRule{
													{Name: "*", Protocol: protocol},
												},
												Params: map[string]any{
													"tools": []any{
														map[string]any{
															"type": "image_generation", "name": "image_generation",
															"parameters": map[string]any{
																"type": "object", "description": "configured",
															},
														},
													},
													"tool_choice": map[string]any{"type": "image_generation"},
												},
											},
										)
									}
								}
								var captured []byte
								calls := 0
								ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
								defer cancel()
								ctx = util.WithRoundTripper(
									ctx, devinTestRoundTripper(
										func(request *http.Request) (*http.Response, error) {
											calls++
											wire, err := io.ReadAll(request.Body)
											if err != nil {
												return nil, err
											}
											flag, protoBody, err := helps.ReadConnectFrame(
												bytes.NewReader(wire),
											)
											if err != nil || flag != helps.ConnectFlagData {
												return nil, fmt.Errorf(
													"invalid synthetic Devin frame: flag=%d error=%v",
													flag,
													err,
												)
											}
											_, captured, err = helps.FinalizeDevinPayload(
												protoBody,
												func(native []byte) ([]byte, error) { return native, nil },
											)
											if err != nil {
												return nil, err
											}
											response := append(
												helps.WrapConnectEnvelope(
													testDevinBytesField(
														nil, 3, []byte("ok"),
													),
												), helps.WrapConnectEnvelopeWithFlag(
													helps.ConnectFlagEndStream, []byte(`{}`),
												)...,
											)
											return &http.Response{
												StatusCode: http.StatusOK,
												Header: http.Header{
													"Content-Type": {"application/connect+proto"},
												},
												Body: io.NopCloser(
													bytes.NewReader(response),
												), Request: request,
											}, nil
										},
									),
								)
								path := "/v1/responses"
								if imageEndpoint {
									path = "/v1/images/generations"
								}
								opts := cliproxyexecutor.Options{
									SourceFormat:   sdktranslator.FormatInteractions,
									ResponseFormat: sdktranslator.FormatInteractions,
									Metadata: map[string]any{
										cliproxyexecutor.RequestPathMetadataKey: path,
									},
								}
								if explicitAlt {
									opts.Alt = "json"
									if stream {
										opts.Alt = "sse"
									}
								}
								payload := []byte(
									(`{"input":[{"type":"user_input","content":"synthetic"}],"tools":[{"type":` +
										`"function","name":"lookup","parameters":{"type":"object"}},{"type":"` +
										`image_generation","name":"image_generation","parameters":{"type":"` +
										`object","description":"caller"}}],"tool_choice":{"type":"` +
										`image_generation"}}`),
								)
								original := bytes.Clone(payload)
								executor := NewDevinExecutor(cfg)
								request := cliproxyexecutor.Request{Model: "devin/swe-2", Payload: payload}
								credential := googlePayloadAuth("devin")
								var err error
								if stream {
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
								wantImage := override || mode == config.DisableImageGenerationOff ||
									mode == config.DisableImageGenerationPassthrough ||
									mode == config.DisableImageGenerationChat && imageEndpoint
								image := gjson.GetBytes(captured, `tools.#(name=="image_generation")`)
								if image.Exists() != wantImage || calls != 1 ||
									!bytes.Equal(payload, original) {
									t.Fatalf(
										"Connect image=%t want=%t calls=%d payload unchanged=%t body=%s",
										image.Exists(),
										wantImage,
										calls,
										bytes.Equal(payload, original),
										captured,
									)
								}
								if override {
									if image.Get("parameters.description").String() != "configured" || len(
										gjson.GetBytes(
											captured, "tools",
										).Array(),
									) != 1 {
										t.Fatalf("native payload override lost final authority: %s", captured)
									}
								} else if !gjson.GetBytes(captured, `tools.#(name=="lookup")`).Exists() {
									t.Fatalf("ordinary lookup tool was removed: %s", captured)
								}
							},
						)
					}
				}
			}
		}
	}
}
