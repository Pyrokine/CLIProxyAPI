package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesIdentityClaudeRequestCustomHistoryRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}],
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]},
			{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","namespace":"mcp__editor","input":{"patch":"x"}},
			{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_text","text":"applied"}]}
		]
	}`)

	output := convertOpenAIResponsesRequestToClaudeDefault("claude-test", request, false)
	if got := gjson.GetBytes(output, "tools.#").Int(); got != 1 {
		t.Fatalf("tool count = %d, want 1; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "tools.0.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("tool name = %q, want qualified name", got)
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("history tool name = %q, want qualified name", got)
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.input.input.patch").String(); got != "x" {
		t.Fatalf("history custom input = %q, want x; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "messages.1.content.0.content").String(); got != "applied" {
		t.Fatalf("history custom output = %q, want applied", got)
	}
}

func TestResponsesIdentityClaudeStreamCustomItemDeltaDoneRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"mcp__editor__apply_patch","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** patch\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var events [][]byte
	for _, chunk := range chunks {
		events = append(
			events, convertClaudeResponseToOpenAIResponses(
				context.Background(), "claude-test", request, request, chunk, &param,
			)...,
		)
	}

	var added, delta, done, completed gjson.Result
	for _, chunk := range events {
		event, data := parseClaudeResponsesSSEEvent(t, chunk)
		switch event {
		case "response.output_item.added":
			if data.Get("item.type").String() == "custom_tool_call" {
				added = data
			}
		case "response.custom_tool_call_input.delta":
			delta = data
		case "response.output_item.done":
			if data.Get("item.type").String() == "custom_tool_call" {
				done = data
			}
		case "response.completed":
			completed = data
		}
	}

	if got := added.Get("item.namespace").String(); got != "mcp__editor" {
		t.Fatalf("added namespace = %q, want mcp__editor; events=%q", got, events)
	}
	if got := delta.Get("delta").String(); got != "*** patch" {
		t.Fatalf("custom delta = %q, want patch", got)
	}
	if got := done.Get("item.name").String(); got != "apply_patch" {
		t.Fatalf("done name = %q, want apply_patch", got)
	}
	if got := completed.Get("response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("completed type = %q, want custom_tool_call; completed=%s", got, completed.Raw)
	}
	if got := completed.Get("response.output.0.namespace").String(); got != "mcp__editor" {
		t.Fatalf("completed namespace = %q, want mcp__editor", got)
	}
}

func TestResponsesIdentityClaudeNonStreamCustomRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]
	}`)
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"mcp__editor__apply_patch","input":{}}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** patch\"}"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	output := convertClaudeResponseToOpenAIResponsesNonStream(
		context.Background(), "claude-test", request, request, raw, nil,
	)
	if got := gjson.GetBytes(output, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("non-stream type = %q, want custom_tool_call; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "output.0.name").String(); got != "apply_patch" {
		t.Fatalf("non-stream name = %q, want apply_patch", got)
	}
	if got := gjson.GetBytes(output, "output.0.namespace").String(); got != "mcp__editor" {
		t.Fatalf("non-stream namespace = %q, want mcp__editor", got)
	}
	if got := gjson.GetBytes(output, "output.0.input").String(); got != "*** patch" {
		t.Fatalf("non-stream input = %q, want patch", got)
	}
}
