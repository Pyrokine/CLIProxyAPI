package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCLI_NormalizesMessageLevelSystemRole(t *testing.T) {
	inputJSON := []byte(`{
		"messages": [
			{"role": "system", "content": [{"type":"text", "text":"system in messages"}]}
		]
	}`)

	output := convertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "request.contents.0.role").String(); got != "user" {
		t.Fatalf("request.contents.0.role = %q, want user; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.text").String(); got != "system in messages" {
		t.Fatalf("request.contents.0.parts.0.text = %q, want system in messages; output=%s", got, output)
	}
}
