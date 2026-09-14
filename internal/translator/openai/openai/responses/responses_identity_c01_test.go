package responses

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesIdentityRequestAdditionalToolsStructuredOutputAndBatching(t *testing.T) {
	request := []byte(`{
		"tools":[
			{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]},
			{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}
		],
		"tool_choice":{"type":"custom","name":"apply_patch","namespace":"mcp__editor"},
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]},
			{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","namespace":"mcp__editor","input":"*** patch"},
			{"type":"function_call","call_id":"call_inspect","name":"inspect","arguments":"{\"path\":\"a.go\"}"},
			{"type":"custom_tool_call_output","call_id":"call_patch","output":[{"type":"input_text","text":"applied"}]},
			{"type":"function_call_output","call_id":"call_inspect","output":[
				{"type":"input_text","text":"preview"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"low"}
			]},
			{"type":"message","role":"user","content":"continue"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", request, false)
	if got := gjson.GetBytes(output, "tools.#").Int(); got != 2 {
		t.Fatalf("tool count = %d, want 2; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "tools.0.function.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("custom tool name = %q, want qualified name", got)
	}
	if got := gjson.GetBytes(output, "tool_choice.function.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("tool choice name = %q, want qualified name; output=%s", got, output)
	}

	messages := gjson.GetBytes(output, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want 4; output=%s", len(messages), output)
	}
	assistant := messages[0]
	if got := assistant.Get("tool_calls.#").Int(); got != 2 {
		t.Fatalf("batched tool call count = %d, want 2; output=%s", got, output)
	}
	if got := assistant.Get("tool_calls.0.function.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("custom history name = %q, want qualified name", got)
	}
	if got := gjson.Parse(assistant.Get("tool_calls.0.function.arguments").String()).Get("input").String(); got != "*** patch" {
		t.Fatalf("custom history input = %q, want patch", got)
	}
	if got := messages[1].Get("content").String(); got != "applied" {
		t.Fatalf("custom output = %q, want applied", got)
	}
	if got := messages[2].Get("content.0.type").String(); got != "text" {
		t.Fatalf("structured output text type = %q, want text", got)
	}
	if got := messages[2].Get("content.1.image_url.url").String(); got != "data:image/png;base64,AAAA" {
		t.Fatalf("structured output image URL = %q", got)
	}
	if got := messages[3].Get("content").String(); got != "continue" {
		t.Fatalf("deferred message = %q, want continue", got)
	}
}

func TestResponsesIdentityOpenAIStreamCustomItemDeltaDoneRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]
	}`)
	chunks := [][]byte{
		[]byte(`{"id":"chat_1","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"mcp__editor__apply_patch","arguments":"{\"input\":\"*** patch\"}"}}]}}]}`),
		[]byte(`{"id":"chat_1","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		[]byte(`{"id":"chat_1","created":1,"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		[]byte(`[DONE]`),
	}

	var param any
	var events [][]byte
	for _, chunk := range chunks {
		events = append(
			events, convertOpenAIChatCompletionsResponseToOpenAIResponses(
				context.Background(), "gpt-test", request, request, chunk, &param,
			)...,
		)
	}

	var added, delta, done, completed gjson.Result
	for _, chunk := range events {
		event, data := openAIResponsesSSEData(t, chunk)
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

	for label, item := range map[string]gjson.Result{
		"added": added, "delta": delta, "done": done, "completed": completed,
	} {
		if !item.Exists() {
			t.Fatalf("missing %s event; events=%q", label, events)
		}
	}
	if got := added.Get("item.name").String(); got != "apply_patch" {
		t.Fatalf("added name = %q, want apply_patch", got)
	}
	if got := added.Get("item.namespace").String(); got != "mcp__editor" {
		t.Fatalf("added namespace = %q, want mcp__editor", got)
	}
	if got := delta.Get("delta").String(); got != "*** patch" {
		t.Fatalf("custom delta = %q, want patch", got)
	}
	if got := done.Get("item.input").String(); got != "*** patch" {
		t.Fatalf("done input = %q, want patch", got)
	}
	if got := completed.Get("response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("completed type = %q, want custom_tool_call; completed=%s", got, completed.Raw)
	}
	if got := completed.Get("response.output.0.namespace").String(); got != "mcp__editor" {
		t.Fatalf("completed namespace = %q, want mcp__editor", got)
	}
	if got := completed.Get("response.usage.input_tokens").Int(); got != 11 {
		t.Fatalf("completed input tokens = %d, want 11", got)
	}
	if got := completed.Get("response.usage.output_tokens").Int(); got != 7 {
		t.Fatalf("completed output tokens = %d, want 7", got)
	}
}
