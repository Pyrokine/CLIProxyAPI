package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaudeGroupsConsecutiveParallelToolResults(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"user","content":"Use both tools."},
			{
				"role":"assistant",
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"tool_a","arguments":"{}"}},
					{"id":"call_2","type":"function","function":{"name":"tool_b","arguments":"{}"}}
				]
			},
			{"role":"tool","tool_call_id":"call_1","content":"one"},
			{"role":"tool","tool_call_id":"call_2","content":"two"},
			{"role":"assistant","content":"Done."}
		]
	}`)

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-5", inputJSON, false)
	messages := gjson.GetBytes(result, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want 4. Output: %s", len(messages), result)
	}
	if got := messages[2].Get("role").String(); got != "user" {
		t.Fatalf("grouped tool result role = %q, want user", got)
	}
	toolResults := messages[2].Get("content").Array()
	if len(toolResults) != 2 {
		t.Fatalf("grouped tool result count = %d, want 2. Output: %s", len(toolResults), result)
	}
	wantIDs := []string{"call_1", "call_2"}
	wantContent := []string{"one", "two"}
	for index := range wantIDs {
		if got := toolResults[index].Get("type").String(); got != "tool_result" {
			t.Fatalf("tool result %d type = %q, want tool_result", index, got)
		}
		if got := toolResults[index].Get("tool_use_id").String(); got != wantIDs[index] {
			t.Fatalf("tool result %d ID = %q, want %q", index, got, wantIDs[index])
		}
		if got := toolResults[index].Get("content").String(); got != wantContent[index] {
			t.Fatalf("tool result %d content = %q, want %q", index, got, wantContent[index])
		}
	}
	if got := messages[3].Get("content.0.text").String(); got != "Done." {
		t.Fatalf("following assistant text = %q, want Done.", got)
	}
}

func TestConvertOpenAIRequestToClaudeGroupsToolResultContentArrays(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{
				"role":"assistant",
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"first","arguments":"{}"}},
					{"id":"call_2","type":"function","function":{"name":"second","arguments":"{}"}}
				]
			},
			{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"first result"}]},
			{"role":"tool","tool_call_id":"call_2","content":[{"type":"image_url","image_url":{"url":"https://example.com/result.png"}}]}
		]
	}`)

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-5", inputJSON, false)
	messages := gjson.GetBytes(result, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want assistant and grouped user. Output: %s", len(messages), result)
	}
	toolResults := messages[1].Get("content").Array()
	if len(toolResults) != 2 {
		t.Fatalf("grouped tool result count = %d, want 2. Output: %s", len(toolResults), result)
	}
	if got := toolResults[0].Get("content.0.text").String(); got != "first result" {
		t.Fatalf("first tool result text = %q, want first result", got)
	}
	if got := toolResults[1].Get("content.0.type").String(); got != "image" {
		t.Fatalf("second tool result content type = %q, want image", got)
	}
	if got := toolResults[1].Get("content.0.source.url").String(); got != "https://example.com/result.png" {
		t.Fatalf("second tool result URL = %q, want result image URL", got)
	}
}

func TestConvertOpenAIRequestToClaudeKeepsToolResultTurnBoundaries(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"first","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"first result"},
			{"role":"user","content":"Use that result."},
			{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"second","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_2","content":"second result"}
		]
	}`)

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-5", inputJSON, false)
	messages := gjson.GetBytes(result, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want assistant, user, assistant, user. Output: %s", len(messages), result)
	}
	wantRoles := []string{"assistant", "user", "assistant", "user"}
	for index, wantRole := range wantRoles {
		if got := messages[index].Get("role").String(); got != wantRole {
			t.Fatalf("message %d role = %q, want %q", index, got, wantRole)
		}
	}
	userContent := messages[1].Get("content").Array()
	if len(userContent) != 2 {
		t.Fatalf("first user turn content count = %d, want tool result and text", len(userContent))
	}
	if got := userContent[0].Get("type").String(); got != "tool_result" {
		t.Fatalf("first user content type = %q, want tool_result", got)
	}
	if got := userContent[1].Get("text").String(); got != "Use that result." {
		t.Fatalf("second user content text = %q, want follow-up", got)
	}
	if got := messages[3].Get("content.#").Int(); got != 1 {
		t.Fatalf("second tool result turn content count = %d, want 1", got)
	}
}
