package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiResponseToOpenAIAddsThoughtTokensWhenCandidateCountIsMissing(t *testing.T) {
	var param any
	chunk := []byte(`{"usageMetadata":{"promptTokenCount":16,"thoughtsTokenCount":42,"totalTokenCount":58}}`)

	result := convertGeminiResponseToOpenAI(context.Background(), "model", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("result count = %d, want 1", len(result))
	}
	completionTokens := gjson.GetBytes(result[0], "usage.completion_tokens")
	if !completionTokens.Exists() || completionTokens.Int() != 42 {
		t.Fatalf("completion_tokens = %s, want present with value 42; output: %s", completionTokens.Raw, result[0])
	}
}

func TestConvertGeminiResponseToOpenAINonStreamAddsThoughtTokensWhenCandidateCountIsMissing(t *testing.T) {
	response := []byte(`{"usageMetadata":{"promptTokenCount":16,"thoughtsTokenCount":42,"totalTokenCount":58}}`)

	result := ConvertGeminiResponseToOpenAINonStream(context.Background(), "model", nil, nil, response, nil)
	completionTokens := gjson.GetBytes(result, "usage.completion_tokens")
	if !completionTokens.Exists() || completionTokens.Int() != 42 {
		t.Fatalf("completion_tokens = %s, want present with value 42; output: %s", completionTokens.Raw, result)
	}
}

func TestConvertGeminiResponseToOpenAINonStreamKeepsAssistantRole(t *testing.T) {
	input := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`)

	output := ConvertGeminiResponseToOpenAINonStream(
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

func TestConvertGeminiResponseToOpenAIStreamingSetsAssistantRoleOnce(t *testing.T) {
	input := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hello"},{"functionCall":{"name":"lookup","args":{}}},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}}]}`)
	var param any

	outputs := convertGeminiResponseToOpenAI(
		context.Background(),
		"",
		nil,
		nil,
		input,
		&param,
	)

	if len(outputs) != 1 {
		t.Fatalf("output count = %d, want 1", len(outputs))
	}
	if role := gjson.GetBytes(outputs[0], "choices.0.delta.role").String(); role != "assistant" {
		t.Fatalf("role = %q, want assistant", role)
	}
	if got := gjson.GetBytes(outputs[0], "choices.0.delta.content").String(); got != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}
	if !gjson.GetBytes(outputs[0], "choices.0.delta.tool_calls.0").Exists() {
		t.Fatal("tool call should be present")
	}
	if !gjson.GetBytes(outputs[0], "choices.0.delta.images.0").Exists() {
		t.Fatal("image should be present")
	}
}

func TestConvertGeminiResponseToOpenAIValidatesUsageBoundaries(t *testing.T) {
	response := []byte(`{"usageMetadata":{"promptTokenCount":-1,"candidatesTokenCount":7,"thoughtsTokenCount":1.5,"totalTokenCount":"8","cachedContentTokenCount":-3}}`)
	var param any
	stream := convertGeminiResponseToOpenAI(context.Background(), "model", nil, nil, response, &param)
	if len(stream) != 1 {
		t.Fatalf("stream output count = %d, want 1", len(stream))
	}
	for path, want := range map[string]int64{
		"usage.prompt_tokens":                              0,
		"usage.completion_tokens":                          7,
		"usage.completion_tokens_details.reasoning_tokens": 0,
		"usage.total_tokens":                               0,
		"usage.prompt_tokens_details.cached_tokens":        0,
	} {
		if got := gjson.GetBytes(stream[0], path).Int(); got != want {
			t.Fatalf("stream %s = %d, want %d; output=%s", path, got, want, stream[0])
		}
	}

	nonStream := ConvertGeminiResponseToOpenAINonStream(
		context.Background(), "model", nil, nil, response, nil,
	)
	if got := gjson.GetBytes(nonStream, "usage.completion_tokens").Int(); got != 7 {
		t.Fatalf("non-stream completion_tokens = %d, want 7; output=%s", got, nonStream)
	}
}

func TestConvertGeminiResponseToOpenAIPreservesTextAndAudioTranscriptionOrder(t *testing.T) {
	response := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"first "},{"audioTranscription":{"text":"second "}},{"text":"third"}]}}]}`)
	var param any
	stream := convertGeminiResponseToOpenAI(context.Background(), "model", nil, nil, response, &param)
	if len(stream) != 1 || gjson.GetBytes(stream[0], "choices.0.delta.content").String() != "first second third" {
		t.Fatalf("stream text/audio order changed: %q", stream)
	}

	nonStream := ConvertGeminiResponseToOpenAINonStream(
		context.Background(), "model", nil, nil, response, nil,
	)
	if got := gjson.GetBytes(nonStream, "choices.0.message.content").String(); got != "first second third" {
		t.Fatalf("non-stream text/audio order = %q; output=%s", got, nonStream)
	}
}

func TestConvertGeminiResponseToOpenAIAccumulatesWrappedStreamMetadata(t *testing.T) {
	var param any
	first := convertGeminiResponseToOpenAI(
		context.Background(), "fallback", nil, nil,
		[]byte(`{"response":{"responseId":"response-1","modelVersion":"gemini-model","usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":4,"totalTokenCount":9}}}`),
		&param,
	)
	if len(first) != 1 {
		t.Fatalf("metadata chunk count = %d, want 1", len(first))
	}
	final := convertGeminiResponseToOpenAI(
		context.Background(), "fallback", nil, nil,
		[]byte(`{"response":{"candidates":[{"index":0,"finishReason":"STOP"}]}}`),
		&param,
	)
	if len(final) != 1 {
		t.Fatalf("final chunk count = %d, want 1", len(final))
	}
	if got := gjson.GetBytes(final[0], "id").String(); got != "response-1" {
		t.Fatalf("id = %q, want response-1; output=%s", got, final[0])
	}
	if got := gjson.GetBytes(final[0], "model").String(); got != "gemini-model" {
		t.Fatalf("model = %q, want gemini-model; output=%s", got, final[0])
	}
	if got := gjson.GetBytes(final[0], "usage.completion_tokens").Int(); got != 6 {
		t.Fatalf("completion_tokens = %d, want 6; output=%s", got, final[0])
	}
}

func TestGeminiFinishReasonOnlyOnFinalChunk(t *testing.T) {
	var param any
	first := convertGeminiResponseToOpenAI(
		context.Background(), "gemini-2.5-pro", nil, nil, []byte(`{
		"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}]
	}`), &param,
	)
	if len(first) != 1 {
		t.Fatalf("first chunks = %d, want 1", len(first))
	}
	if got := gjson.GetBytes(first[0], "choices.0.finish_reason"); got.Exists() && got.Type != gjson.Null {
		t.Fatalf("first finish_reason = %s, want null; chunk: %s", got.Raw, first[0])
	}

	final := convertGeminiResponseToOpenAI(
		context.Background(), "gemini-2.5-pro", nil, nil, []byte(`{
		"candidates":[{"index":0,"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}
	}`), &param,
	)
	if len(final) != 1 {
		t.Fatalf("final chunks = %d, want 1", len(final))
	}
	if got := gjson.GetBytes(final[0], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("final finish_reason = %q, want tool_calls; chunk: %s", got, final[0])
	}
	if got := gjson.GetBytes(final[0], "choices.0.native_finish_reason").String(); got != "stop" {
		t.Fatalf("native_finish_reason = %q, want stop; chunk: %s", got, final[0])
	}
}
