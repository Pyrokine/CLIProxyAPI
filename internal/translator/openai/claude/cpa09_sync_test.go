package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCPA09ClaudeRequestKeepsToolAdjacencyAcrossSystemReminder(t *testing.T) {
	out := convertClaudeRequestToOpenAIDefault(
		"openai-test",
		[]byte(`{
			"messages":[
				{"role":"assistant","content":[
					{"type":"tool_use","id":"call_1","name":"first","input":{}},
					{"type":"tool_use","id":"call_2","name":"second","input":{}}
				]},
				{"role":"system","content":"context update"},
				{"role":"user","content":[
					{"type":"text","text":"before"},
					{"type":"tool_result","tool_use_id":"call_2","content":"result 2"},
					{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},
					{"type":"tool_result","tool_use_id":"call_1","content":"result 1"}
				]}
			]
		}`),
		false,
	)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 5 {
		t.Fatalf("message count = %d, want 5; output=%s", len(messages), out)
	}
	wantRoles := []string{"assistant", "tool", "tool", "user", "user"}
	for index, want := range wantRoles {
		if got := messages[index].Get("role").String(); got != want {
			t.Fatalf("messages[%d].role = %q, want %q; output=%s", index, got, want, out)
		}
	}
	if got := messages[1].Get("tool_call_id").String(); got != "call_1" {
		t.Fatalf("first tool result id = %q, want call_1", got)
	}
	if got := messages[2].Get("tool_call_id").String(); got != "call_2" {
		t.Fatalf("second tool result id = %q, want call_2", got)
	}
	if got := messages[3].Get("content.0.text").String(); got != "<system-reminder>\ncontext update\n</system-reminder>" {
		t.Fatalf("system reminder = %q", got)
	}
	if got := messages[4].Get("content.1.type").String(); got != "image_url" {
		t.Fatalf("user image type = %q, want image_url", got)
	}
}

func TestCPA09ClaudeRequestUsesArrayStopAndNormalizesMissingSchema(t *testing.T) {
	out := convertClaudeRequestToOpenAIDefault(
		"openai-test",
		[]byte(`{
			"stop_sequences":["END"],
			"messages":[{"role":"user","content":"hello"}],
			"tools":[{"name":"work","input_schema":null}]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	if !root.Get("stop").IsArray() || root.Get("stop.0").String() != "END" {
		t.Fatalf("stop = %s, want [\"END\"]", root.Get("stop").Raw)
	}
	schema := root.Get("tools.0.function.parameters")
	if got := schema.Get("type").String(); got != "object" {
		t.Fatalf("parameters.type = %q, want object", got)
	}
	if !schema.Get("properties").IsObject() {
		t.Fatalf("parameters.properties must be object: %s", schema.Raw)
	}
}

func TestCPA09ClaudeRequestRemovesUnsupportedUnicodePatterns(t *testing.T) {
	out := convertClaudeRequestToOpenAIDefault(
		"openai-test",
		[]byte(`{
			"messages":[{"role":"user","content":"hello"}],
			"tools":[{"name":"work","input_schema":{"type":"object","properties":{"bad":{"type":"string","pattern":"\\p{L}+"},"good":{"type":"string","pattern":"^[a-z]+$"}},"patternProperties":{"^\\p{N}+$":{"type":"string"},"^[a-z]+$":{"type":"string"}}}}]
		}`),
		false,
	)
	schema := gjson.GetBytes(out, "tools.0.function.parameters")
	if schema.Get("properties.bad.pattern").Exists() {
		t.Fatalf("unsupported Unicode pattern remains: %s", schema.Raw)
	}
	if got := schema.Get("properties.good.pattern").String(); got != "^[a-z]+$" {
		t.Fatalf("valid pattern = %q", got)
	}
	if _, exists := schema.Get("patternProperties").Map()[`^\p{N}+$`]; exists {
		t.Fatalf("unsupported patternProperties key remains: %s", schema.Raw)
	}
}
