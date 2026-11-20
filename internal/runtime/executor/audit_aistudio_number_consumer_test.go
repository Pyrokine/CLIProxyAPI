package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditAIStudioFinalResponseKeepsStructuredNumbers(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				auth := &coreauth.Auth{ID: "synthetic-number-relay", Provider: "aistudio"}
				connected := make(chan struct{})
				manager := wsrelay.NewManager(
					wsrelay.Options{
						ProviderFactory: func(*http.Request) (
							string,
							error,
						) {
							return auth.ID, nil
						},
						OnConnected: func(string) { close(connected) },
					},
				)
				server := httptest.NewServer(manager.Handler())
				t.Cleanup(server.Close)
				t.Cleanup(func() { _ = manager.Stop(context.Background()) })
				client, response, err := websocket.DefaultDialer.Dial(
					"ws"+strings.TrimPrefix(
						server.URL, "http",
					)+manager.Path(), nil,
				)
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = client.Close() })
				done := make(chan error, 1)
				go func() {
					var envelope json.RawMessage
					if err := client.ReadJSON(&envelope); err != nil {
						done <- err
						return
					}
					body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"n` +
						`ame":"calculate","args":{"positive":9007199254740993,"negative":-90071` +
						`99254740993,"decimal":0.123456789012345678901,"small":42,"text":"90071` +
						`99254740993"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptToke` +
						`nCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
					done <- client.WriteJSON(
						map[string]any{
							"id": gjson.GetBytes(envelope, "id").String(), "type": wsrelay.MessageTypeHTTPResp,
							"payload": map[string]any{"status": http.StatusOK, "body": body},
						},
					)
				}()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				select {
				case <-connected:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				req := cliproxyexecutor.Request{
					Model:   "gemini-2.5-flash",
					Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"calculate"}]}]}`),
				}
				opts := cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini,
				}
				exec := NewAIStudioExecutor(&config.Config{}, "aistudio", manager)
				var output []byte
				if stream {
					result, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
				} else {
					result, err := exec.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					output = result.Payload
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("synthetic relay did not complete")
				}
				for _, literal := range []string{
					`"positive": 9007199254740993`, `"negative": -9007199254740993`,
					`"decimal": 0.123456789012345678901`, `"small": 42`, `"text": "9007199254740993"`,
				} {
					if !strings.Contains(string(output), literal) {
						t.Fatalf("final structured literal %s changed: %s", literal, output)
					}
				}
			},
		)
	}
}
