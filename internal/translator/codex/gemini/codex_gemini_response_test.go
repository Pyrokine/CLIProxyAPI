package gemini

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToGeminiIncompleteTerminal(t *testing.T) {
	terminal := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","model":"gpt-5.5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	var param any
	streamOut := ConvertCodexResponseToGemini(
		context.Background(), "gemini-2.5-pro", nil, nil, append([]byte("data: "), terminal...), &param,
	)
	if len(streamOut) != 1 {
		t.Fatalf("stream output count = %d, want 1", len(streamOut))
	}
	if got := gjson.GetBytes(streamOut[0], "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
		t.Fatalf("stream finishReason = %q, want MAX_TOKENS. Output: %s", got, streamOut[0])
	}

	nonStreamOut := ConvertCodexResponseToGeminiNonStream(
		context.Background(), "gemini-2.5-pro", nil, nil, terminal, nil,
	)
	if got := gjson.GetBytes(nonStreamOut, "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
		t.Fatalf("non-stream finishReason = %q, want MAX_TOKENS. Output: %s", got, nonStreamOut)
	}
}

func TestConvertCodexResponseToGemini_StreamEmptyOutputUsesOutputItemDoneMessageFallback(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(
			outputs, ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)...,
		)
	}

	found := false
	for _, out := range outputs {
		if gjson.GetBytes(out, "candidates.0.content.parts.0.text").String() == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fallback content from response.output_item.done message; outputs=%q", outputs)
	}
}
