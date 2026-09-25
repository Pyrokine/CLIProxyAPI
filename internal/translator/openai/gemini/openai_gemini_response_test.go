package gemini

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponseToGeminiIgnoresNonTerminalFinishReasons(t *testing.T) {
	for _, finishReason := range []string{"null", `""`, "true", "1", `{}`} {
		t.Run(finishReason, func(t *testing.T) {
			var state any
			chunk := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":` + finishReason + `}]}`)
			for _, output := range ConvertOpenAIResponseToGemini(
				context.Background(), "gpt-test", nil, nil, chunk, &state,
			) {
				if got := gjson.GetBytes(output, "candidates.0.finishReason"); got.Exists() {
					t.Fatalf("finishReason = %s, want omitted; output=%s", got.Raw, output)
				}
			}
		})
	}

	var state any
	output := ConvertOpenAIResponseToGemini(
		context.Background(),
		"gpt-test",
		nil,
		nil,
		[]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		&state,
	)
	if len(output) != 1 || gjson.GetBytes(output[0], "candidates.0.finishReason").String() != "STOP" {
		t.Fatalf("terminal finish reason not translated: %q", output)
	}
}

func TestConvertOpenAIResponseToGeminiNonStreamIgnoresNonTerminalFinishReasons(t *testing.T) {
	for _, finishReason := range []string{"null", `""`, "true", "1", `{}`} {
		t.Run(finishReason, func(t *testing.T) {
			payload := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":` + finishReason + `}]}`)
			output := ConvertOpenAIResponseToGeminiNonStream(
				context.Background(), "gpt-test", nil, nil, payload, nil,
			)
			if got := gjson.GetBytes(output, "candidates.0.finishReason"); got.Exists() {
				t.Fatalf("finishReason = %s, want omitted; output=%s", got.Raw, output)
			}
		})
	}
}
