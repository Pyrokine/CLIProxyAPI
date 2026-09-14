package openai

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRepairResponsesWebsocketToolCallsInsertsCachedOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)
	cache.record(
		"session-1", "call-1", json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"ok"}`),
	)

	payload := []byte(`{"input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"call-1","name":"tool"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCache(cache, "session-1", payload)
	input := gjson.GetBytes(updated, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input len = %d, want 3", len(input))
	}
	if input[1].Get("type").String() != "function_call" || input[2].Get("type").String() != "function_call_output" {
		t.Fatalf("unexpected repaired order: %s", gjson.GetBytes(updated, "input").Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsOrphanFunctionCall(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)

	payload := []byte(`{"input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"call-1","name":"tool"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCache(cache, "session-1", payload)
	input := gjson.GetBytes(updated, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input len = %d, want 1", len(input))
	}
	if input[0].Get("type").String() != "message" {
		t.Fatalf("unexpected remaining item: %s", input[0].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedCallForOrphanOutput(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(0, 8)
	callCache := newWebsocketToolOutputCache(0, 8)
	callCache.record(
		"session-1", "call-1", json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"tool"}`),
	)

	payload := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, "session-1", payload)
	input := gjson.GetBytes(updated, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input len = %d, want 2", len(input))
	}
	if input[0].Get("type").String() != "function_call" || input[1].Get("type").String() != "function_call_output" {
		t.Fatalf("unexpected repaired order: %s", gjson.GetBytes(updated, "input").Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseIDOrphanOutputWithCachedCall(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(0, 8)
	callCache := newWebsocketToolOutputCache(0, 8)
	callCache.record(
		"session-1", "call-1", json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"tool"}`),
	)

	payload := []byte(`{"previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, "session-1", payload)
	input := gjson.GetBytes(updated, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input len = %d, want 2", len(input))
	}
	if input[0].Get("type").String() != "function_call" || input[1].Get("type").String() != "function_call_output" {
		t.Fatalf("unexpected repaired order: %s", gjson.GetBytes(updated, "input").Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsOrphanOutputWithoutCachedCall(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(0, 8)
	callCache := newWebsocketToolOutputCache(0, 8)

	payload := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, "session-1", payload)
	if input := gjson.GetBytes(updated, "input").Array(); len(input) != 0 {
		t.Fatalf("input len = %d, want 0", len(input))
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedCustomToolOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)
	cache.record(
		"session-1", "call-1", json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}`),
	)

	payload := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call-1","name":"tool"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCache(cache, "session-1", payload)
	input := gjson.GetBytes(updated, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input len = %d, want 2", len(input))
	}
	if input[0].Get("type").String() != "custom_tool_call" || input[1].Get("type").String() != "custom_tool_call_output" {
		t.Fatalf("unexpected repaired order: %s", gjson.GetBytes(updated, "input").Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsOrphanCustomToolCall(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)

	payload := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call-1","name":"tool"}]}`)
	updated := repairResponsesWebsocketToolCallsWithCache(cache, "session-1", payload)
	if input := gjson.GetBytes(updated, "input").Array(); len(input) != 0 {
		t.Fatalf("input len = %d, want 0", len(input))
	}
}

func TestRecordResponsesWebsocketToolCallsFromPayloadWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)
	payload := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call-1","name":"tool","arguments":""}}`)

	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, "session-1", payload)
	cached, ok := cache.get("session-1", "call-1")
	if !ok {
		t.Fatalf("expected cached tool call")
	}
	if gjson.GetBytes(cached, "type").String() != "function_call" {
		t.Fatalf("unexpected cached item: %s", string(cached))
	}
}

func TestRecordResponsesWebsocketCustomToolCallsFromCompletedPayloadWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)
	payload := []byte(`{"type":"response.completed","response":{"output":[{"type":"custom_tool_call","call_id":"call-1","name":"tool","input":""}]}}`)

	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, "session-1", payload)
	cached, ok := cache.get("session-1", "call-1")
	if !ok {
		t.Fatalf("expected cached custom tool call")
	}
	if gjson.GetBytes(cached, "type").String() != "custom_tool_call" {
		t.Fatalf("unexpected cached item: %s", string(cached))
	}
}

func TestRecordResponsesWebsocketCustomToolCallsFromOutputItemDoneWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)
	payload := []byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call-1","name":"tool","input":""}}`)

	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, "session-1", payload)
	cached, ok := cache.get("session-1", "call-1")
	if !ok {
		t.Fatalf("expected cached custom tool call")
	}
	if gjson.GetBytes(cached, "type").String() != "custom_tool_call" {
		t.Fatalf("unexpected cached item: %s", string(cached))
	}
}

func TestIsCompleteResponsesWebsocketToolCallRequiresStringFields(t *testing.T) {
	tests := []struct {
		name string
		item string
		want bool
	}{
		{name: "numeric call id", item: `{"type":"function_call","call_id":123,"name":"exec","arguments":"{}"}`},
		{name: "boolean name", item: `{"type":"function_call","call_id":"call-1","name":true,"arguments":"{}"}`},
		{name: "numeric arguments", item: `{"type":"function_call","call_id":"call-1","name":"exec","arguments":123}`},
		{name: "object custom input", item: `{"type":"custom_tool_call","call_id":"call-1","name":"exec","input":{}}`},
		{
			name: "valid function call",
			item: `{"type":"function_call","call_id":"call-1","name":"exec","arguments":""}`, want: true,
		},
		{
			name: "valid custom tool call",
			item: `{"type":"custom_tool_call","call_id":"call-1","name":"exec","input":""}`, want: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := isCompleteResponsesWebsocketToolCall(gjson.Parse(tt.item)); got != tt.want {
					t.Fatalf("isCompleteResponsesWebsocketToolCall() = %t, want %t", got, tt.want)
				}
			},
		)
	}
}

func TestRecordResponsesWebsocketToolCallsIgnoresIncompleteCall(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 8)
	pending := make(map[string]struct{})
	payload := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call-1","name":"exec"}}`)

	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, "session-1", payload)
	recordPendingToolCallIDsFromPayload(pending, payload)

	if cached, ok := cache.get("session-1", "call-1"); ok {
		t.Fatalf("incomplete tool call was cached: %s", cached)
	}
	if len(pending) != 0 {
		t.Fatalf("incomplete tool call was recorded as pending: %v", pending)
	}
}
