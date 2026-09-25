package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeMessageSystemReminderTextPreservesTextPartOrder(t *testing.T) {
	content := gjson.Parse(`[
		{"type":"text","text":"first instruction"},
		{"type":"image","source":{"type":"base64","data":"image"}},
		{"type":"text","text":"second instruction"}
	]`)

	got, ok := ClaudeMessageSystemReminderText(content)
	if !ok {
		t.Fatal("system content was not converted")
	}
	const want = "<system-reminder>\nfirst instruction\nsecond instruction\n</system-reminder>"
	if got != want {
		t.Fatalf("reminder = %q, want %q", got, want)
	}
}

func TestClaudeMessageSystemReminderTextRejectsEmptyContent(t *testing.T) {
	for _, content := range []gjson.Result{
		{},
		gjson.Parse(`""`),
		gjson.Parse(`[]`),
		gjson.Parse(`[{"type":"image","source":{"data":"image"}}]`),
	} {
		if got, ok := ClaudeMessageSystemReminderText(content); ok || got != "" {
			t.Fatalf("empty reminder = %q, %v", got, ok)
		}
	}
}
