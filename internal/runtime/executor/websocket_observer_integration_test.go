package executor

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

type webSocketObserverExecuteStream func(
	context.Context,
	*cliproxyauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error)

func TestWebSocketExecutorsPublishRawUpstreamFrames(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		model    string
		execute  func(*config.Config) webSocketObserverExecuteStream
	}{
		{
			name:     "codex",
			provider: "codex",
			model:    "gpt-5.5",
			execute: func(cfg *config.Config) webSocketObserverExecuteStream {
				return newCodexWebsocketsExecutor(cfg).ExecuteStream
			},
		},
		{
			name:     "xai",
			provider: "xai",
			model:    "grok-4.3",
			execute: func(cfg *config.Config) webSocketObserverExecuteStream {
				return newXAIWebsocketsExecutor(cfg).ExecuteStream
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`)
			server := newWebsocketReadPhaseServer(t, websocket.TextMessage, payload)
			defer server.Close()

			events := make(chan cliproxyexecutor.WebSocketResponseEvent, 2)
			auth := &cliproxyauth.Auth{
				ID:       test.provider + "-observer-auth",
				Label:    "observer-account",
				Provider: test.provider,
				Attributes: map[string]string{
					"api_key":  "test-token",
					"base_url": server.URL,
				},
			}
			stream, errExecute := test.execute(&config.Config{})(
				cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
				auth,
				cliproxyexecutor.Request{
					Model:   test.model,
					Payload: []byte(`{"model":"` + test.model + `","input":"hello"}`),
				},
				cliproxyexecutor.Options{
					Stream:         true,
					SourceFormat:   sdktranslator.FormatOpenAIResponse,
					ResponseFormat: sdktranslator.FormatOpenAIResponse,
					Metadata: map[string]any{
						cliproxyexecutor.RequestedModelMetadataKey: "requested-alias",
					},
					WebSocketResponseObserver: func(_ context.Context, event cliproxyexecutor.WebSocketResponseEvent) {
						events <- event
					},
				},
			)
			if errExecute != nil {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}
			_ = streamTerminalError(t, stream)

			select {
			case event := <-events:
				if event.Provider != test.provider || event.AuthID != auth.ID || event.AuthLabel != auth.Label {
					t.Fatalf("event identity = %+v", event)
				}
				if event.EventType != "response.created" || string(event.Payload) != string(payload) {
					t.Fatalf("event frame = %+v payload=%s", event, event.Payload)
				}
				if event.Model != test.model || event.RequestedModel != "requested-alias" {
					t.Fatalf("event models = (%q, %q)", event.Model, event.RequestedModel)
				}
			case <-time.After(time.Second):
				t.Fatal("observer did not receive upstream frame")
			}
		})
	}
}
