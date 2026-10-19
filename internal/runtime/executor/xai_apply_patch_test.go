package executor

import (
	"context"
	"encoding/json"
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

func TestXAIApplyPatchHTTPRoundTrip(t *testing.T) {
	const patch = "*** Begin Patch\n*** Update File: 中文.txt\n@@\n-旧\n+新 😀\n*** End Patch"
	const tool = `{"type":"custom","name":"apply_patch","description":"This is a FREEFORM tool, so do not wrap the patch in JSON.","format":{"type":"grammar","syntax":"lark","definition":"start: /./"}}`
	arguments, err := json.Marshal(map[string]string{"input": patch})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan []byte, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request: %v", errRead)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		if gjson.GetBytes(body, "input.0.type").String() == "function_call" {
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"patched"}]}]}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-patch","name":"apply_patch","arguments":""}}`+"\n\n")
		for _, fragment := range strings.Split(string(arguments), "") {
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"item-1\",\"delta\":%q}\n\n", fragment)
		}
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"item_id\":\"item-1\",\"arguments\":%q}\n\n", arguments)
		item := fmt.Sprintf(`{"type":"function_call","id":"item-1","call_id":"call-patch","name":"apply_patch","arguments":%q}`, arguments)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[%s]}}\n\n", item)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
	payload := fmt.Sprintf(`{"model":"grok-4.7","input":[{"role":"user","content":"patch"}],"tools":[%s],"tool_choice":{"type":"custom","name":"apply_patch"}}`, tool)
	req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(payload)}
	response, err := exec.Execute(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if gjson.GetBytes(first, "tools.0.type").String() != "function" || gjson.GetBytes(first, "tools.0.parameters.properties.input.type").String() != "string" || gjson.GetBytes(first, "tools.0.format").Exists() {
		t.Fatalf("invalid function declaration: %s", first)
	}
	if strings.Contains(gjson.GetBytes(first, "tools.0.description").String(), "do not wrap the patch in JSON") || gjson.GetBytes(first, "tool_choice.type").String() != "function" {
		t.Fatalf("contradictory declaration or choice: %s", first)
	}
	if gjson.GetBytes(response.Payload, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(response.Payload, "output.0.input").String() != patch {
		t.Fatalf("freeform response: %s", response.Payload)
	}

	followUp := fmt.Sprintf(`{"model":"grok-4.7","input":[{"type":"custom_tool_call","call_id":"call-patch","name":"apply_patch","input":%q},{"type":"custom_tool_call_output","call_id":"call-patch","output":"patched"}],"tools":[%s]}`, patch, tool)
	response, err = exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: req.Model, Payload: []byte(followUp)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	second := <-requests
	if gjson.GetBytes(second, "input.0.arguments").String() != string(arguments) || gjson.GetBytes(second, "input.1.type").String() != "function_call_output" || gjson.GetBytes(second, "input.1.call_id").String() != "call-patch" {
		t.Fatalf("freeform history: %s", second)
	}
	if gjson.GetBytes(response.Payload, "output.0.content.0.text").String() != "patched" {
		t.Fatalf("history response: %s", response.Payload)
	}

	stream, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var done, itemDone, terminal string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		data := jsonPayload(chunk.Payload)
		switch gjson.GetBytes(data, "type").String() {
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			t.Fatalf("function wrapper leaked downstream: %s", data)
		case "response.custom_tool_call_input.delta":
			deltas.WriteString(gjson.GetBytes(data, "delta").String())
		case "response.custom_tool_call_input.done":
			done = gjson.GetBytes(data, "input").String()
		case "response.output_item.done":
			itemDone = gjson.GetBytes(data, "item.input").String()
		case "response.completed":
			terminal = gjson.GetBytes(data, "response.output.0.input").String()
		}
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(nil)
		}
	}
	<-requests
	if deltas.String() != patch || done != patch || itemDone != patch || terminal != patch {
		t.Fatalf("delta=%q done=%q item=%q terminal=%q", deltas.String(), done, itemDone, terminal)
	}
}

func TestXAIApplyPatchInvalidUpstream(t *testing.T) {
	for _, arguments := range []string{`{"input":1}`, `{"input":"x","extra":0}`, `{"input":"\ud800"}`, `{"input":"partial`, `{"input":"x","input":"y"}`} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", arguments, streaming), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					item := fmt.Sprintf(`{"type":"function_call","id":"item-1","call_id":"call-patch","name":"apply_patch","arguments":%q}`, arguments)
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[%s]}}\n\n", item)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				exec := newXAIExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
				if !streaming {
					response, err := exec.Execute(ctx, auth, req, opts)
					if err == nil || !strings.Contains(err.Error(), "invalid apply_patch") || len(response.Payload) != 0 || strings.Contains(err.Error(), arguments) {
						t.Fatalf("failure response=%s error=%v", response.Payload, err)
					}
					return
				}
				stream, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var streamErr error
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
					if gjson.GetBytes(jsonPayload(chunk.Payload), "type").String() == "response.completed" {
						t.Fatal("invalid input produced success")
					}
				}
				if streamErr == nil || !strings.Contains(streamErr.Error(), "invalid apply_patch") {
					t.Fatalf("missing patch failure: %v", streamErr)
				}
			})
		}
	}
}

func TestXAIApplyPatchFoldedNamespace(t *testing.T) {
	tools := make([]map[string]any, 0, xaiMaxTools+1)
	tools = append(tools, map[string]any{"type": "custom", "name": "apply_patch", "description": "patch"})
	for index := 0; index < xaiMaxTools; index++ {
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("tool_%d", index), "parameters": map[string]any{"type": "object"}})
	}
	payload, err := json.Marshal(map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "editor", "tools": tools}}, "input": "patch"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(cliproxyexecutor.Request{Model: "grok-4.7", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(prepared.body, "tools.0.name").String() != "editor" {
		t.Fatalf("namespace was not folded: %s", prepared.body)
	}
	normalizer := prepared.newResponseNormalizer()
	if events := normalizer.normalize([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item-1","call_id":"call-patch","type":"function_call","name":"editor","arguments":""}}`)); len(events) != 0 {
		t.Fatalf("dispatcher added leaked: %q", events)
	}
	if events := normalizer.normalize([]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"item-1","delta":"{\"name\":\"apply_patch\","}`)); len(events) != 0 {
		t.Fatalf("dispatcher delta leaked: %q", events)
	}
	const arguments = `{"name":"apply_patch","arguments":{"input":"*** Begin Patch\n*** End Patch"}}`
	events := normalizer.normalize([]byte(fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"item-1","arguments":%q}`, arguments)))
	var added, inputDone bool
	for _, event := range events {
		if gjson.GetBytes(event, "type").String() == "response.output_item.added" {
			added = gjson.GetBytes(event, "item.type").String() == "custom_tool_call" && gjson.GetBytes(event, "item.name").String() == "apply_patch" && gjson.GetBytes(event, "item.namespace").String() == "editor"
		}
		if gjson.GetBytes(event, "type").String() == "response.custom_tool_call_input.done" {
			inputDone = gjson.GetBytes(event, "input").String() == "*** Begin Patch\n*** End Patch"
		}
	}
	if !added || !inputDone {
		t.Fatalf("namespace identity or input missing: %q", events)
	}
	events = normalizer.normalize([]byte(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"item-1","call_id":"call-patch","name":"editor","arguments":%q}]}}`, arguments)))
	if len(events) != 2 || gjson.GetBytes(events[0], "type").String() != "response.output_item.done" || gjson.GetBytes(events[0], "item.input").String() != "*** Begin Patch\n*** End Patch" || gjson.GetBytes(events[1], "type").String() != "response.completed" || gjson.GetBytes(events[1], "response.output.0.namespace").String() != "editor" || gjson.GetBytes(events[1], "response.output.0.type").String() != "custom_tool_call" {
		t.Fatalf("folded terminal response: %q", events)
	}
}
