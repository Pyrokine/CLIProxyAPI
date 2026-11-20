package responses

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_OmitsToolSettingsWithoutTools(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{
			"empty tools", []byte(`{
			"input": [{"role":"user","content":"say ok"}],
			"tools": [],
			"tool_choice": "auto",
			"parallel_tool_calls": false
		}`),
		},
		{
			"unconvertible tools", []byte(`{
			"tools": [{"type":"unsupported"}],
			"tool_choice": "auto",
			"parallel_tool_calls": false
		}`),
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				output := convertResponsesRequestToChatCompletions("grok-4.5", test.raw, false)
				if !json.Valid(output) {
					t.Fatalf("conversion did not return a JSON request: %s", output)
				}
				for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
					if gjson.GetBytes(output, field).Exists() {
						t.Fatalf("%s should be omitted without tools: %s", field, output)
					}
				}
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesParallelToolCallsWithTools(t *testing.T) {
	raw := []byte(`{
		"tools": [
			{
				"type": "function",
				"name": "run_command",
				"parameters": {"type": "object"}
			}
		],
		"parallel_tool_calls": false
	}`)
	output := convertResponsesRequestToChatCompletions("grok-4.5", raw, false)
	if !json.Valid(output) {
		t.Fatalf("conversion did not return a JSON request: %s", output)
	}
	if parallel := gjson.GetBytes(output, "parallel_tool_calls"); !parallel.Exists() || parallel.Bool() {
		t.Fatalf("parallel_tool_calls = %v, want false: %s", parallel.Value(), output)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesStructuredToolChoice(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"role":"user","content":"Run command."}
		],
		"tools": [
			{
				"type": "function",
				"name": "run_command",
				"parameters": {"type": "object"}
			}
		],
		"tool_choice": {
			"type": "function",
			"function": {
				"name": "run_command"
			}
		}
	}`)
	output := convertResponsesRequestToChatCompletions("gpt-5.4", raw, false)
	if !json.Valid(output) || gjson.GetBytes(output, "tool_choice.type").String() != "function" ||
		gjson.GetBytes(output, "tool_choice.function.name").String() != "run_command" {
		t.Fatalf("structured tool_choice changed: %s", output)
	}
}
