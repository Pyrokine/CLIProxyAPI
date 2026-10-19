package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func sendCodexToolCallEvent(t *testing.T, param *any, event string) [][]byte {
	t.Helper()
	return ConvertCodexResponseToOpenAI(
		context.Background(),
		"gpt-5.5",
		nil,
		nil,
		[]byte("data: "+event),
		param,
	)
}

func TestConvertCodexResponseToOpenAICustomToolCallStream(t *testing.T) {
	var param any
	out := sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`,
	)
	if len(out) != 1 {
		t.Fatalf("announcement chunks = %d, want 1", len(out))
	}
	toolCall := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
	if got := toolCall.Get("index").Int(); got != 0 {
		t.Fatalf("announcement index = %d, want 0; chunk=%s", got, out[0])
	}
	if got := toolCall.Get("id").String(); got != "call_apply" {
		t.Fatalf("announcement id = %q, want call_apply; chunk=%s", got, out[0])
	}
	if got := toolCall.Get("function.name").String(); got != "ApplyPatch" {
		t.Fatalf("announcement name = %q, want ApplyPatch; chunk=%s", got, out[0])
	}

	for _, delta := range []string{"first", "second"} {
		out = sendCodexToolCallEvent(
			t, &param,
			`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":"`+delta+`"}`,
		)
		if len(out) != 1 {
			t.Fatalf("delta %q chunks = %d, want 1", delta, len(out))
		}
		if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != delta {
			t.Fatalf("delta arguments = %q, want %q; chunk=%s", got, delta, out[0])
		}
	}

	for _, event := range []string{
		`{"type":"response.custom_tool_call_input.done","item_id":"ctc_1","output_index":0,"input":"firstsecond"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"firstsecond"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"firstsecond"}}`,
	} {
		if out = sendCodexToolCallEvent(t, &param, event); len(out) != 0 {
			t.Fatalf("terminal event emitted %d duplicate chunks for %s", len(out), event)
		}
	}

	out = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	)
	if len(out) != 1 {
		t.Fatalf("completion chunks = %d, want 1", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAIInterleavedToolCallsUseIdentity(t *testing.T) {
	var param any
	out := sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`,
	)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("function index = %d, want 0", got)
	}
	out = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"ctc_2","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`,
	)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("custom index = %d, want 1", got)
	}

	out = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"query\":"}`,
	)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("function delta index = %d, want 0; chunk=%s", got, out[0])
	}
	out = sendCodexToolCallEvent(
		t, &param, `{"type":"response.custom_tool_call_input.done","call_id":"call_apply","input":"patch"}`,
	)
	if len(out) != 1 {
		t.Fatalf("custom done chunks = %d, want 1", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("custom done index = %d, want 1; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "patch" {
		t.Fatalf("custom done arguments = %q, want patch; chunk=%s", got, out[0])
	}

	out = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"query\":\"test\"}"}`,
	)
	if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int() != 0 ||
		gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String() != `"test"}` {
		t.Fatalf("function snapshot tail not delivered: %s", out)
	}
	for _, event := range []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"query\":\"test\"}"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"ctc_2","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"patch"}}`,
	} {
		if out = sendCodexToolCallEvent(t, &param, event); len(out) != 0 {
			t.Fatalf("terminal event emitted %d duplicate chunks for %s", len(out), event)
		}
	}
}

func TestConvertCodexResponseToOpenAIEmptyDeltaUsesDoneFallback(t *testing.T) {
	var param any
	_ = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`,
	)
	out := sendCodexToolCallEvent(
		t, &param, `{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":""}`,
	)
	if len(out) != 0 {
		t.Fatalf("empty delta emitted %d chunks", len(out))
	}
	out = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.custom_tool_call_input.done","item_id":"ctc_1","output_index":0,"input":"full patch"}`,
	)
	if len(out) != 1 {
		t.Fatalf("done fallback chunks = %d, want 1", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "full patch" {
		t.Fatalf("done fallback arguments = %q, want full patch; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAIUnknownExplicitIdentityDoesNotUseCurrentCall(t *testing.T) {
	var param any
	_ = sendCodexToolCallEvent(
		t, &param,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`,
	)
	out := sendCodexToolCallEvent(
		t, &param, `{"type":"response.function_call_arguments.delta","item_id":"missing","delta":"wrong"}`,
	)
	if len(out) != 0 {
		t.Fatalf("unknown explicit item emitted %d chunks", len(out))
	}
	out = sendCodexToolCallEvent(
		t, &param, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"right"}`,
	)
	if len(out) != 1 {
		t.Fatalf("known item chunks = %d, want 1", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "right" {
		t.Fatalf("known item arguments = %q, want right", got)
	}
}

func TestConvertCodexResponseToOpenAIToolCallDoneWithoutAdded(t *testing.T) {
	var param any
	event := `{"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"full patch"}}`
	out := sendCodexToolCallEvent(t, &param, event)
	if len(out) != 1 {
		t.Fatalf("fallback chunks = %d, want 1", len(out))
	}
	toolCall := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
	if got := toolCall.Get("id").String(); got != "call_apply" {
		t.Fatalf("fallback id = %q, want call_apply; chunk=%s", got, out[0])
	}
	if got := toolCall.Get("function.name").String(); got != "ApplyPatch" {
		t.Fatalf("fallback name = %q, want ApplyPatch; chunk=%s", got, out[0])
	}
	if got := toolCall.Get("function.arguments").String(); got != "full patch" {
		t.Fatalf("fallback arguments = %q, want full patch; chunk=%s", got, out[0])
	}
	if out = sendCodexToolCallEvent(t, &param, event); len(out) != 0 {
		t.Fatalf("repeated done emitted %d duplicate chunks", len(out))
	}
}

func TestConvertCodexResponseToOpenAINonStreamCustomToolCall(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"full patch"}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.5", nil, nil, raw, nil)
	toolCall := gjson.GetBytes(out, "choices.0.message.tool_calls.0")
	if got := toolCall.Get("id").String(); got != "call_apply" {
		t.Fatalf("id = %q, want call_apply; response=%s", got, out)
	}
	if got := toolCall.Get("function.name").String(); got != "ApplyPatch" {
		t.Fatalf("name = %q, want ApplyPatch; response=%s", got, out)
	}
	if got := toolCall.Get("function.arguments").String(); got != "full patch" {
		t.Fatalf("arguments = %q, want full patch; response=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; response=%s", got, out)
	}
}
