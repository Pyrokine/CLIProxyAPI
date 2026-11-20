package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHomeCodexTerminalStreamFailureUsesFreshDispatchOnNextRequest(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		name := "legacy"
		if acknowledged {
			name = "acknowledged"
		}
		t.Run(
			name, func(t *testing.T) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				var connections atomic.Int32
				peerClosed := make(chan struct{}, 2)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(writer http.ResponseWriter, request *http.Request) {
							conn, err := upgrader.Upgrade(writer, request, nil)
							if err != nil {
								t.Errorf("upgrade websocket: %v", err)
								return
							}
							defer func(closeResource func() error) { _ = closeResource() }(conn.Close)
							defer func() { peerClosed <- struct{}{} }()
							if _, _, err := conn.ReadMessage(); err != nil {
								return
							}
							if connections.Add(1) == 1 {
								if err := conn.WriteJSON(
									map[string]any{
										"type": "response.created", "response": map[string]any{"id": "response-1"},
									},
								); err != nil {
									t.Error(err)
									return
								}
								err = conn.WriteJSON(
									map[string]any{
										"type": "error", "status": http.StatusBadGateway,
										"error": map[string]any{"message": "terminal failure"},
									},
								)
							} else {
								err = conn.WriteJSON(
									map[string]any{
										"type":     "response.completed",
										"response": map[string]any{"id": "response-2", "output": []any{}},
									},
								)
							}
							if err != nil {
								t.Error(err)
								return
							}
							for {
								if _, _, err := conn.ReadMessage(); err != nil {
									return
								}
							}
						},
					),
				)
				defer server.Close()

				credential := auth.Auth{
					ID: "home-codex", Provider: "codex", Status: auth.StatusActive,
					Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
				}
				payload, err := json.Marshal(credential)
				if err != nil {
					t.Fatal(err)
				}
				dispatcher := &auditWebsocketHomeDispatcher{
					payload: payload,
				}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				registry := executionregistry.New()
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				base := newCodexWebsocketsExecutor(&config.Config{})
				spy := &auditWebsocketLifecycleExecutor{ProviderExecutor: base, closed: make(chan struct{})}
				manager.RegisterExecutor(spy)
				const sessionID = "terminal-home-session"
				defer manager.CloseExecutionSession(sessionID)
				defer base.CloseExecutionSession(sessionID)

				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = core.WithDownstreamWebsocket(ctx)
				if acknowledged {
					ctx = core.WithStreamDeliveryAcknowledgement(ctx)
				}
				opts := core.Options{
					Stream: true, SourceFormat: translator.FormatOpenAIResponse,
					ResponseFormat: translator.FormatOpenAIResponse,
					Metadata:       map[string]any{core.ExecutionSessionMetadataKey: sessionID},
				}
				request := core.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[]}`)}
				first, err := manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
				if err != nil {
					t.Fatalf("first ExecuteStream error = %v", err)
				}
				var firstFailure error
				for chunk := range first.Chunks {
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
					if chunk.Err != nil {
						firstFailure = chunk.Err
					}
				}
				if firstFailure == nil || !strings.Contains(firstFailure.Error(), "terminal failure") {
					t.Fatalf("first stream error = %v, want original terminal failure", firstFailure)
				}
				firstOwner := spy.owner
				select {
				case <-peerClosed:
				case <-ctx.Done():
					t.Fatal("terminal failure did not close the first upstream connection")
				}
				if firstOwner == nil || firstOwner.Active() || spy.closeCount.Load() != 1 || ctx.Err() != nil {
					t.Fatalf(
						"failed Home owner active=%t close callbacks=%d downstream=%v",
						firstOwner != nil && firstOwner.Active(), spy.closeCount.Load(), ctx.Err(),
					)
				}
				second, err := manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
				if err != nil {
					t.Fatalf("second ExecuteStream error = %v", err)
				}
				var output strings.Builder
				for chunk := range second.Chunks {
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
					if chunk.Err != nil {
						t.Fatalf("second stream error = %v", chunk.Err)
					}
					output.Write(chunk.Payload)
				}
				if dispatcher.calls.Load() != 2 || connections.Load() != 2 {
					t.Fatalf(
						"Home RPOP calls=%d websocket connections=%d, want 2/2",
						dispatcher.calls.Load(), connections.Load(),
					)
				}
				if spy.owner == firstOwner || !strings.Contains(output.String(), "response-2") {
					t.Fatalf("next request did not use a fresh Home owner and deliver response-2: %s", output.String())
				}
			},
		)
	}
}
