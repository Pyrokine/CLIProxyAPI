package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gorilla/websocket"
)

func TestTranslatedClaudeInputEstimateReachesWebsocketConsumers(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, explicitOriginal := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/original=%t", provider, explicitOriginal), func(t *testing.T) {
					model := "gpt-5.5"
					cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
					var implementation coreauth.ProviderExecutor = newCodexWebsocketsExecutor(cfg)
					if provider == "xai" {
						model, implementation = "grok-4.3", newXAIWebsocketsExecutor(cfg)
					}
					serverDone := make(chan error, 1)
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, req *http.Request) {
								upgrader := websocket.Upgrader{}
								conn, err := upgrader.Upgrade(w, req, nil)
								if err != nil {
									serverDone <- err
									return
								}
								defer func() { _ = conn.Close() }()
								if err = conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err == nil {
									_, _, err = conn.ReadMessage()
								}
								if err == nil {
									for _, event := range claudeInputEstimateResponseEvents(model) {
										if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
											break
										}
									}
								}
								serverDone <- err
							},
						),
					)
					defer server.Close()
					request, opts := claudeInputEstimateRequest(model, explicitOriginal)
					before, originalBefore := bytes.Clone(request.Payload), bytes.Clone(opts.OriginalRequest)
					credential := &coreauth.Auth{
						ID: t.Name(), Provider: provider, ProxyURL: "none",
						Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL},
					}
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
					registerTestUsagePlugin(t, plugin)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					result, err := implementation.ExecuteStream(ctx, credential, request, opts)
					if err != nil {
						t.Fatal(err)
					}
					assertClaudeInputEstimateDelivery(t, ctx, result, plugin.records, 13)
					select {
					case err := <-serverDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal("synthetic websocket did not finish")
					}
					if !bytes.Equal(request.Payload, before) || !bytes.Equal(opts.OriginalRequest, originalBefore) {
						t.Fatal("caller request changed")
					}
				},
			)
		}
	}
}

func TestTranslatedClaudeInputEstimateReachesAIStudioRelay(t *testing.T) {
	for _, mode := range []string{"stream", "http-response"} {
		for _, explicitOriginal := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/original=%t", mode, explicitOriginal), func(t *testing.T) {
					const authID = "synthetic-claude-estimate"
					connected := make(chan struct{})
					relay := wsrelay.NewManager(
						wsrelay.Options{
							ProviderFactory: func(*http.Request) (string, error) { return authID, nil },
							OnConnected:     func(string) { close(connected) },
						},
					)
					server := httptest.NewServer(relay.Handler())
					defer server.Close()
					defer func() { _ = relay.Stop(context.Background()) }()
					client, response, err := websocket.DefaultDialer.Dial(
						"ws"+strings.TrimPrefix(server.URL, "http")+relay.Path(), nil,
					)
					if response != nil && response.Body != nil {
						defer func() { _ = response.Body.Close() }()
					}
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = client.Close() }()
					serverDone := make(chan error, 1)
					go func() {
						var request struct{ ID string }
						if err := client.ReadJSON(&request); err != nil {
							serverDone <- err
							return
						}
						const body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},` +
							`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,` +
							`"candidatesTokenCount":5,"totalTokenCount":12}}`
						messages := []map[string]any{
							{
								"id": request.ID, "type": wsrelay.MessageTypeHTTPResp,
								"payload": map[string]any{"status": http.StatusOK, "body": body},
							},
						}
						if mode == "stream" {
							messages = []map[string]any{
								{
									"id": request.ID, "type": wsrelay.MessageTypeStreamStart,
									"payload": map[string]any{"status": http.StatusOK},
								},
								{
									"id": request.ID, "type": wsrelay.MessageTypeStreamChunk,
									"payload": map[string]any{"data": "data: " + body + "\n\n"},
								},
								{"id": request.ID, "type": wsrelay.MessageTypeStreamEnd},
							}
						}
						for _, message := range messages {
							if err := client.WriteJSON(message); err != nil {
								serverDone <- err
								return
							}
						}
						serverDone <- nil
					}()
					request, opts := claudeInputEstimateRequest("gemini-2.5-flash", explicitOriginal)
					before, originalBefore := bytes.Clone(request.Payload), bytes.Clone(opts.OriginalRequest)
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
					registerTestUsagePlugin(t, plugin)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					select {
					case <-connected:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					result, err := NewAIStudioExecutor(&config.Config{}, "aistudio", relay).ExecuteStream(
						ctx, &coreauth.Auth{ID: authID}, request, opts,
					)
					if err != nil {
						t.Fatal(err)
					}
					assertClaudeInputEstimateDelivery(t, ctx, result, plugin.records, 13)
					select {
					case err := <-serverDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal("synthetic relay did not finish")
					}
					if !bytes.Equal(request.Payload, before) || !bytes.Equal(opts.OriginalRequest, originalBefore) {
						t.Fatal("caller request changed")
					}
				},
			)
		}
	}
}
