package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

func TestAIStudioStreamTerminatesOrReportsLateHTTPError(t *testing.T) {
	for _, tc := range []struct {
		name, terminalType string
		status             int
	}{
		{name: "clean end without finish reason", terminalType: wsrelay.MessageTypeStreamEnd},
		{name: "http error after start", terminalType: wsrelay.MessageTypeHTTPResp, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const authID = "aistudio-stream-termination"
			manager := wsrelay.NewManager(wsrelay.Options{ProviderFactory: func(*http.Request) (string, error) {
				return authID, nil
			}})
			server := httptest.NewServer(manager.Handler())
			t.Cleanup(server.Close)
			t.Cleanup(func() { _ = manager.Stop(context.Background()) })
			conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+manager.Path(), nil)
			if resp != nil && resp.Body != nil {
				t.Cleanup(func() { _ = resp.Body.Close() })
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			clientDone := make(chan error, 1)
			go func() {
				var request struct {
					ID string `json:"id"`
				}
				if errRead := conn.ReadJSON(&request); errRead != nil {
					clientDone <- errRead
					return
				}
				if errWrite := conn.WriteJSON(map[string]any{
					"id": request.ID, "type": wsrelay.MessageTypeStreamStart,
					"payload": map[string]any{"status": http.StatusOK},
				}); errWrite != nil {
					clientDone <- errWrite
					return
				}
				if errWrite := conn.WriteJSON(map[string]any{
					"id": request.ID, "type": wsrelay.MessageTypeStreamChunk,
					"payload": map[string]any{"data": `data: {"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}` + "\n\n"},
				}); errWrite != nil {
					clientDone <- errWrite
					return
				}
				terminal := map[string]any{"id": request.ID, "type": tc.terminalType}
				if tc.status != 0 {
					terminal["payload"] = map[string]any{"status": tc.status, "body": `{"error":"capacity unavailable"}`}
				}
				clientDone <- conn.WriteJSON(terminal)
			}()

			exec := NewAIStudioExecutor(nil, "aistudio", manager)
			stream, err := exec.ExecuteStream(context.Background(), &cliproxyauth.Auth{ID: authID},
				cliproxyexecutor.Request{Model: "gemini-test", Payload: []byte(`{"model":"gemini-test","messages":[{"role":"user","content":"hello"}]}`)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
			)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var streamErr error
			for chunk := range stream.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
			}
			if errClient := <-clientDone; errClient != nil {
				t.Fatal(errClient)
			}
			if tc.status != 0 {
				if status, ok := streamErr.(interface{ StatusCode() int }); !ok || status.StatusCode() != tc.status ||
					!strings.Contains(streamErr.Error(), "capacity unavailable") || strings.Contains(output.String(), `"type":"message_stop"`) {
					t.Fatalf("HTTP %d stream error = %v, output = %s", tc.status, streamErr, output.String())
				}
				return
			}
			if streamErr != nil || !strings.Contains(output.String(), `"stop_reason":"end_turn"`) ||
				!strings.Contains(output.String(), `"type":"message_stop"`) {
				t.Fatalf("clean end stream error = %v, output = %s", streamErr, output.String())
			}
		})
	}
}
