package chat_completions

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCPA09ChatRequestPreservesIdentityAndUsesLastDuplicateToolResult(t *testing.T) {
	out := convertOpenAIRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"metadata":{"user_id":"caller-user"},
			"messages":[
				{"role":"assistant","tool_calls":[{"id":"call.raw/1","type":"function","function":{"name":"work","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call.raw/1","content":"first"},
				{"role":"tool","tool_call_id":"call.raw/1","content":"last"}
			]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	if got := root.Get("metadata.user_id").String(); got != "caller-user" {
		t.Fatalf("metadata.user_id = %q, want caller-user", got)
	}
	messages := root.Get("messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2; output=%s", len(messages), out)
	}
	result := messages[1].Get("content.0")
	if got := result.Get("tool_use_id").String(); got != "call_raw_1" {
		t.Fatalf("tool_use_id = %q, want call_raw_1", got)
	}
	if got := result.Get("content").String(); got != "last" {
		t.Fatalf("tool result = %q, want last", got)
	}
}

func TestCPA09ChatRequestAddsStructuredOutputAndDefaultToolSchema(t *testing.T) {
	out := convertOpenAIRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"messages":[{"role":"user","content":"return data"}],
			"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"value":{"type":"string"}}}}},
			"tools":[{"type":"function","function":{"name":"work"}}]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	instruction := root.Get("system.0.text").String()
	if !strings.Contains(instruction, "Schema Name: answer") || !strings.Contains(instruction, `"value"`) {
		t.Fatalf("structured output instruction missing schema: %q", instruction)
	}
	schema := root.Get("tools.0.input_schema")
	if got := schema.Get("type").String(); got != "object" {
		t.Fatalf("input_schema.type = %q, want object", got)
	}
	if !schema.Get("properties").IsObject() {
		t.Fatalf("input_schema.properties must be an object: %s", schema.Raw)
	}
}

func TestCPA09ChatRequestDerivesStableRequestScopedIdentity(t *testing.T) {
	requestA := []byte(`{"prompt_cache_key":"session-a","messages":[{"role":"user","content":"hello"}]}`)
	requestB := []byte(`{"prompt_cache_key":"session-b","messages":[{"role":"user","content":"hello"}]}`)
	first := gjson.GetBytes(convertOpenAIRequestToClaudeDefault("claude-test", requestA, false), "metadata.user_id").String()
	retry := gjson.GetBytes(convertOpenAIRequestToClaudeDefault("claude-test", requestA, false), "metadata.user_id").String()
	second := gjson.GetBytes(convertOpenAIRequestToClaudeDefault("claude-test", requestB, false), "metadata.user_id").String()
	if first == "" || first != retry {
		t.Fatalf("request identity is not stable: first=%q retry=%q", first, retry)
	}
	if first == second {
		t.Fatalf("different sessions share identity %q", first)
	}
}
