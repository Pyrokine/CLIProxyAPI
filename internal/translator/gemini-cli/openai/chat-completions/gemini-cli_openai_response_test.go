package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCliFinishReasonOnlyOnFinalChunk(t *testing.T) {
	var param any
	first := convertCliResponseToOpenAI(
		context.Background(), "gemini-2.5-pro", nil, nil, []byte(`{
		"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}]}
	}`), &param,
	)
	if len(first) != 1 {
		t.Fatalf("first chunks = %d, want 1", len(first))
	}
	if got := gjson.GetBytes(first[0], "choices.0.finish_reason"); got.Exists() && got.Type != gjson.Null {
		t.Fatalf("first finish_reason = %s, want null; chunk: %s", got.Raw, first[0])
	}

	final := convertCliResponseToOpenAI(
		context.Background(), "gemini-2.5-pro", nil, nil, []byte(`{
		"response":{
			"candidates":[{"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}
		}
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
