package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

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
