package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToGeminiTreatsDeveloperAsSystemInstruction(t *testing.T) {
	input := []byte(`{
		"input": [
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"follow policy"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", input, false)

	if got := gjson.GetBytes(output, "systemInstruction.parts.0.text").String(); got != "follow policy" {
		t.Fatalf("systemInstruction.parts.0.text = %q, want follow policy; output=%s", got, output)
	}
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("contents length = %d, want 1; output=%s", len(contents), output)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("contents.0.role = %q, want user; output=%s", got, output)
	}
}
