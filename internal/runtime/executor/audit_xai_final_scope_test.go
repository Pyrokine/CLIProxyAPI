package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditXAIFinalModelReplayAcrossPublicTransports(t *testing.T) {
	for _, mode := range []string{"http-execute", "http-stream", "websocket-stream"} {
		for _, same := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/same=%t", mode, same), func(t *testing.T) {
					internalcache.ClearXAIReasoningReplayCache()
					t.Cleanup(internalcache.ClearXAIReasoningReplayCache)
					proof := validGrokEncryptedContentForExecutorTest()
					firstOutput := fmt.Sprintf(
						`[{"type":"reasoning","summary":[],"encrypted_content":%q},{"type":"function_call","`+
							`id":"call-item","call_id":"call_1","name":"lookup","arguments":"{}"}]`,
						proof,
					)
					received := make(chan []byte, 2)
					var calls atomic.Int32
					response := func() string {
						turn := calls.Add(1)
						output, model := firstOutput, "grok-4.6"
						if turn == 2 {
							output = `[{"type":"message","role":"assistant","content":[{"type":` +
								`"output_text","text":` +
								`"done"}]}]`
							if !same {
								model = "grok-4.7"
							}
						}
						return fmt.Sprintf(
							`{"type":"response.completed","response":{"id":"synthetic-%d","status":"`+
								`completed","model":%q,"output":%s,"usage":{"input_tokens":1,"output_tokens":`+
								`1,"total_tokens":2}}}`,
							turn, model, output,
						)
					}
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
									for {
										_, body, err := peer.ReadMessage()
										if err != nil {
											return
										}
										received <- body
										if err = peer.WriteMessage(
											websocket.TextMessage, []byte(response()),
										); err != nil {
											return
										}
									}
								}
								body, err := io.ReadAll(r.Body)
								if err != nil {
									t.Error(err)
									return
								}
								received <- body
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: "+response()+"\n\n")
							},
						),
					)
					defer server.Close()
					credential := &auth.Auth{
						ID: t.Name(), Provider: "xai",
						Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL},
					}
					ctx, cancel := context.WithTimeout(
						xaiC03ContextWithAPIKey("synthetic-final-model-caller"), 5*time.Second,
					)
					defer cancel()
					opts := core.Options{
						SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse,
						Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name()},
					}
					for turn := range 2 {
						final := "grok-4.6"
						if turn == 1 && !same {
							final = "grok-4.7"
						}
						cfg := &config.Config{
							Payload: config.PayloadConfig{
								Override: []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{{Name: "grok-4.3"}},
										Params: map[string]any{"model": final},
									},
								},
							},
						}
						var executor auth.ProviderExecutor = newXAIExecutor(cfg)
						if mode == "websocket-stream" {
							ws := newXAIWebsocketsExecutor(cfg)
							// 两个请求轮次共享会话，完成缓存隔离断言后统一关闭
							// noinspection GoDeferInLoop
							defer ws.CloseExecutionSession(t.Name())
							executor = ws
						}
						input := `[{"role":"user","content":"earlier"}]`
						if turn == 1 {
							input = `[{"role":"user","content":"earlier"},{"type":"function_call_output","c` +
								`all_id":"call_1","output":"result"},{"role":"user","content":"continue` +
								`"}]`
						}
						req := core.Request{
							Model: "grok-4.3",
							Payload: []byte(
								`{"model":"grok-4.3","input":` +
									input +
									(`,"tools":[{"type":"function",` +
										`"name":"lookup","parameters":` +
										`{"type":"object"}}]}`),
							),
						}
						var err error
						if mode == "http-execute" {
							_, err = executor.Execute(ctx, credential, req, opts)
						} else {
							var result *core.StreamResult
							result, err = executor.ExecuteStream(ctx, credential, req, opts)
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
						}
						if err != nil {
							t.Fatal(err)
						}
						wire := <-received
						if gjson.GetBytes(wire, "model").String() != final {
							t.Fatalf("wire final model=%s", wire)
						}
						var base []string
						if mode == "websocket-stream" {
							base = []string{server.URL}
						}
						scope := xaiReasoningReplayScopeFromRequest(
							ctx, credential, opts.SourceFormat, req, opts, wire, base...,
						)
						if scope.modelName != final {
							t.Fatalf("scope model=%s final=%s", scope.modelName, final)
						}
						if turn == 0 {
							items, found, err := internalcache.GetXAIReasoningReplayItemsRequired(
								ctx, final, scope.sessionKey,
							)
							if err != nil || !found || len(items) == 0 {
								t.Fatalf("final model cache items=%d found=%t error=%v", len(items), found, err)
							}
							if _, found, err := internalcache.GetXAIReasoningReplayItemsRequired(
								ctx, "grok-4.3", scope.sessionKey,
							); err != nil || found {
								t.Fatalf("original model cache found=%t error=%v", found, err)
							}
						} else {
							restored := false
							for _, item := range gjson.GetBytes(wire, "input").Array() {
								restored = restored || item.Get("encrypted_content").String() == proof
							}
							if restored != same {
								t.Fatalf("same final=%t restored=%t wire=%s", same, restored, wire)
							}
						}
					}
				},
			)
		}
	}
}
