package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditCodexWebsocketContextChecksBeforeHandshake(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, target := range []string{"same", "small", "other-capable"} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", streaming, target), func(t *testing.T) {
					var handshakes atomic.Int32
					final := "gpt-5.4"
					if target != "same" {
						final += "-" + target
					}
					upgrader := websocket.Upgrader{}
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, r *http.Request) {
								handshakes.Add(1)
								connection, err := upgrader.Upgrade(w, r, nil)
								if err != nil {
									t.Error(err)
									return
								}
								defer func(closeResource func() error) { _ = closeResource() }(connection.Close)
								_, body, err := connection.ReadMessage()
								if err != nil {
									t.Error(err)
									return
								}
								if gjson.GetBytes(body, "model").String() != final {
									t.Errorf("model=%s", body)
								}
								_ = connection.WriteMessage(
									websocket.TextMessage,
									[]byte(
										(`{"type":"response.completed","response":{"id":"synthetic","object":`+
											`"response","status":"completed","model":`+
											`"`)+
											final+
											(`","output":`+
												`[]}}`),
									),
								)
							},
						),
					)
					defer server.Close()
					cfg := &config.Config{}
					cfg.Payload.Override = []config.PayloadRule{
						{
							Models: []config.PayloadModelRule{
								{Name: "gpt-5.4"},
							}, Params: map[string]any{"model": final},
						},
					}
					manager := auth.NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
					manager.SetConfig(cfg)
					manager.RegisterExecutor(newCodexWebsocketsExecutor(cfg))
					credential, err := manager.Register(
						t.Context(), &auth.Auth{
							ID: t.Name(), Provider: "codex", Attributes: map[string]string{
								"api_key": "synthetic-key", "base_url": server.URL, "websockets": "true",
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(
						credential.ID, "codex", []*registry.ModelInfo{
							{
								ID:            "gpt-5.4",
								ContextLength: 1_000_000,
							}, {ID: "gpt-5.4-small", ContextLength: 128_000},
							{ID: "gpt-5.4-other-capable", MaxContextLength: 1_000_000},
						},
					)
					t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
					manager.RefreshSchedulerEntry(credential.ID)
					const session = "audit-context-websocket"
					defer manager.CloseExecutionSession(session)
					timeoutCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					ctx := core.WithDownstreamWebsocket(timeoutCtx)
					request := core.Request{
						Model: "gpt-5.4[1m]", Payload: []byte(`{"model":"gpt-5.4[1m]","input":"synthetic"}`),
					}
					opts := core.Options{
						SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatCodex,
						Metadata: map[string]any{core.ExecutionSessionMetadataKey: session},
					}
					if streaming {
						var result *core.StreamResult
						result, err = manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(chunk.Err)
								}
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = manager.Execute(ctx, []string{"codex"}, request, opts)
					}
					if target == "small" {
						status, ok := err.(interface{ StatusCode() int })
						if !ok || status.StatusCode() != http.StatusBadRequest || handshakes.Load() != 0 {
							t.Fatalf("error=%v handshakes=%d", err, handshakes.Load())
						}
					} else if err != nil || handshakes.Load() != 1 {
						t.Fatalf("error=%v handshakes=%d", err, handshakes.Load())
					}
				},
			)
		}
	}
}
