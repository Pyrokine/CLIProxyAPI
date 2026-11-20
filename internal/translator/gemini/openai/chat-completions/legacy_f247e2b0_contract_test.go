package chat_completions

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToGemini_ToolStrictMapsToValidatedMode(t *testing.T) {
	tests := []struct {
		name         string
		toolChoice   string // empty means absent
		expectedMode string
		allowedNames []string
	}{
		{
			name:         "absent tool_choice maps to VALIDATED",
			toolChoice:   "",
			expectedMode: "VALIDATED",
		},
		{
			name:         "explicit auto tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": "auto",`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "null tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": null,`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "required tool_choice maps to ANY",
			toolChoice:   `"tool_choice": "required",`,
			expectedMode: "ANY",
		},
		{
			name:         "none tool_choice maps to NONE",
			toolChoice:   `"tool_choice": "none",`,
			expectedMode: "NONE",
		},
		{
			name:         "specific function tool_choice maps to ANY with allowedFunctionNames",
			toolChoice:   `"tool_choice": {"type": "function", "function": {"name": "tool_a"}},`,
			expectedMode: "ANY",
			allowedNames: []string{"tool_a"},
		},
		{
			name: "allowed_tools with auto mode and strict tool maps to VALIDATED",
			toolChoice: `"tool_choice": {"type": "allowed_tools", "allowed_tools": {"mode": "au` +
				`to", "tools": [{"type": "function", "function": {"name": "tool_a"}}]}}` +
				`,`,
			expectedMode: "VALIDATED",
		},
		{
			name: "allowed_tools with required mode and strict tool maps to ANY",
			toolChoice: `"tool_choice": {"type": "allowed_tools", "allowed_tools": {"mode": "required", "` +
				`tools": [{"type": "function", "function": {"name": "tool_a"}}]}},`,
			expectedMode: "ANY",
			allowedNames: []string{"tool_a"},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				inputJSON := fmt.Sprintf(
					`{
				"model": "gemini-3.1-pro-high",
				"messages": [{"role": "user", "content": "hi"}],
				%s
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
			}`, tt.toolChoice,
				)
				result := convertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
				if gjson.GetBytes(result, "tools.0.functionDeclarations.0.strict").Exists() {
					t.Fatalf(
						"tools.0.functionDeclarations.0.strict should be removed from function declaration: %s", result,
					)
				}
				mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
				if mode != tt.expectedMode {
					t.Fatalf(
						"expected toolConfig.functionCallingConfig.mode = %q, got %q. Output: %s", tt.expectedMode,
						mode, result,
					)
				}
				if len(tt.allowedNames) > 0 {
					allowed := gjson.GetBytes(result, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
					if len(allowed) != len(tt.allowedNames) {
						t.Fatalf("expected %d allowedFunctionNames, got %d", len(tt.allowedNames), len(allowed))
					}
					for i, name := range tt.allowedNames {
						if allowed[i].String() != name {
							t.Fatalf("allowedFunctionNames[%d] = %q, want %q", i, allowed[i].String(), name)
						}
					}
				}
			},
		)
	}

	t.Run(
		"mixed tools where one is strict maps to VALIDATED", func(t *testing.T) {
			inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_a",
						"description": "Loose tool.",
						"strict": false,
						"parameters": {"type": "object", "properties": {}}
					}
				},
				{
					"type": "function",
					"function": {
						"name": "tool_b",
						"description": "Strict tool.",
						"strict": true,
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
			result := convertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
			mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
			if mode != "VALIDATED" {
				t.Fatalf("expected mode = 'VALIDATED' for mixed tools, got %q", mode)
			}
		},
	)

	t.Run(
		"non-strict tools omit toolConfig mode", func(t *testing.T) {
			inputJSON := `{
			"model": "gemini-3.1-pro-high",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "tool_a",
						"description": "Loose tool.",
						"strict": false,
						"parameters": {"type": "object", "properties": {}}
					}
				},
				{
					"type": "function",
					"function": {
						"name": "tool_b",
						"description": "Unspecified tool.",
						"parameters": {"type": "object", "properties": {}}
					}
				}
			]
		}`
			result := convertOpenAIRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
			if gjson.GetBytes(result, "toolConfig").Exists() {
				t.Fatalf("expected toolConfig not to be set when no strict tools and no tool_choice, got: %s", result)
			}
		},
	)
}
