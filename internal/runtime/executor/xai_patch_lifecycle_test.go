package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIApplyPatchNamedLateIdentityAndSparseTerminal(t *testing.T) {
	for _, tc := range []struct {
		streaming bool
		lateName  bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		streaming := tc.streaming
		t.Run(fmt.Sprintf("stream=%t/name-late=%t", streaming, tc.lateName), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				added := `{"type":"response.output_item.added","response_id":"resp-1","source_index":9,"opaque":{"marker":"synthetic"},"output_index":2,"item":{"type":"function_call","name":"apply_patch","arguments":""}}`
				if tc.lateName {
					added = strings.Replace(added, `"name":"apply_patch",`, "", 1)
				}
				for _, event := range []string{
					added,
					`{"type":"response.function_call_arguments.delta","response_id":"resp-1","source_index":9,"opaque":{"marker":"synthetic"},"output_index":2,"delta":"{\"input\":\"p"}`,
					`{"type":"response.function_call_arguments.delta","response_id":"resp-1","source_index":9,"opaque":{"marker":"synthetic"},"output_index":2,"delta":"q\"}"}`,
					`{"type":"response.function_call_arguments.done","response_id":"resp-1","output_index":2,"arguments":"{\"input\":\"pq\"}"}`,
					`{"type":"response.output_item.done","response_id":"resp-1","source_index":9,"opaque":{"marker":"synthetic"},"output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"pq\"}"}}`,
					`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}]}}`,
				} {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exec := newXAIExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
			if !streaming {
				response, err := exec.Execute(ctx, auth, req, opts)
				if err != nil || gjson.GetBytes(response.Payload, "output.0.input").String() != "pq" || gjson.GetBytes(response.Payload, "output.0.call_id").String() != "call-1" {
					t.Fatalf("late identity response=%s error=%v", response.Payload, err)
				}
				return
			}
			stream, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			counts := make(map[string]int)
			var fragments []string
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				root := gjson.ParseBytes(jsonPayload(chunk.Payload))
				kind := root.Get("type").String()
				counts[kind]++
				if kind == "response.output_item.added" || kind == "response.output_item.done" || kind == "response.custom_tool_call_input.delta" {
					if root.Get("response_id").String() != "resp-1" || root.Get("source_index").Int() != 9 || root.Get("opaque.marker").String() != "synthetic" {
						t.Fatalf("source event metadata was lost: %s", chunk.Payload)
					}
					if root.Get("output_index").Int() != 2 {
						t.Fatalf("sparse terminal changed output index: %s", chunk.Payload)
					}
				}
				if kind == "response.custom_tool_call_input.delta" {
					fragments = append(fragments, root.Get("delta").String())
					if root.Get("item_id").String() != "item-1" || root.Get("call_id").String() != "call-1" {
						t.Fatalf("unresolved identity was delivered: %s", chunk.Payload)
					}
				}
				if kind == "response.completed" && root.Get("response.output.0.input").String() != "pq" {
					t.Fatalf("sparse completion lost patch: %s", chunk.Payload)
				}
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			}
			if strings.Join(fragments, "|") != "p|q" {
				t.Fatalf("real pending fragments=%q", fragments)
			}
			for _, kind := range []string{"response.output_item.added", "response.custom_tool_call_input.done", "response.output_item.done", "response.completed"} {
				if counts[kind] != 1 {
					t.Fatalf("lifecycle counts=%v", counts)
				}
			}
		})
	}
}

func TestXAIApplyPatchCompletedInputDoesNotReplaceResponseCompletion(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}`+"\n\n")
			}))
			t.Cleanup(server.Close)
			exec := newXAIExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if !streaming {
				response, err := exec.Execute(ctx, auth, req, opts)
				if err == nil || len(response.Payload) != 0 {
					t.Fatalf("EOF fabricated response completion: response=%s error=%v", response.Payload, err)
				}
				return
			}
			stream, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			for chunk := range stream.Chunks {
				failed = failed || chunk.Err != nil
				if chunk.Terminal || gjson.GetBytes(jsonPayload(chunk.Payload), "type").String() == "response.completed" {
					t.Fatal("EOF fabricated response completion")
				}
			}
			if !failed {
				t.Fatal("EOF without response completion did not fail")
			}
		})
	}
}
