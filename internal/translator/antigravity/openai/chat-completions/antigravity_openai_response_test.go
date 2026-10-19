package chat_completions

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
)

func TestConvertAntigravityResponseToOpenAIIncludesThoughtTokensInCompletion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chunk      string
		completion int64
	}{
		{name: "thought only", chunk: `{"response":{"usageMetadata":{"promptTokenCount":16,"thoughtsTokenCount":42,"totalTokenCount":58}}}`, completion: 42},
		{name: "thought and candidate", chunk: `{"response":{"usageMetadata":{"promptTokenCount":16,"candidatesTokenCount":5,"thoughtsTokenCount":42,"totalTokenCount":63}}}`, completion: 47},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var param any
			payload := []byte(tc.chunk)
			result := convertAntigravityResponseToOpenAI(context.Background(), "model", nil, nil, payload, &param)
			if len(result) != 1 {
				t.Fatalf("result count = %d, want 1", len(result))
			}
			completion := gjson.GetBytes(result[0], "usage.completion_tokens")
			if !completion.Exists() || completion.Int() != tc.completion {
				t.Fatalf("completion_tokens = %s, want %d; output: %s", completion.Raw, tc.completion, result[0])
			}
			if reasoning := gjson.GetBytes(result[0], "usage.completion_tokens_details.reasoning_tokens").Int(); reasoning != 42 {
				t.Fatalf("reasoning_tokens = %d, want 42", reasoning)
			}
			if total := gjson.GetBytes(result[0], "usage.total_tokens").Int(); total != 16+tc.completion {
				t.Fatalf("total_tokens = %d, want %d", total, 16+tc.completion)
			}
			nonStream := convertAntigravityResponseToOpenAINonStream(context.Background(), "model", nil, nil, payload, nil)
			if got := gjson.GetBytes(nonStream, "usage.completion_tokens").Int(); got != tc.completion {
				t.Fatalf("non-stream completion_tokens = %d, want %d", got, tc.completion)
			}
		})
	}
}

func TestFinishReasonToolCallsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Contains functionCall - should set SawToolCall = true
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_files","args":{"path":"."}}}]}}]}}`)
	result1 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Verify chunk1 has no finish_reason (null)
	if len(result1) != 1 {
		t.Fatalf("Expected 1 result from chunk1, got %d", len(result1))
	}
	fr1 := gjson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Errorf("Expected finish_reason to be null in chunk1, got: %v", fr1.String())
	}

	// Chunk 2: Contains finishReason STOP + usage (final chunk, no functionCall)
	// This simulates what the upstream sends AFTER the tool call chunk
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"totalTokenCount":30}}}`)
	result2 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify chunk2 has finish_reason: "tool_calls" (not "stop")
	if len(result2) != 1 {
		t.Fatalf("Expected 1 result from chunk2, got %d", len(result2))
	}
	fr2 := gjson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr2 != "tool_calls" {
		t.Errorf("Expected finish_reason 'tool_calls', got: %s", fr2)
	}

	// Verify native_finish_reason is lowercase upstream value
	nfr2 := gjson.GetBytes(result2[0], "choices.0.native_finish_reason").String()
	if nfr2 != "stop" {
		t.Errorf("Expected native_finish_reason 'stop', got: %s", nfr2)
	}
}

func TestFinishReasonStopForNormalText(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content only
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello world"}]}}]}}`)
	convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with STOP
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)
	result2 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "stop" (no tool calls were made)
	fr := gjson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "stop" {
		t.Errorf("Expected finish_reason 'stop', got: %s", fr)
	}
}

func TestFinishReasonMaxTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`)
	convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with MAX_TOKENS
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":110}}}`)
	result2 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "max_tokens"
	fr := gjson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "max_tokens" {
		t.Errorf("Expected finish_reason 'max_tokens', got: %s", fr)
	}
}

func TestToolCallTakesPriorityOverMaxTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Contains functionCall
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"test","args":{}}}]}}]}}`)
	convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with MAX_TOKENS (but we had a tool call, so tool_calls should win)
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":110}}}`)
	result2 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "tool_calls" (takes priority over max_tokens)
	fr := gjson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "tool_calls" {
		t.Errorf("Expected finish_reason 'tool_calls', got: %s", fr)
	}
}

func TestNoFinishReasonOnIntermediateChunks(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content (no finish reason, no usage)
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`)
	result1 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Verify no finish_reason on intermediate chunk
	fr1 := gjson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Errorf("Expected no finish_reason on intermediate chunk, got: %v", fr1)
	}

	// Chunk 2: More text (no finish reason, no usage)
	chunk2 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":" world"}]}}]}}`)
	result2 := convertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify no finish_reason on intermediate chunk
	fr2 := gjson.GetBytes(result2[0], "choices.0.finish_reason")
	if fr2.Exists() && fr2.String() != "" && fr2.Type.String() != "Null" {
		t.Errorf("Expected no finish_reason on intermediate chunk, got: %v", fr2)
	}
}

func TestConvertAntigravityResponseToOpenAIStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[
		{"type":"function","function":{"name":"` + first + `"}},
		{"type":"function","function":{"name":"` + second + `"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)
	var param any

	output := convertAntigravityResponseToOpenAI(
		context.Background(),
		"gemini-3-flash",
		original,
		nil,
		responseJSON,
		&param,
	)
	if len(output) != 1 {
		t.Fatalf("output count = %d, want 1", len(output))
	}
	if got := gjson.GetBytes(output[0], "choices.0.delta.tool_calls.0.function.name").String(); got != second {
		t.Fatalf("stream function.name = %q, want %q. Output: %s", got, second, output[0])
	}
}

func TestConvertAntigravityResponseToOpenAINonStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[
		{"type":"function","function":{"name":"` + first + `"}},
		{"type":"function","function":{"name":"` + second + `"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)

	output := convertAntigravityResponseToOpenAINonStream(
		context.Background(),
		"gemini-3-flash",
		original,
		nil,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(output, "choices.0.message.tool_calls.0.function.name").String(); got != second {
		t.Fatalf("function.name = %q, want %q. Output: %s", got, second, output)
	}
}

func TestConvertAntigravityResponseToOpenAIRestoresExternalToolNameChain(t *testing.T) {
	const clientName = "external_external_read_file"
	const upstreamName = "external_external_external_read_file"
	original := []byte(`{"tools":[{"type":"function","function":{"name":"` + clientName + `"}}]}`)
	response := []byte(`{"response":{"candidates":[{"content":{"parts":[{
		"functionCall":{"name":"` + upstreamName + `","args":{}}
	}]}}]}}`)

	var param any
	stream := convertAntigravityResponseToOpenAI(
		context.Background(),
		"gemini-3-flash",
		original,
		nil,
		response,
		&param,
	)
	if len(stream) != 1 {
		t.Fatalf("stream output count = %d, want 1", len(stream))
	}
	if got := gjson.GetBytes(stream[0], "choices.0.delta.tool_calls.0.function.name").String(); got != clientName {
		t.Fatalf("stream function.name = %q, want %q; output=%s", got, clientName, stream[0])
	}

	nonStream := convertAntigravityResponseToOpenAINonStream(
		context.Background(),
		"gemini-3-flash",
		original,
		nil,
		response,
		nil,
	)
	if got := gjson.GetBytes(nonStream, "choices.0.message.tool_calls.0.function.name").String(); got != clientName {
		t.Fatalf("non-stream function.name = %q, want %q; output=%s", got, clientName, nonStream)
	}
}

func TestConvertAntigravityResponseToOpenAIPreservesAudioTranscriptionOrder(t *testing.T) {
	response := []byte(`{"response":{"candidates":[{"content":{"parts":[
		{"text":"first "},
		{"audioTranscription":{"text":"second "}},
		{"text":"third"}
	]}}]}}`)

	var param any
	stream := convertAntigravityResponseToOpenAI(
		context.Background(),
		"gemini-3-flash",
		nil,
		nil,
		response,
		&param,
	)
	if len(stream) != 1 {
		t.Fatalf("stream output count = %d, want 1", len(stream))
	}
	if got := gjson.GetBytes(stream[0], "choices.0.delta.content").String(); got != "first second third" {
		t.Fatalf("stream content = %q, want ordered transcription; output=%s", got, stream[0])
	}

	nonStream := convertAntigravityResponseToOpenAINonStream(
		context.Background(),
		"gemini-3-flash",
		nil,
		nil,
		response,
		nil,
	)
	if got := gjson.GetBytes(nonStream, "choices.0.message.content").String(); got != "first second third" {
		t.Fatalf("non-stream content = %q, want ordered transcription; output=%s", got, nonStream)
	}
}
