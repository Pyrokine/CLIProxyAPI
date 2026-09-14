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
	streamValue := gjson.GetBytes(stream[0], "usage.prompt_tokens_details.cached_creation_tokens")
	if !streamValue.Exists() || streamValue.Int() != 0 {
		t.Fatalf("stream cached_creation_tokens = %s, want explicit zero", streamValue.Raw)
	}

	nonStream := ConvertCodexResponseToOpenAINonStream(
		ctx,
		"gpt-5.4",
		nil,
		nil,
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens_details":{"cache_write_tokens":40}},"output":[]}}`),
		nil,
	)
	if got := gjson.GetBytes(nonStream, "usage.prompt_tokens_details.cached_creation_tokens").Int(); got != 40 {
		t.Fatalf("non-stream cached_creation_tokens = %d, want 40; output: %s", got, nonStream)
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
