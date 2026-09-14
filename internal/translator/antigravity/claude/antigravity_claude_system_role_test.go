package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToAntigravity_NormalizesMessageLevelSystemRole(t *testing.T) {
	inputJSON := []byte(`{
		"messages": [
			{"role": "system", "content": [{"type":"text", "text":"system in messages"}]}
		]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3-flash-preview", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "request.contents.0.role").String(); got != "user" {
		t.Fatalf("request.contents.0.role = %q, want user; output=%s", got, outputStr)
	}
	wantReminder := "<system-reminder>\nsystem in messages\n</system-reminder>"
	if got := gjson.Get(outputStr, "request.contents.0.parts.0.text").String(); got != wantReminder {
		t.Fatalf("request.contents.0.parts.0.text = %q, want system reminder; output=%s", got, outputStr)
	}
}
