package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAIIncompleteTerminal(t *testing.T) {
	terminal := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","model":"gpt-5.5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	var param any
	streamOut := ConvertCodexResponseToOpenAI(
		context.Background(), "gpt-5.5", nil, nil, append([]byte("data: "), terminal...), &param,
	)
	if len(streamOut) != 1 {
		t.Fatalf("stream output count = %d, want 1", len(streamOut))
	}
	if got := gjson.GetBytes(streamOut[0], "choices.0.finish_reason").String(); got != "length" {
		t.Fatalf("stream finish_reason = %q, want length. Output: %s", got, streamOut[0])
	}
	if got := gjson.GetBytes(streamOut[0], "choices.0.native_finish_reason").String(); got != "max_output_tokens" {
		t.Fatalf("stream native_finish_reason = %q, want max_output_tokens. Output: %s", got, streamOut[0])
	}

	nonStreamOut := ConvertCodexResponseToOpenAINonStream(
		context.Background(), "gpt-5.5", nil, nil, terminal, nil,
	)
	if got := gjson.GetBytes(nonStreamOut, "choices.0.finish_reason").String(); got != "length" {
		t.Fatalf("non-stream finish_reason = %q, want length. Output: %s", got, nonStreamOut)
	}
}

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(
		ctx, modelName, nil, nil,
		[]byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`),
		&param,
	)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(
		ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param,
	)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(
		ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param,
	)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamMultiMessageEmptyTrailingKeepsContent(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAINonStream(
		ctx,
		"gpt-5.3-codex",
		nil,
		nil,
		[]byte(`{
			"type":"response.completed",
			"response":{
				"id":"resp_123",
				"created_at":1700000000,
				"model":"gpt-5.3-codex",
				"status":"completed",
				"output":[
					{"type":"reasoning","summary":[{"type":"summary_text","text":"think one"}]},
					{"type":"reasoning","summary":[{"type":"summary_text","text":" and two"}]},
					{"type":"message","content":[{"type":"output_text","text":"answer one"}]},
					{"type":"message","content":[{"type":"output_text","text":" and two"}]},
					{"type":"message","content":[{"type":"output_text","text":""}]}
				]
			}
		}`),
		&param,
	)

	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "answer one and two" {
		t.Fatalf("content = %q, want concatenated non-empty text; output: %s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "think one and two" {
		t.Fatalf("reasoning = %q, want concatenated non-empty text; output: %s", got, out)
	}
}

func TestConvertCodexResponseToOpenAINonStreamKeepsAssistantRole(t *testing.T) {
	input := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`)

	output := ConvertCodexResponseToOpenAINonStream(
		context.Background(),
		"",
		nil,
		nil,
		input,
		nil,
	)

	if role := gjson.GetBytes(output, "choices.0.message.role").String(); role != "assistant" {
		t.Fatalf("role = %q, want assistant", role)
	}
}

func TestConvertCodexResponseToOpenAIForwardsCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	var param any
	_ = ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.4",
		nil,
		nil,
		[]byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4"}}`),
		&param,
	)
	stream := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.4",
		nil,
		nil,
		[]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens_details":{"cache_write_tokens":0}}}}`),
		&param,
	)
	if len(stream) != 1 {
		t.Fatalf("stream chunks = %d, want 1", len(stream))
	}
	for _, path := range []string{
		"usage.prompt_tokens_details.cache_write_tokens",
		"usage.prompt_tokens_details.cached_creation_tokens",
	} {
		value := gjson.GetBytes(stream[0], path)
		if !value.Exists() || value.Raw != "0" {
			t.Fatalf("stream %s = %s, want explicit zero", path, value.Raw)
		}
	}

	nonStream := ConvertCodexResponseToOpenAINonStream(
		ctx,
		"gpt-5.4",
		nil,
		nil,
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens_details":{"cache_write_tokens":40}},"output":[]}}`),
		nil,
	)
	for _, path := range []string{
		"usage.prompt_tokens_details.cache_write_tokens",
		"usage.prompt_tokens_details.cached_creation_tokens",
	} {
		if got := gjson.GetBytes(nonStream, path).Int(); got != 40 {
			t.Fatalf("non-stream %s = %d, want 40; output: %s", path, got, nonStream)
		}
	}
}

func TestConvertCodexResponseToOpenAIPreservesCacheWriteIntegerPrecision(t *testing.T) {
	const large = "999999999999999999999999999999"
	output := ConvertCodexResponseToOpenAINonStream(
		context.Background(),
		"gpt-5.6",
		nil,
		nil,
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens_details":{"cache_write_tokens":`+large+`}},"output":[]}}`),
		nil,
	)
	for _, path := range []string{
		"usage.prompt_tokens_details.cache_write_tokens",
		"usage.prompt_tokens_details.cached_creation_tokens",
	} {
		if got := gjson.GetBytes(output, path).Raw; got != large {
			t.Fatalf("%s = %s, want %s; output: %s", path, got, large, output)
		}
	}

	for _, invalid := range []string{"-1", "1.5", `"40"`, "true"} {
		input := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens_details":{"cache_write_tokens":` + invalid + `}},"output":[]}}`)
		invalidOutput := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.6", nil, nil, input, nil)
		if gjson.GetBytes(invalidOutput, "usage.prompt_tokens_details.cache_write_tokens").Exists() ||
			gjson.GetBytes(invalidOutput, "usage.prompt_tokens_details.cached_creation_tokens").Exists() {
			t.Fatalf("invalid cache write value %s survived: %s", invalid, invalidOutput)
		}
	}
}

func TestConvertCodexResponseToOpenAIPreservesServiceTierAcrossStream(t *testing.T) {
	ctx := context.Background()
	var param any
	created := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6",
		nil,
		nil,
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6","service_tier":" priority "}}`),
		&param,
	)
	if len(created) != 0 {
		t.Fatalf("response.created chunks = %d, want 0", len(created))
	}

	delta := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_text.delta","delta":"hello"}`),
		&param,
	)
	if len(delta) != 1 || gjson.GetBytes(delta[0], "service_tier").String() != "priority" {
		t.Fatalf("delta did not preserve service_tier: %q", delta)
	}

	terminal := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6",
		nil,
		nil,
		[]byte(`data: {"type":"response.completed","response":{"usage":{}}}`),
		&param,
	)
	if len(terminal) != 1 || gjson.GetBytes(terminal[0], "service_tier").String() != "priority" {
		t.Fatalf("terminal did not preserve service_tier: %q", terminal)
	}

	nonStream := ConvertCodexResponseToOpenAINonStream(
		ctx,
		"gpt-5.6",
		nil,
		nil,
		[]byte(`{"type":"response.completed","response":{"service_tier":"scale","output":[]}}`),
		nil,
	)
	if got := gjson.GetBytes(nonStream, "service_tier").String(); got != "scale" {
		t.Fatalf("non-stream service_tier = %q, want scale; output=%s", got, nonStream)
	}
}

func TestConvertCodexResponseToOpenAIMapsReasoningText(t *testing.T) {
	ctx := context.Background()
	var param any
	for _, event := range []struct {
		raw  string
		want string
	}{
		{raw: `data: {"type":"response.reasoning_text.delta","delta":"full reasoning"}`, want: "full reasoning"},
		{raw: `data: {"type":"response.reasoning_text.done"}`, want: "\n\n"},
	} {
		chunks := ConvertCodexResponseToOpenAI(ctx, "gpt-5.6", nil, nil, []byte(event.raw), &param)
		if len(chunks) != 1 || gjson.GetBytes(chunks[0], "choices.0.delta.reasoning_content").String() != event.want {
			t.Fatalf("reasoning event %s mapped to %q, want %q", event.raw, chunks, event.want)
		}
	}

	nonStream := ConvertCodexResponseToOpenAINonStream(
		ctx,
		"gpt-5.6",
		nil,
		nil,
		[]byte(`{"type":"response.completed","response":{"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":" then full"}]},
			{"type":"message","content":[{"type":"output_text","text":"answer"}]}
		]}}`),
		nil,
	)
	if got := gjson.GetBytes(nonStream, "choices.0.message.reasoning_content").String(); got != "summary then full" {
		t.Fatalf("reasoning_content = %q, want summary then full; output=%s", got, nonStream)
	}
}

func TestConvertCodexResponseToOpenAIMatchesDoneByCallIDWhenItemIDAppearsLate(t *testing.T) {
	ctx := context.Background()
	var param any

	added := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6-terra",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"TaskCreate","arguments":""}}`),
		&param,
	)
	if len(added) != 1 {
		t.Fatalf("added chunks = %d, want 1", len(added))
	}

	done := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6-terra",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"TaskCreate","arguments":"{\"subject\":\"test\"}"}}`),
		&param,
	)
	if len(done) != 1 {
		t.Fatalf("done chunks = %d, want 1", len(done))
	}
	toolCall := gjson.GetBytes(done[0], "choices.0.delta.tool_calls.0")
	if toolCall.Get("id").Exists() || toolCall.Get("function.name").Exists() {
		t.Fatalf("done chunk repeated tool identity: %s", toolCall.Raw)
	}
	if got := toolCall.Get("index").Int(); got != 0 {
		t.Fatalf("done tool index = %d, want 0", got)
	}
	if got := toolCall.Get("function.arguments").String(); got != `{"subject":"test"}` {
		t.Fatalf("done arguments = %q", got)
	}
}

func TestConvertCodexResponseToOpenAIDoesNotUseCurrentStateForConflictingIdentity(t *testing.T) {
	ctx := context.Background()
	var param any
	for _, event := range []string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"first","arguments":""}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_2","name":"second","arguments":""}}`,
	} {
		if chunks := ConvertCodexResponseToOpenAI(ctx, "gpt-5.6-terra", nil, nil, []byte(event), &param); len(chunks) != 1 {
			t.Fatalf("added chunks = %d, want 1", len(chunks))
		}
	}

	conflict := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6-terra",
		nil,
		nil,
		[]byte(`data: {"type":"response.function_call_arguments.delta","item_id":"fc_unknown","call_id":"call_unknown","output_index":99,"delta":"wrong"}`),
		&param,
	)
	if len(conflict) != 0 {
		t.Fatalf("conflicting identity produced %d chunks, want 0: %s", len(conflict), conflict[0])
	}

	matched := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6-terra",
		nil,
		nil,
		[]byte(`data: {"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"right"}`),
		&param,
	)
	if len(matched) != 1 {
		t.Fatalf("matched chunks = %d, want 1", len(matched))
	}
	if got := gjson.GetBytes(matched[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("matched tool index = %d, want 0; output: %s", got, matched[0])
	}
}

func TestConvertCodexResponseToOpenAINonStreamUsesToolCallsFinishReason(t *testing.T) {
	input := []byte(`{
		"type":"response.completed",
		"response":{
			"status":"completed",
			"output":[{
				"type":"function_call",
				"call_id":"call_1",
				"name":"search",
				"arguments":"{\"query\":\"test\"}"
			}]
		}
	}`)

	output := ConvertCodexResponseToOpenAINonStream(
		context.Background(),
		"",
		nil,
		nil,
		input,
		nil,
	)

	if got := gjson.GetBytes(output, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; output: %s", got, output)
	}
	if got := gjson.GetBytes(output, "choices.0.native_finish_reason").String(); got != "tool_calls" {
		t.Fatalf("native_finish_reason = %q, want tool_calls; output: %s", got, output)
	}
}
