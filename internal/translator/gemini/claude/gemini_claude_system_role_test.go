package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToGemini_NormalizesMessageLevelSystemRole(t *testing.T) {
	inputJSON := []byte(`{
		"messages": [
			{"role": "system", "content": [{"type":"text", "text":"system in messages"}]}
		]
	}`)

	output := ConvertClaudeRequestToGemini("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "contents.0.role").String(); got != "user" {
		t.Fatalf("contents.0.role = %q, want user; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "contents.0.parts.0.text").String(); got != "system in messages" {
		t.Fatalf("contents.0.parts.0.text = %q, want system in messages; output=%s", got, output)
	}
}
