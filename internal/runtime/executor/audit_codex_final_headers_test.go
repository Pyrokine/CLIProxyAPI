package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func auditRegisterHeaderModel(t *testing.T, client, provider, model, value string) {
	t.Helper()
	var info registry.ModelInfo
	raw, err := json.Marshal(
		map[string]any{
			"id": model, "config": map[string]any{
				"override_header": map[string]string{
					codexRoutingHintHeader: value, "X-Model-Authority": provider,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(client, provider, []*registry.ModelInfo{&info})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(client) })
}

func TestAuditCodexFinalModelHeadersReachEveryTransport(t *testing.T) {
	for _, source := range []translator.Format{translator.FormatCodex, translator.FormatOpenAIResponse} {
		for _, apiKey := range []bool{false, true} {
			for _, forced := range []bool{false, true} {
				for _, operator := range []bool{false, true} {
					for _, transport := range []string{"http", "http-stream", "compact", "ws", "ws-stream"} {
						t.Run(
							fmt.Sprintf(
								"%s/api-key=%t/forced=%t/operator=%t/%s",
								source,
								apiKey,
								forced,
								operator,
								transport,
							), func(t *testing.T) {
								const finalModel = "audit-final-codex-header-model"
								if forced {
									auditRegisterHeaderModel(
										t, t.Name()+"-codex", "codex", finalModel, "model-forced-hint",
									)
								}
								auditRegisterHeaderModel(
									t, t.Name()+"-foreign", "gemini", finalModel, "foreign-provider-hint",
								)
								type observed struct {
									body    []byte
									headers http.Header
								}
								received := make(chan observed, 1)
								server := httptest.NewServer(
									http.HandlerFunc(
										func(w http.ResponseWriter, r *http.Request) {
											if websocket.IsWebSocketUpgrade(r) {
												peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
												if err != nil {
													t.Error(err)
													return
												}
												defer func(closeResource func() error) {
													_ =
														closeResource()
												}(peer.Close)
												_, body, err := peer.ReadMessage()
												if err != nil {
													t.Error(err)
													return
												}
												received <- observed{body, r.Header.Clone()}
												if err = peer.WriteMessage(
													websocket.TextMessage, []byte(runtimeCodexCompleted),
												); err != nil {
													t.Error(err)
												}
												return
											}
											body, err := io.ReadAll(r.Body)
											if err != nil {
												t.Error(err)
												return
											}
											received <- observed{body, r.Header.Clone()}
											if strings.HasSuffix(r.URL.Path, "/compact") {
												w.Header().Set("Content-Type", "application/json")
												_, _ = io.WriteString(
													w,
													(`{"id":"synthetic-compact","object":`+
														`"response.compaction","model":`+
														`"`)+
														finalModel+
														(`","output":[],`+
															`"usage":`+
															`{"input_tokens`+
															`":1,"output_to`+
															`kens":1,`+
															`"total_tokens"`+
															`:2}}`),
												)
												return
											}
											w.Header().Set("Content-Type", "text/event-stream")
											_, _ = io.WriteString(w, "data: "+runtimeCodexCompleted+"\n\n")
										},
									),
								)
								defer server.Close()
								cfg := &config.Config{
									DisableImageGeneration: config.DisableImageGenerationAll,
									Payload: config.PayloadConfig{
										Override: []config.PayloadRule{
											{
												Models: []config.PayloadModelRule{{Name: "gpt-5.4"}},
												Params: map[string]any{
													"model":        finalModel,
													"service_tier": "priority",
												},
											},
										},
									},
								}
								credential := codexOAuthTestAuth(server.URL)
								if apiKey {
									credential = codexAPIKeyTestAuth(server.URL)
								}
								credential.ID = t.Name()
								if operator {
									credential.Attributes["header:x-codex-routing-hint"] = "operator-hint"
								}
								opts := core.Options{
									SourceFormat: source, ResponseFormat: translator.FormatOpenAIResponse,
									Headers: http.Header{codexRoutingHintHeader: {"client-hint"}},
								}
								if transport == "compact" {
									opts.Alt = "responses/compact"
								}
								ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
								defer cancel()
								if strings.HasPrefix(transport, "ws") {
									ctx = core.WithDownstreamWebsocket(ctx)
								}
								execute := NewCodexAutoExecutor(cfg)
								request := core.Request{
									Model: "gpt-5.4",
									Payload: []byte(
										`{"model":"gpt-5.4","input":"synthetic","service_tier":"default"}`,
									),
								}
								var err error
								if strings.HasSuffix(transport, "stream") {
									var stream *core.StreamResult
									stream, err = execute.ExecuteStream(ctx, credential, request, opts)
									if err == nil {
										for chunk := range stream.Chunks {
											if chunk.Err != nil {
												err = chunk.Err
											}
										}
									}
								} else {
									_, err = execute.Execute(ctx, credential, request, opts)
								}
								if err != nil {
									t.Fatal(err)
								}
								wire := <-received
								if gjson.GetBytes(wire.body, "model").String() != finalModel {
									t.Fatalf("final body=%s", wire.body)
								}
								hint := wire.headers.Get(codexRoutingHintHeader)
								if forced {
									if hint != "model-forced-hint" ||
										wire.headers.Get("X-Model-Authority") != "codex" {
										t.Fatalf("forced headers=%v", wire.headers)
									}
								} else if operator {
									if hint != "operator-hint" {
										t.Fatalf("operator hint=%q", hint)
									}
								} else if !apiKey && hint != "model="+finalModel+";tier=priority" {
									t.Fatalf("derived hint=%q", hint)
								}
								if hint == "foreign-provider-hint" ||
									wire.headers.Get("X-Model-Authority") == "gemini" {
									t.Fatalf("foreign provider headers=%v", wire.headers)
								}
							},
						)
					}
				}
			}
		}
	}
}

func TestAuditCodexPersistentHandshakeTracksFinalModelHeaders(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
				const firstModel = "audit-header-first-model"
				const secondModel = "audit-header-second-model"
				auditRegisterHeaderModel(t, t.Name()+"-first", "codex", firstModel, "forced-first")
				auditRegisterHeaderModel(t, t.Name()+"-second", "codex", secondModel, "forced-second")
				type observed struct {
					connection int32
					headers    http.Header
					model      string
				}
				received := make(chan observed, 4)
				var connections atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
							number := connections.Add(1)
							turn := 0
							for {
								_, body, readErr := peer.ReadMessage()
								if readErr != nil {
									return
								}
								turn++
								received <- observed{number, r.Header.Clone(), gjson.GetBytes(body, "model").String()}
								completed, err := sjson.Set(
									runtimeCodexCompleted,
									"response.id",
									fmt.Sprintf("resp_audit_%d_%d", number, turn),
								)
								if err != nil {
									t.Error(err)
									return
								}
								if err = peer.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
									return
								}
							}
						},
					),
				)
				defer server.Close()
				cfg := &config.Config{
					DisableImageGeneration: config.DisableImageGenerationAll, Payload: config.PayloadConfig{
						Override: []config.PayloadRule{
							{
								Models: []config.PayloadModelRule{
									{
										Name: "gpt-5.4", Headers: map[string]string{"X-Final-Model": "first"},
									},
								}, Params: map[string]any{"model": firstModel},
							},
							{
								Models: []config.PayloadModelRule{
									{
										Name: "gpt-5.4", Headers: map[string]string{"X-Final-Model": "second"},
									},
								}, Params: map[string]any{"model": secondModel},
							},
						},
					},
				}
				execute := newCodexWebsocketsExecutor(cfg)
				defer execute.CloseExecutionSession(t.Name())
				credential := codexOAuthTestAuth(server.URL)
				credential.ID = t.Name()
				ctx, cancel := context.WithTimeout(core.WithDownstreamWebsocket(t.Context()), 4*time.Second)
				defer cancel()
				for turn, selector := range []string{"first", "first", "second", "second"} {
					if turn == 3 {
						auditRegisterHeaderModel(
							t,
							t.Name()+"-second",
							"codex",
							secondModel,
							"forced-second-updated",
						)
					}
					opts := core.Options{
						SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse,
						Headers:  http.Header{"X-Final-Model": {selector}},
						Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name()},
					}
					request := core.Request{
						Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`),
					}
					var err error
					if streaming {
						var stream *core.StreamResult
						stream, err = execute.ExecuteStream(ctx, credential, request, opts)
						if err == nil {
							for chunk := range stream.Chunks {
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = execute.Execute(ctx, credential, request, opts)
					}
					if err != nil {
						t.Fatal(err)
					}
					wire := <-received
					wantConnection := []int32{1, 1, 2, 3}[turn]
					wantHint := []string{"forced-first", "forced-first", "forced-second", "forced-second-updated"}[turn]
					wantModel := firstModel
					if selector == "second" {
						wantModel = secondModel
					}
					if wire.connection != wantConnection ||
						wire.headers.Get(codexRoutingHintHeader) != wantHint ||
						wire.model != wantModel {
						t.Fatalf(
							"turn=%d connection=%d hint=%q model=%s", turn, wire.connection,
							wire.headers.Get(codexRoutingHintHeader), wire.model,
						)
					}
				}
			},
		)
	}
}
