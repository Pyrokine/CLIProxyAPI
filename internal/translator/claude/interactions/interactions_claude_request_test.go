package interactions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertInteractionsRequestToClaudePreservesToolError(t *testing.T) {
	out := convertInteractionsRequestToClaude("claude-test", []byte(`{"input":[{"type":"function_result","name":"lookup","call_id":"call-1","result":"failed","is_error":true}]}`), false)
	result := gjson.GetBytes(out, "messages.0.content.0")
	if result.Get("type").String() != "tool_result" || result.Get("tool_use_id").String() != "call-1" || !result.Get("is_error").Bool() {
		t.Fatalf("tool error was not preserved: %s", out)
	}
}

func TestConvertInteractionsRequestToClaudeNormalizesToolSchemas(t *testing.T) {
	raw := []byte(`{
		"input":"hello",
		"tools":[{
			"functionDeclarations":[
				{
					"name":"any_union",
					"parameters":{"anyOf":[
						{"type":"object","properties":{"query":{"type":"string"}}},
						{"type":"object","properties":{"limit":{"type":"integer"}}}
					]}
				},
				{
					"name":"one_union_alias",
					"parametersJsonSchema":{
						"type":"object",
						"properties":{"query":{"type":"string"},"id":{"type":"string"}},
						"oneOf":[{"required":["query"]},{"required":["id"]}]
					}
				},
				{"name":"missing_schema"}
			]
		}]
	}`)

	out := convertInteractionsRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)
	for _, name := range []string{"any_union", "one_union_alias", "missing_schema"} {
		schema := root.Get(`tools.#(name=="` + name + `").input_schema`)
		if got := schema.Get("type").String(); got != "object" {
			t.Fatalf("%s input_schema.type = %q, want object. Output: %s", name, got, string(out))
		}
		if !schema.Get("properties").IsObject() {
			t.Fatalf("%s input_schema.properties should be an object. Output: %s", name, string(out))
		}
		if schema.Get("anyOf").Exists() || schema.Get("oneOf").Exists() {
			t.Fatalf("%s input_schema should not contain a root union. Output: %s", name, string(out))
		}
	}

	anySchema := root.Get(`tools.#(name=="any_union").input_schema`)
	if !anySchema.Get("properties.query").Exists() || !anySchema.Get("properties.limit").Exists() {
		t.Fatalf("any_union properties were not merged. Output: %s", string(out))
	}
	oneSchema := root.Get(`tools.#(name=="one_union_alias").input_schema`)
	if oneSchema.Get("required").Exists() {
		t.Fatalf("one_union_alias should not merge alternative required fields. Output: %s", string(out))
	}
}

func TestConvertInteractionsRequestToClaudeGroupsConsecutiveRoleTurns(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"thought","content":[{"type":"thinking","thinking":"reason"}]},
			{"type":"model_output","content":[{"type":"text","text":"answer"}]},
			{"type":"function_call","name":"first","call_id":"call_1","arguments":{}},
			{"type":"function_call","name":"second","call_id":"call_2","arguments":{}},
			{"type":"function_result","call_id":"call_1","result":{"value":"one"}},
			{"type":"function_result","call_id":"call_2","result":{"value":"two"}}
		]
	}`)

	out := convertInteractionsRequestToClaude("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	assistantContent := messages[0].Get("content").Array()
	wantTypes := []string{"thinking", "text", "tool_use", "tool_use"}
	if len(assistantContent) != len(wantTypes) {
		t.Fatalf(
			"assistant content count = %d, want %d. Output: %s", len(assistantContent), len(wantTypes), string(out),
		)
	}
	for index, wantType := range wantTypes {
		if got := assistantContent[index].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q", index, got, wantType)
		}
	}
	userContent := messages[1].Get("content").Array()
	if len(userContent) != 2 {
		t.Fatalf("user content count = %d, want 2. Output: %s", len(userContent), string(out))
	}
	for index, wantID := range []string{"call_1", "call_2"} {
		if got := userContent[index].Get("tool_use_id").String(); got != wantID {
			t.Fatalf("user content[%d].tool_use_id = %q, want %q", index, got, wantID)
		}
	}
}

func TestConvertInteractionsRequestToClaudeDoesNotMergeAcrossRoleChanges(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"model_output","content":"first"},
			{"type":"user_input","content":"question"},
			{"type":"model_output","content":"second"}
		]
	}`)

	out := convertInteractionsRequestToClaude("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("message count = %d, want 3. Output: %s", len(messages), string(out))
	}
	for index, wantRole := range []string{"assistant", "user", "assistant"} {
		if got := messages[index].Get("role").String(); got != wantRole {
			t.Fatalf("messages[%d].role = %q, want %q", index, got, wantRole)
		}
	}
}
