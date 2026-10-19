package chat_completions

import (
	"context"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRegisteredGeminiCLICacheUsagePreservesExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		wantExists  bool
		want        int64
	}{
		{name: "absent"},
		{name: "zero", field: `,"cachedContentTokenCount":0`, wantExists: true},
		{name: "positive", field: `,"cachedContentTokenCount":2`, wantExists: true, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			response := []byte(`{"response":{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5` + tc.field + `}}}`)
			chunks := sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatGeminiCLI, sdktranslator.FormatOpenAI,
				"gemini-2.5-pro", nil, nil, response, &state)
			if len(chunks) != 1 {
				t.Fatalf("chunk count = %d, want 1", len(chunks))
			}
			cached := gjson.GetBytes(chunks[0], "usage.prompt_tokens_details.cached_tokens")
			if cached.Exists() != tc.wantExists || cached.Int() != tc.want {
				t.Fatalf("cache usage = %s, want present=%t value=%d: %s", cached.Raw, tc.wantExists, tc.want, chunks[0])
			}
		})
	}
}

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
