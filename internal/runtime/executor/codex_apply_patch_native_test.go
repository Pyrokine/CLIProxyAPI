package executor

import (
	"context"
	"fmt"
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

func TestNativeCodexApplyPatchRoundTrip(t *testing.T) {
	const patch = "*** Begin Patch\n*** Update File: file.txt\n@@\n-old\n+new\n*** End Patch"
	const tool = `{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /./"}}`
	requests := make(chan []byte, 3)
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		call++
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 2 {
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"patched"}]}]}}`+"\n\n")
			return
		}
		item := fmt.Sprintf(`{"type":"custom_tool_call","id":"item-1","call_id":"call-patch","name":"apply_patch","input":%q}`, patch)
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp-1","model":"gpt-6-astra"}}`+"\n\n")
		if call == 3 {
			_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"item-1","call_id":"call-patch","name":"apply_patch","input":""}}`+"\n\n")
			_, _ = io.WriteString(w, fmt.Sprintf("data: {\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"item_id\":\"item-1\",\"delta\":%q}\n\n", patch[:18]))
			_, _ = io.WriteString(w, fmt.Sprintf("data: {\"type\":\"response.custom_tool_call_input.done\",\"output_index\":0,\"item_id\":\"item-1\",\"input\":%q}\n\n", patch))
		}
		_, _ = io.WriteString(w, fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item))
		_, _ = io.WriteString(w, fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[%s]}}\n\n", item))
	}))
	defer server.Close()

	exec := newCodexExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"}},
	}
	payload := fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_text","text":"Apply the patch"}]}],"tools":[%s],"tool_choice":{"type":"custom","name":"apply_patch"}}`, tool)
	response, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(payload)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if got := gjson.GetBytes(first, "tools.0"); got.Raw != tool {
		t.Fatalf("freeform tool changed: %s", first)
	}
	if got := gjson.GetBytes(first, "tool_choice.name").String(); got != "apply_patch" {
		t.Fatalf("tool choice = %q, want apply_patch; request=%s", got, first)
	}
	if got := gjson.GetBytes(response.Payload, "output.0.input").String(); got != patch {
		t.Fatalf("non-stream patch = %q, want %q; response=%s", got, patch, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "output.0.call_id").String(); got != "call-patch" {
		t.Fatalf("call ID = %q, want call-patch", got)
	}

	followUp := fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"type":"custom_tool_call","call_id":"call-patch","name":"apply_patch","input":%q},{"type":"custom_tool_call_output","call_id":"call-patch","output":"patched"}],"tools":[%s]}`, patch, tool)
	response, err = exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(followUp)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	second := <-requests
	if got := gjson.GetBytes(second, "input.0.input").String(); got != patch {
		t.Fatalf("replayed patch = %q, want %q; request=%s", got, patch, second)
	}
	if got := gjson.GetBytes(second, "input.1.call_id").String(); got != "call-patch" {
		t.Fatalf("output call ID = %q, want call-patch", got)
	}
	if got := gjson.GetBytes(response.Payload, "output.0.content.0.text").String(); got != "patched" {
		t.Fatalf("follow-up response = %s", response.Payload)
	}

	stream, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(payload)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var streamedPatch, terminalPatch, deltaPatch, donePatch string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		data := jsonPayload(chunk.Payload)
		switch gjson.GetBytes(data, "type").String() {
		case "response.custom_tool_call_input.delta":
			deltaPatch = gjson.GetBytes(data, "delta").String()
		case "response.custom_tool_call_input.done":
			donePatch = gjson.GetBytes(data, "input").String()
		case "response.output_item.done":
			streamedPatch = gjson.GetBytes(data, "item.input").String()
		case "response.completed":
			terminalPatch = gjson.GetBytes(data, "response.output.0.input").String()
		}
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(nil)
		}
	}
	<-requests
	if deltaPatch != patch[:18] || donePatch != patch || streamedPatch != patch || terminalPatch != patch {
		t.Fatalf("stream delta=%q done=%q item=%q terminal=%q, want %q", deltaPatch, donePatch, streamedPatch, terminalPatch, patch)
	}
}

func TestNativeCodexApplyPatchUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_tool_schema","message":"patch tool is unsupported"}}`)
	}))
	defer server.Close()

	response, err := newCodexExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll}).Execute(
		context.Background(),
		&cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"apply patch","tools":[{"type":"custom","name":"apply_patch"}]}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid_tool_schema") || len(response.Payload) != 0 {
		t.Fatalf("upstream failure: response=%s error=%v", response.Payload, err)
	}
}
