package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaude_ToolStrict(t *testing.T) {
	t.Run(
		"preserves strict true on function tool", func(t *testing.T) {
			inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_a",
						"description": "Controlled tool.",
						"strict": true,
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
			result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-6", []byte(inputJSON), false)
			toolStrict := gjson.GetBytes(result, "tools.0.strict")
			if !toolStrict.Exists() {
				t.Fatalf("expected tools.0.strict to exist in Claude output: %s", result)
			}
			if !toolStrict.Bool() {
				t.Fatalf("expected tools.0.strict to be true, got %v", toolStrict.Value())
			}
		},
	)

	t.Run(
		"preserves strict true when on top level tool", func(t *testing.T) {
			inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"strict": true,
					"function": {
						"name": "tool_b",
						"description": "Controlled tool.",
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
			result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-6", []byte(inputJSON), false)
			toolStrict := gjson.GetBytes(result, "tools.0.strict")
			if !toolStrict.Exists() || !toolStrict.Bool() {
				t.Fatalf("expected tools.0.strict to be true, got %s", result)
			}
		},
	)

	t.Run(
		"preserves strict false on function tool", func(t *testing.T) {
			inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_c",
						"description": "Controlled tool.",
						"strict": false,
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
			result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-6", []byte(inputJSON), false)
			toolStrict := gjson.GetBytes(result, "tools.0.strict")
			if !toolStrict.Exists() {
				t.Fatalf("expected tools.0.strict to exist in Claude output: %s", result)
			}
			if toolStrict.Bool() {
				t.Fatalf("expected tools.0.strict to be false, got %v", toolStrict.Value())
			}
		},
	)

	t.Run(
		"omits strict when not provided", func(t *testing.T) {
			inputJSON := `{
			"model": "claude-sonnet-4-6",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_d",
						"description": "Controlled tool.",
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
			result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-6", []byte(inputJSON), false)
			if gjson.GetBytes(result, "tools.0.strict").Exists() {
				t.Fatalf("expected tools.0.strict to be omitted when not provided, got %s", result)
			}
		},
	)
}
