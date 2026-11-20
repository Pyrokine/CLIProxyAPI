package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

func TestCodexHomeEndClosesAndNotifiesIdleTransport(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				var handshakes atomic.Int32
				peerClosed := make(chan struct{}, 1)
				upstream := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							handshakes.Add(1)
							defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
							defer func() { peerClosed <- struct{}{} }()
							if _, _, err = peer.ReadMessage(); err != nil {
								t.Error(err)
								return
							}
							terminal := `{"type":"response.completed","response":{"id":"synthetic-home-end",` +
								`"status":"completed","model":"gpt-5.4","output":[],` +
								`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
							if err = peer.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
								t.Error(err)
								return
							}
							_, _, _ = peer.ReadMessage()
						},
					),
				)
				defer upstream.Close()
				cfg := &config.Config{
					Home: config.HomeConfig{Enabled: true}, DisableImageGeneration: config.DisableImageGenerationAll,
				}
				executor := newCodexWebsocketsExecutor(cfg)
				sessionID := t.Name()
				defer executor.CloseExecutionSession(sessionID)
				session := executor.getOrCreateSession(sessionID)
				disconnected := executor.UpstreamDisconnectChan(sessionID)
				spy := &auditWebsocketLifecycleExecutor{ProviderExecutor: executor, closed: make(chan struct{})}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.RegisterExecutor(spy)
				credential := &auth.Auth{
					ID: t.Name(), Provider: "codex",
					Attributes: map[string]string{
						"api_key": "synthetic-key", "base_url": upstream.URL, "websockets": "true",
						"auth_kind": "apikey",
					},
				}
				payload, err := json.Marshal(
					map[string]any{
						"model": "gpt-5.4", "model_info": map[string]any{"id": "gpt-5.4", "context_length": 32768},
						"auth": credential,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				dispatcher := &auditWebsocketHomeDispatcher{
					payload: payload,
				}
				registry := executionregistry.New()
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = core.WithDownstreamWebsocket(core.WithStreamDeliveryAcknowledgement(ctx))
				opts := core.Options{
					SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse,
					Stream: mode == "stream", Metadata: map[string]any{core.ExecutionSessionMetadataKey: sessionID},
				}
				req := core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`)}
				if mode == "execute" {
					_, err = manager.Execute(ctx, []string{"codex"}, req, opts)
				} else {
					var result *core.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
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
				if spy.owner == nil || !spy.owner.Retained() || spy.closeCount.Load() != 0 ||
					handshakes.Load() != 1 || dispatcher.calls.Load() != 1 {
					t.Fatal("successful downstream websocket did not retain its real Home owner")
				}
				spy.owner.End("synthetic-owner-end")
				select {
				case <-peerClosed:
				case <-ctx.Done():
					t.Fatal("Home End did not close the retained upstream connection")
				}
				select {
				case disconnectErr, open := <-disconnected:
					if !open || disconnectErr != nil {
						t.Fatalf(
							"intentional Home closure lost its first disconnect notification: %v, open=%t",
							disconnectErr, open,
						)
					}
				case <-time.After(time.Second):
					t.Fatal("Home End closed transport without notifying the idle session consumer")
				}
				session.connMu.Lock()
				retained := session.conn != nil || session.homeLifecycle != nil
				session.connMu.Unlock()
				if retained || spy.owner.Active() || spy.closeCount.Load() != 1 {
					t.Fatal("Home End retained connection state or failed to release its owner once")
				}
				spy.owner.End("repeated-end")
				executor.CloseExecutionSession(sessionID)
				if _, open := <-disconnected; open {
					t.Fatal("repeated closure produced another disconnect notification")
				}
				if err = registry.Drain(ctx); err != nil || spy.closeCount.Load() != 1 {
					t.Fatalf("registry drain duplicated or failed Home release: %v", err)
				}
			},
		)
	}
}
