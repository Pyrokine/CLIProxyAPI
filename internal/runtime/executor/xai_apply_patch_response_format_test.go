package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIApplyPatchResponseFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp-1","model":"grok-4.7","status":"in_progress"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":""}}`,
			`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"item-1","arguments":"{\"input\":\"patch 😀\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch 😀\"}"}}`,
			`{"type":"response.completed","response":{"id":"resp-1","model":"grok-4.7","status":"completed","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch 😀\"}"}]}}`,
		} {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	defer server.Close()
	for _, test := range []struct {
		format sdktranslator.Format
		path   string
	}{
		{format: sdktranslator.FormatOpenAIResponse, path: "output.0.input"},
		{format: sdktranslator.FormatOpenAI, path: "choices.0.message.tool_calls.0.function.arguments"},
		{format: sdktranslator.FormatClaude, path: "content.0.input.input"},
	} {
		t.Run(test.format.String(), func(t *testing.T) {
			exec := newXAIExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: test.format}
			response, err := exec.Execute(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			got := gjson.GetBytes(response.Payload, test.path).String()
			if test.format == sdktranslator.FormatOpenAI {
				got = gjson.Get(got, "input").String()
			}
			if got != "patch 😀" {
				t.Fatalf("%s response lost patch: %s", test.format, response.Payload)
			}
			stream, err := exec.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			}
			if !strings.Contains(output.String(), "patch 😀") || !strings.Contains(output.String(), "call-1") {
				t.Fatalf("%s stream lost tool call: %s", test.format, output.String())
			}
		})
	}
}
