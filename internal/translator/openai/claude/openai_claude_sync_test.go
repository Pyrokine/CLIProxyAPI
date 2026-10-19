package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeToOpenAIToolResultNamesAndOrdering(t *testing.T) {
	input := []byte(`{
		"messages": [
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"fetch","input":{"q":"news"}}]},
			{"role":"user","content":"interleaved"},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}
		]
	}`)
	result := convertClaudeRequestToOpenAIDefault("test-model", input, false)
	messages := gjson.GetBytes(result, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("messages = %s, want assistant + tool + user", gjson.GetBytes(result, "messages").Raw)
	}
	if got := messages[0].Get("tool_calls.0.function.name").String(); got != "fetch" {
		t.Fatalf("tool call name = %q", got)
	}
	if got := messages[1].Get("role").String(); got != "tool" {
		t.Fatalf("message after tool call has role %q", got)
	}
	if got := messages[1].Get("name").String(); got != "fetch" {
		t.Fatalf("tool result name = %q", got)
	}
	if got := messages[2].Get("content").String(); got != "interleaved" {
		t.Fatalf("interleaved user content = %q", got)
	}
}

func TestClaudeToOpenAIToolChoiceDoesNotEnableUnknownTools(t *testing.T) {
	cases := []struct {
		name     string
		choice   string
		want     string
		parallel string
	}{
		{"auto", `{"type":"auto","disable_parallel_tool_use":true}`, "auto", "false"},
		{"any", `{"type":"any"}`, "required", ""},
		{"none", `{"type":"none"}`, "none", ""},
		{"string", `"none"`, "none", ""},
		{"unsupported", `{"type":"invalid"}`, "none", ""},
		{"missing named tool", `{"type":"tool"}`, "none", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(`{"messages":[{"role":"user","content":"hello"}],"tool_choice":` + test.choice + `}`)
			result := convertClaudeRequestToOpenAIDefault("test-model", input, false)
			if got := gjson.GetBytes(result, "tool_choice").String(); got != test.want {
				t.Fatalf("tool_choice = %q, want %q in %s", got, test.want, result)
			}
			if got := gjson.GetBytes(result, "parallel_tool_calls").Raw; got != test.parallel {
				t.Fatalf("parallel_tool_calls = %q, want %q", got, test.parallel)
			}
		})
	}
}
