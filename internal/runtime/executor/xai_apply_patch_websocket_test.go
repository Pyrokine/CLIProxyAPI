package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestXAIApplyPatchWebsocket(t *testing.T) {
	for _, arguments := range []string{`{"input":"patch 😀"}`, `{"input":"\ud800"}`, `{"input":"partial`} {
		t.Run(arguments, func(t *testing.T) {
			requests := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, body, errRead := conn.ReadMessage(); errRead != nil {
					t.Errorf("read: %v", errRead)
					return
				} else {
					requests <- body
				}
				item := fmt.Sprintf(`{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":%q}`, arguments)
				for _, event := range []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":""}}`,
					fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"item-1","arguments":%q}`, arguments),
					fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, item),
					fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[%s]}}`, item),
				} {
					if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			exec := newXAIWebsocketsExecutor(&config.Config{})
			sessionID := t.Name()
			t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			ctx, cancel := context.WithTimeout(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), 5*time.Second)
			defer cancel()
			auth := &cliproxyauth.Auth{ID: sessionID, Provider: "xai", Attributes: map[string]string{"base_url": server.URL, "websockets": "true"}, Metadata: map[string]any{"access_token": "test"}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			stream, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var deltas strings.Builder
			var done, terminal string
			var streamErr error
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
				data := jsonPayload(chunk.Payload)
				switch gjson.GetBytes(data, "type").String() {
				case "response.custom_tool_call_input.delta":
					deltas.WriteString(gjson.GetBytes(data, "delta").String())
				case "response.custom_tool_call_input.done":
					done = gjson.GetBytes(data, "input").String()
				case "response.completed":
					terminal = gjson.GetBytes(data, "response.output.0.input").String()
				case "response.function_call_arguments.delta", "response.function_call_arguments.done":
					t.Fatalf("function wrapper leaked: %s", data)
				}
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			}
			request := <-requests
			if gjson.GetBytes(request, "type").String() != "response.create" || gjson.GetBytes(request, "tools.0.type").String() != "function" {
				t.Fatalf("websocket function declaration: %s", request)
			}
			if arguments == `{"input":"patch 😀"}` {
				if streamErr != nil || deltas.String() != "patch 😀" || done != "patch 😀" || terminal != "patch 😀" {
					t.Fatalf("delta=%q done=%q terminal=%q error=%v", deltas.String(), done, terminal, streamErr)
				}
			} else if streamErr == nil || terminal != "" || !strings.Contains(streamErr.Error(), "invalid apply_patch") {
				t.Fatalf("invalid input completed: terminal=%q error=%v", terminal, streamErr)
			}
		})
	}
}

func TestXAIApplyPatchSourceFormats(t *testing.T) {
	for _, test := range []struct {
		name    string
		format  sdktranslator.Format
		payload string
		patch   bool
	}{
		{name: "Responses custom", format: sdktranslator.FormatOpenAIResponse, payload: `{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`, patch: true},
		{name: "Codex custom", format: sdktranslator.FormatCodex, payload: `{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`, patch: true},
		{name: "Responses function", format: sdktranslator.FormatOpenAIResponse, payload: `{"input":"patch","tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"patch":{"type":"string"}}}}]}`},
		{name: "Chat function", format: sdktranslator.FormatOpenAI, payload: `{"messages":[{"role":"user","content":"patch"}],"tools":[{"type":"function","function":{"name":"apply_patch","parameters":{"type":"object","properties":{"patch":{"type":"string"}}}}}]}`},
		{name: "Messages function", format: sdktranslator.FormatClaude, payload: `{"messages":[{"role":"user","content":"patch"}],"tools":[{"name":"apply_patch","input_schema":{"type":"object","properties":{"patch":{"type":"string"}}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(test.payload)}, cliproxyexecutor.Options{SourceFormat: test.format, ResponseFormat: sdktranslator.FormatOpenAIResponse}, false)
			if err != nil {
				t.Fatal(err)
			}
			if (prepared.newResponseNormalizer().patch != nil) != test.patch {
				t.Fatalf("patch provenance = %t, want %t; body=%s", prepared.newResponseNormalizer().patch != nil, test.patch, prepared.body)
			}
			if !test.patch && !gjson.GetBytes(prepared.body, "tools.0.parameters.properties.patch").Exists() {
				t.Fatalf("ordinary function schema changed: %s", prepared.body)
			}
		})
	}
}
