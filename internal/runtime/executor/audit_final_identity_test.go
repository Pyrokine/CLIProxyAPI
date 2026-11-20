package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditCodexFinalModelCacheIsolationAcrossTransports(t *testing.T) {
	claudeScopes := make(map[string]string)
	claudeReplayScopes := make(map[string]string)
	for _, identity := range []string{
		"responses-explicit", "chat-explicit", "execution-session", "api-key", "claude", "claude-root-delimiter",
		"claude-agent-delimiter",
	} {
		for _, confuse := range []bool{false, true} {
			t.Run(
				identity+map[bool]string{false: "/plain", true: "/confused"}[confuse], func(t *testing.T) {
					keys := make(map[string]string)
					for _, transport := range []string{"http", "ws"} {
						for _, stream := range []bool{false, true} {
							for _, finalModel := range []string{"gpt-5.5", "gpt-5.5", "gpt-5.6"} {
								type observed struct {
									body    []byte
									headers http.Header
								}
								received := make(chan observed, 1)
								server := httptest.NewServer(
									http.HandlerFunc(
										func(w http.ResponseWriter, r *http.Request) {
											var body []byte
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
												_, body, err = peer.ReadMessage()
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
											w.Header().Set("Content-Type", "text/event-stream")
											_, _ = io.WriteString(w, "data: "+runtimeCodexCompleted+"\n\n")
										},
									),
								)
								cfg := &config.Config{
									DisableImageGeneration: config.DisableImageGenerationAll,
									Payload: config.PayloadConfig{
										Override: []config.PayloadRule{
											{
												Models: []config.PayloadModelRule{
													{
														Name: "gpt-5.4", Protocol: "codex",
													},
												}, Params: map[string]any{"model": finalModel},
											},
										},
									},
								}
								cfg.Codex.IdentityConfuse = confuse
								cfg.Routing.SessionAffinity = true
								request := core.Request{
									Model: "gpt-5.4",
									Payload: []byte(
										`{"model":"gpt-5.4","input":"synthetic","prompt_cache_key":` +
											`"same-client-session"}`,
									),
								}
								opts := core.Options{SourceFormat: translator.FormatOpenAIResponse}
								switch identity {
								case "chat-explicit":
									opts.SourceFormat = translator.FormatOpenAI
									request.Payload = []byte(
										`{"model":"gpt-5.4","messages":[{"role":"user","content":` +
											`"synthetic"}],"prompt_cache_key":` +
											`"same-client-session"}`,
									)
								case "api-key":
									opts.SourceFormat = translator.FormatOpenAI
									request.Payload = []byte(
										`{"model":"gpt-5.4","messages":[{"role":"user","content":"synthetic"}]}`,
									)
								case "execution-session":
									request.Payload = []byte(`{"model":"gpt-5.4","input":"synthetic"}`)
									request.Metadata = map[string]any{
										core.ExecutionSessionMetadataKey: "same-execution",
									}
								case "claude", "claude-root-delimiter", "claude-agent-delimiter":
									opts.SourceFormat = translator.FormatClaude
									request.Payload = []byte(
										`{"model":"gpt-5.4","metadata":{"user_id":"{\"session_id\":\"same-claud` +
											`e-session\"}"},"messages":[{"role":"user","content":"synthetic"}]}`,
									)
								}
								if identity == "claude-root-delimiter" {
									opts.Headers = http.Header{
										"X-Claude-Code-Session-Id": {"root:agent:child"},
										"X-Claude-Code-Agent-Id":   {"leaf"},
									}
								} else if identity == "claude-agent-delimiter" {
									opts.Headers = http.Header{
										"X-Claude-Code-Session-Id": {"root"},
										"X-Claude-Code-Agent-Id":   {"child:agent:leaf"},
									}
								}
								recorder := httptest.NewRecorder()
								ginCtx, _ := gin.CreateTestContext(recorder)
								ginCtx.Set("apiKey", "synthetic-api-key")
								ctx := util.WithGinContext(t.Context(), ginCtx)
								if transport == "ws" {
									ctx = core.WithDownstreamWebsocket(ctx)
								}
								ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
								executor := NewCodexAutoExecutor(cfg)
								credential := runtimeCodexAuth(server.URL)
								var err error
								if stream {
									var result *core.StreamResult
									result, err = executor.ExecuteStream(ctx, credential, request, opts)
									if err == nil {
										for chunk := range result.Chunks {
											if chunk.Acknowledge != nil {
												chunk.Acknowledge(nil)
											}
											if chunk.Err != nil {
												err = chunk.Err
											}
										}
									}
								} else {
									_, err = executor.Execute(ctx, credential, request, opts)
								}
								cancel()
								server.Close()
								if err != nil {
									t.Fatalf("%s stream=%t final=%s: %v", transport, stream, finalModel, err)
								}
								wire := <-received
								scope := codexReasoningReplayScopeFromRequest(
									ctx, credential, opts.SourceFormat, request, opts, wire.body,
								)
								claudeReplayScopes[fmt.Sprintf("%s/%t", identity, confuse)] = scope.sessionKey
								key := gjson.GetBytes(wire.body, "prompt_cache_key").String()
								if key == "" || wire.headers.Get("Conversation_id") != key ||
									codexSessionHeaderValue(wire.headers) != key {
									t.Fatalf(
										"%s stream=%t body=%s headers=%v",
										transport,
										stream,
										wire.body,
										wire.headers,
									)
								}
								if gjson.GetBytes(wire.body, "model").String() != finalModel {
									t.Fatalf("final model mismatch: %s", wire.body)
								}
								if previous, ok := keys[finalModel]; ok && previous != key {
									t.Fatalf(
										"same final model identity changed across attempts/transports: %s vs %s",
										previous,
										key,
									)
								}
								keys[finalModel] = key
							}
						}
					}
					if keys["gpt-5.5"] == keys["gpt-5.6"] {
						t.Fatal("different final models shared cache/session identity")
					}
					claudeScopes[fmt.Sprintf("%s/%t", identity, confuse)] = keys["gpt-5.5"]
				},
			)
		}
	}
	for _, confuse := range []bool{false, true} {
		left, right := fmt.Sprintf("claude-root-delimiter/%t", confuse), fmt.Sprintf(
			"claude-agent-delimiter/%t", confuse,
		)
		if claudeReplayScopes[left] == "" || claudeReplayScopes[left] == claudeReplayScopes[right] {
			t.Fatalf("distinct Claude session/agent hierarchies share replay scope, confuse=%t", confuse)
		}
		if claudeScopes[fmt.Sprintf(
			"claude-root-delimiter/%t", confuse,
		)] == claudeScopes[fmt.Sprintf("claude-agent-delimiter/%t", confuse)] {
			t.Fatalf("distinct Claude session/agent hierarchies share identity, confuse=%t", confuse)
		}
	}
}

func TestAuditCodexReplayUsesFinalPayloadModel(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			t.Run(
				transport+map[bool]string{false: "/execute", true: "/stream"}[stream], func(t *testing.T) {
					internalcache.ClearCodexReasoningReplayCache()
					t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
					completed := []byte(
						(`{"type":"response.completed","response":{"id":"synthetic","status":"completed","` +
							`model":"gpt-5.5","output":[{"type":"reasoning","summary":[],"content":null,"` +
							`encrypted_content":"`) +
							validCodexReplayEncryptedContentForTest(21) +
							(`"},{"type":` +
								`"function_call` +
								`","call_id":` +
								`"call_1",` +
								`"name":"lookup` +
								`","arguments":` +
								`"{}"}]}}`),
					)
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, r *http.Request) {
								if websocket.IsWebSocketUpgrade(r) {
									peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
									if err != nil {
										t.Error(err)
										return
									}
									defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
									if _, _, err = peer.ReadMessage(); err != nil {
										t.Error(err)
										return
									}
									if err = peer.WriteMessage(websocket.TextMessage, completed); err != nil {
										t.Error(err)
									}
									return
								}
								_, _ = io.ReadAll(r.Body)
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
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
											Name: "gpt-5.4", Protocol: "codex",
										},
									}, Params: map[string]any{"model": "gpt-5.5"},
								},
							},
						},
					}
					req := core.Request{
						Model: "gpt-5.4",
						Payload: []byte(
							`{"model":"gpt-5.4","metadata":{"user_id":"{\"session_id\":\"audit-repl` +
								`ay-final-model\"}"},"tools":[{"name":"lookup","input_schema":{"type":"` +
								`object","properties":{}}}],"messages":[{"role":"user","content":"synth` +
								`etic"}]}`,
						),
					}
					opts := core.Options{SourceFormat: translator.FormatClaude}
					ctx := home.WithClientScope(
						t.Context(),
						home.ClientScope{ID: "audit-final-model", Enabled: false},
					)
					var execute auth.ProviderExecutor = newCodexExecutor(cfg)
					if transport == "ws" {
						execute = newCodexWebsocketsExecutor(cfg)
					}
					var err error
					if stream {
						var result *core.StreamResult
						result, err = execute.ExecuteStream(ctx, runtimeCodexAuth(server.URL), req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = execute.Execute(ctx, runtimeCodexAuth(server.URL), req, opts)
					}
					if err != nil {
						t.Fatal(err)
					}
					scope := codexReasoningReplayScopeFromRequest(
						ctx, runtimeCodexAuth(server.URL), translator.FormatClaude, req, opts,
						[]byte(`{"model":"gpt-5.5","input":[]}`),
					)
					items, found, err := internalcache.GetCodexReasoningReplayItemsRequired(
						ctx, "gpt-5.5", scope.sessionKey,
					)
					if err != nil || !found || len(items) == 0 {
						t.Fatalf("final-model replay items=%d found=%t error=%v", len(items), found, err)
					}
					if _, found, err := internalcache.GetCodexReasoningReplayItemsRequired(
						ctx, "gpt-5.4", scope.sessionKey,
					); err != nil || found {
						t.Fatalf("old-model replay found=%t error=%v", found, err)
					}
				},
			)
		}
	}
}

func TestAuditOpenAICompatDerivedCacheUsesFinalModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		keys := map[string]string{}
		for _, final := range []string{"final-model-b", "final-model-b", "final-model-c"} {
			received := make(chan []byte, 1)
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						received <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(
								w,
								"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic\"},\""+
									"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\""+
									"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n",
							)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(
								w,
								`{"choices":[{"index":0,"message":{"role":"assistant","content":"synthetic"},`+
									`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":`+
									`1,"total_tokens":2}}`,
							)
						}
					},
				),
			)
			cfg := &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{
					{
						Name: "audit-compatible", SupportPromptCacheKey: true,
					},
				}, Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{
							Models: []config.PayloadModelRule{
								{
									Name: "initial-model", Protocol: "openai",
								},
							}, Params: map[string]any{"model": final},
						},
					},
				},
			}
			credential := runtimeCodexAuth(server.URL)
			credential.Provider = "audit-compatible"
			credential.Attributes["compat_name"] = "audit-compatible"
			req := core.Request{
				Model:   "initial-model",
				Payload: []byte(`{"model":"initial-model","messages":[{"role":"user","content":"synthetic"}]}`),
			}
			opts := core.Options{
				SourceFormat: translator.FormatOpenAI,
				Metadata:     map[string]any{core.ExecutionSessionMetadataKey: "same-session"},
			}
			execute := NewOpenAICompatExecutor("audit-compatible", cfg)
			var err error
			if stream {
				var result *core.StreamResult
				result, err = execute.ExecuteStream(t.Context(), credential, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = execute.Execute(t.Context(), credential, req, opts)
			}
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			body := <-received
			key := gjson.GetBytes(body, "prompt_cache_key").String()
			if key == "" || gjson.GetBytes(body, "model").String() != final {
				t.Fatalf("body=%s", body)
			}
			if previous, ok := keys[final]; ok && previous != key {
				t.Fatal("same final model changed cache identity")
			}
			keys[final] = key
		}
		if keys["final-model-b"] == keys["final-model-c"] {
			t.Fatal("different final models shared derived cache identity")
		}
	}
}
