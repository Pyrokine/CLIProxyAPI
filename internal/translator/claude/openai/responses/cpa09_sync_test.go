package responses

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCPA09ResponsesRequestIdentityAgentMessageAndStructuredOutput(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"metadata":{"user_id":"caller-user"},
			"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}},
			"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"delegated task"}]}]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	if got := root.Get("metadata.user_id").String(); got != "caller-user" {
		t.Fatalf("metadata.user_id = %q, want caller-user", got)
	}
	if got := root.Get("messages.0.role").String(); got != "user" {
		t.Fatalf("agent message role = %q, want user; output=%s", got, out)
	}
	if got := root.Get("messages.0.content").String(); got != "delegated task" {
		t.Fatalf("agent message text = %q, want delegated task; output=%s", got, out)
	}
	instruction := root.Get("system.0.text").String()
	if !strings.Contains(instruction, "Schema Name: answer") || !strings.Contains(instruction, `"ok"`) {
		t.Fatalf("structured output instruction missing: %q", instruction)
	}
}

func TestCPA09ResponsesRequestDeduplicatesToolOutputsByRawID(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"input":[
				{"type":"function_call","call_id":"call.raw/1","name":"work","arguments":"{}"},
				{"type":"function_call_output","call_id":"call.raw/1","output":"first"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"between"}]},
				{"type":"function_call_output","call_id":"call.raw/1","output":"last"}
			]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	var results []gjson.Result
	for _, message := range root.Get("messages").Array() {
		for _, part := range message.Get("content").Array() {
			if part.Get("type").String() == "tool_result" {
				results = append(results, part)
			}
		}
	}
	if len(results) != 1 {
		t.Fatalf("tool result count = %d, want 1; output=%s", len(results), out)
	}
	if got := results[0].Get("tool_use_id").String(); got != "call_raw_1" {
		t.Fatalf("tool_use_id = %q, want call_raw_1", got)
	}
	if got := results[0].Get("content").String(); got != "last" {
		t.Fatalf("tool output = %q, want last", got)
	}
}

func TestCPA09ResponsesRequestReplaysWebSearchAndCitations(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{
			"input":[
				{"id":"ws_native-1","type":"web_search_call","action":{"type":"search","query":"Qwen"},"results":[{"type":"computer_initialize_state","url":"https://example.com","encrypted_content":"cipher"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[{"type":"web_search_result_location","encrypted_index":"idx"}]}]}
			]
		}`),
		false,
	)
	root := gjson.ParseBytes(out)
	content := root.Get("messages.0.content")
	if got := content.Get("0.type").String(); got != "server_tool_use" {
		t.Fatalf("content[0].type = %q, want server_tool_use; output=%s", got, out)
	}
	if got := content.Get("0.id").String(); got != "srvtoolu_native_1" {
		t.Fatalf("server tool id = %q, want srvtoolu_native_1", got)
	}
	if got := content.Get("1.type").String(); got != "web_search_tool_result" {
		t.Fatalf("content[1].type = %q, want web_search_tool_result", got)
	}
	if got := content.Get("1.content.0.encrypted_content").String(); got != "cipher" {
		t.Fatalf("encrypted search result = %q, want cipher", got)
	}
	if got := content.Get("2.citations.0.encrypted_index").String(); got != "idx" {
		t.Fatalf("citation encrypted_index = %q, want idx", got)
	}
}

func TestCPA09ResponsesRequestAssistantPrefillBoundary(t *testing.T) {
	request := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prefill"}]}
	]}`)
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5", "claude-sonnet-4-6"} {
		out := convertOpenAIResponsesRequestToClaudeDefault(model, request, false)
		if got := gjson.GetBytes(out, "messages.#").Int(); got != 1 {
			t.Fatalf("%s message count = %d, want 1; output=%s", model, got, out)
		}
	}
	out := convertOpenAIResponsesRequestToClaudeDefault("compatible-unknown", request, false)
	if got := gjson.GetBytes(out, "messages.#").Int(); got != 2 {
		t.Fatalf("unknown model message count = %d, want 2; output=%s", got, out)
	}
}

func TestCPA09ResponsesRequestIdentityIsStableAndRequestScoped(t *testing.T) {
	requestA := []byte(`{"prompt_cache_key":"a","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	requestB := []byte(`{"prompt_cache_key":"b","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	first := gjson.GetBytes(convertOpenAIResponsesRequestToClaudeDefault("claude-test", requestA, false), "metadata.user_id").String()
	retry := gjson.GetBytes(convertOpenAIResponsesRequestToClaudeDefault("claude-test", requestA, false), "metadata.user_id").String()
	second := gjson.GetBytes(convertOpenAIResponsesRequestToClaudeDefault("claude-test", requestB, false), "metadata.user_id").String()
	if first == "" || first != retry {
		t.Fatalf("identity is not stable: first=%q retry=%q", first, retry)
	}
	if first == second {
		t.Fatalf("different request scopes share identity %q", first)
	}
}
