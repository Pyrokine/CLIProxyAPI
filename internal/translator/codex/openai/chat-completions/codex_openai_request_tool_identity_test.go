package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToCodexCustomToolNameRoundTrip(t *testing.T) {
	longName := "a_very_long_custom_tool_name_that_exceeds_sixty_four_characters_limit_test"
	input := []byte(`{
		"messages":[
			{"role":"user","content":"Apply the patch."},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_custom_long","type":"function","function":{"name":"` + longName + `","arguments":"patch"}}
			]},
			{"role":"tool","tool_call_id":"call_custom_long","content":"patched"}
		],
		"tools":[{"type":"custom","name":"` + longName + `","description":"Apply a patch."}],
		"tool_choice":{"type":"custom","name":"` + longName + `"}
	}`)

	out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 3 {
		t.Fatalf("input items = %d, want 3; output=%s", len(items), out)
	}
	if got := items[1].Get("type").String(); got != "custom_tool_call" {
		t.Fatalf("call type = %q, want custom_tool_call; item=%s", got, items[1].Raw)
	}
	shortName := items[1].Get("name").String()
	if shortName == longName || len(shortName) > 64 {
		t.Fatalf("short name = %q, original=%q", shortName, longName)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != shortName {
		t.Fatalf("declaration name = %q, want %q; output=%s", got, shortName, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "custom" {
		t.Fatalf("tool choice type = %q, want custom; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != shortName {
		t.Fatalf("tool choice name = %q, want %q; output=%s", got, shortName, out)
	}
	if got := items[2].Get("type").String(); got != "custom_tool_call_output" {
		t.Fatalf("output type = %q, want custom_tool_call_output; item=%s", got, items[2].Raw)
	}
	if got := buildReverseMapFromOriginalOpenAI(input)[shortName]; got != longName {
		t.Fatalf("reverse name = %q, want %q", got, longName)
	}

	response := []byte(`{
		"type":"response.completed",
		"response":{
			"status":"completed",
			"output":[{"type":"custom_tool_call","call_id":"call_custom_long","name":"` + shortName + `","input":"patch"}]
		}
	}`)
	chatResponse := ConvertCodexResponseToOpenAINonStream(
		context.Background(),
		"gpt-5.6-sol",
		input,
		nil,
		response,
		nil,
	)
	if got := gjson.GetBytes(chatResponse, "choices.0.message.tool_calls.0.function.name").String(); got != longName {
		t.Fatalf("restored response name = %q, want %q; response=%s", got, longName, chatResponse)
	}
}

func TestConvertOpenAIRequestToCodexToolFamilyResolution(t *testing.T) {
	t.Run(
		"normalized custom history", func(t *testing.T) {
			input := []byte(`{
			"messages":[
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"patch"}}
				]},
				{"role":"tool","tool_call_id":"call_patch","content":"patched"}
			],
			"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch."}],
			"tool_choice":{"type":"function","function":{"name":"apply_patch"}}
		}`)

			out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
			items := gjson.GetBytes(out, "input").Array()
			if len(items) != 2 {
				t.Fatalf("input items = %d, want 2; output=%s", len(items), out)
			}
			if got := items[0].Get("type").String(); got != "custom_tool_call" {
				t.Fatalf("call type = %q, want custom_tool_call; item=%s", got, items[0].Raw)
			}
			if got := items[1].Get("type").String(); got != "custom_tool_call_output" {
				t.Fatalf("output type = %q, want custom_tool_call_output; item=%s", got, items[1].Raw)
			}
			if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "custom" {
				t.Fatalf("tool choice type = %q, want custom; output=%s", got, out)
			}
		},
	)

	t.Run(
		"same name defaults to function", func(t *testing.T) {
			input := []byte(`{
			"messages":[
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_shared","type":"function","function":{"name":"shared_tool","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_shared","content":"done"}
			],
			"tools":[
				{"type":"custom","name":"shared_tool","description":"Custom tool."},
				{"type":"function","function":{"name":"shared_tool","parameters":{"type":"object"}}}
			],
			"tool_choice":{"type":"function","function":{"name":"shared_tool"}}
		}`)

			out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
			items := gjson.GetBytes(out, "input").Array()
			if len(items) != 2 {
				t.Fatalf("input items = %d, want 2; output=%s", len(items), out)
			}
			if got := items[0].Get("type").String(); got != "function_call" {
				t.Fatalf("call type = %q, want function_call; item=%s", got, items[0].Raw)
			}
			if got := items[1].Get("type").String(); got != "function_call_output" {
				t.Fatalf("output type = %q, want function_call_output; item=%s", got, items[1].Raw)
			}
			if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "function" {
				t.Fatalf("tool choice type = %q, want function; output=%s", got, out)
			}
			if first, second := gjson.GetBytes(out, "tools.0.name").String(), gjson.GetBytes(
				out, "tools.1.name",
			).String(); first != second {
				t.Fatalf("same-name declarations became %q and %q", first, second)
			}
		},
	)

	t.Run(
		"short name collision preserves function", func(t *testing.T) {
			customName := "a_very_long_custom_tool_name_that_exceeds_sixty_four_characters_limit_test"
			functionName := shortenNameIfNeeded(customName)
			input := []byte(`{
			"messages":[
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_function","type":"function","function":{"name":"` + functionName + `","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_function","content":"done"}
			],
			"tools":[
				{"type":"custom","name":"` + customName + `","description":"Custom tool."},
				{"type":"function","function":{"name":"` + functionName + `","parameters":{"type":"object"}}}
			],
			"tool_choice":{"type":"function","function":{"name":"` + functionName + `"}}
		}`)

			out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
			items := gjson.GetBytes(out, "input").Array()
			if got := items[0].Get("type").String(); got != "function_call" {
				t.Fatalf("call type = %q, want function_call; item=%s", got, items[0].Raw)
			}
			functionDeclarationName := gjson.GetBytes(out, "tools.1.name").String()
			if customDeclarationName := gjson.GetBytes(
				out, "tools.0.name",
			).String(); customDeclarationName == functionDeclarationName {
				t.Fatalf("colliding declarations share translated name %q", functionDeclarationName)
			}
			if got := gjson.GetBytes(out, "tool_choice.name").String(); got != functionDeclarationName {
				t.Fatalf("tool choice name = %q, want %q; output=%s", got, functionDeclarationName, out)
			}
		},
	)
}

func TestConvertOpenAIRequestToCodexMixedToolFamilies(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"user","content":"Run both tools."},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_function","type":"function","function":{"name":"lookup","arguments":"{}"}},
				{"id":"call_custom","type":"function","function":{"name":"apply_patch","arguments":"patch"}}
			]},
			{"role":"tool","tool_call_id":"call_custom","content":"patched"},
			{"role":"tool","tool_call_id":"call_function","content":"found"}
		],
		"tools":[
			{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},
			{"type":"custom","name":"apply_patch","description":"Apply a patch."}
		]
	}`)

	out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
	items := gjson.GetBytes(out, "input").Array()
	expectedTypes := []string{
		"message", "function_call", "custom_tool_call", "custom_tool_call_output", "function_call_output",
	}
	if len(items) != len(expectedTypes) {
		t.Fatalf("input items = %d, want %d; output=%s", len(items), len(expectedTypes), out)
	}
	for index, expectedType := range expectedTypes {
		if got := items[index].Get("type").String(); got != expectedType {
			t.Fatalf("item %d type = %q, want %q; item=%s", index, got, expectedType, items[index].Raw)
		}
	}
}

func TestConvertOpenAIRequestToCodexToolCallIDLifecycle(t *testing.T) {
	t.Run(
		"reused across rounds", func(t *testing.T) {
			input := []byte(`{
			"messages":[
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_reused","type":"function","function":{"name":"lookup","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_reused","content":"found"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_reused","type":"custom","custom":{"name":"apply_patch","input":"patch"}}
				]},
				{"role":"tool","tool_call_id":"call_reused","content":"patched"}
			]
		}`)

			items := gjson.GetBytes(ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true), "input").Array()
			expectedTypes := []string{
				"function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
			}
			if len(items) != len(expectedTypes) {
				t.Fatalf(
					"input items = %d, want %d; items=%s", len(items), len(expectedTypes),
					gjson.GetBytes(ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true), "input").Raw,
				)
			}
			for index, expectedType := range expectedTypes {
				if got := items[index].Get("type").String(); got != expectedType {
					t.Fatalf("item %d type = %q, want %q", index, got, expectedType)
				}
			}
		},
	)

	t.Run(
		"missing IDs pair in order", func(t *testing.T) {
			input := []byte(`{
			"messages":[
				{"role":"tool","content":"orphan"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_explicit","type":"function","function":{"name":"lookup","arguments":"{}"}},
					{"type":"custom","custom":{"name":"apply_patch","input":"patch"}}
				]},
				{"role":"tool","content":"found"},
				{"role":"tool","content":"patched"}
			]
		}`)

			out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
			items := gjson.GetBytes(out, "input").Array()
			if len(items) != 4 {
				t.Fatalf("input items = %d, want 4; output=%s", len(items), out)
			}
			if got := items[2].Get("call_id").String(); got != "call_explicit" {
				t.Fatalf("first output call_id = %q, want call_explicit", got)
			}
			syntheticCallID := items[1].Get("call_id").String()
			if syntheticCallID == "" || items[3].Get("call_id").String() != syntheticCallID {
				t.Fatalf("synthetic call pair mismatch: call=%s output=%s", items[1].Raw, items[3].Raw)
			}
		},
	)

	t.Run(
		"ambiguous duplicate IDs are dropped", func(t *testing.T) {
			input := []byte(`{
			"messages":[
				{"role":"user","content":"Run both tools."},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_duplicate","type":"function","function":{"name":"lookup","arguments":"{}"}},
					{"id":"call_duplicate","type":"custom","custom":{"name":"apply_patch","input":"patch"}}
				]},
				{"role":"tool","tool_call_id":"call_duplicate","content":"first"},
				{"role":"tool","tool_call_id":"call_duplicate","content":"second"}
			]
		}`)

			items := gjson.GetBytes(ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true), "input").Array()
			if len(items) != 1 || items[0].Get("role").String() != "user" {
				t.Fatalf(
					"ambiguous calls were retained: %s",
					gjson.GetBytes(ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true), "input").Raw,
				)
			}
		},
	)

	t.Run(
		"orphan and duplicate outputs are dropped", func(t *testing.T) {
			input := []byte(`{
			"messages":[
				{"role":"tool","tool_call_id":"call_orphan","content":"orphan"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_custom","type":"function","function":{"name":"apply_patch","arguments":"patch"}}
				]},
				{"role":"tool","tool_call_id":"call_custom","content":"patched"},
				{"role":"tool","tool_call_id":"call_custom","content":"duplicate"}
			],
			"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch."}]
		}`)

			out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", input, true)
			items := gjson.GetBytes(out, "input").Array()
			if len(items) != 2 {
				t.Fatalf("input items = %d, want 2; output=%s", len(items), out)
			}
			if got := items[1].Get("output").String(); got != "patched" {
				t.Fatalf("retained output = %q, want patched; item=%s", got, items[1].Raw)
			}
		},
	)
}

func TestConvertOpenAIRequestToCodexCustomResponseFollowUpRoundTrip(t *testing.T) {
	originalRequest := []byte(`{
		"messages":[{"role":"user","content":"Apply the patch."}],
		"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch."}]
	}`)
	upstreamResponse := []byte(`{
		"type":"response.completed",
		"response":{
			"status":"completed",
			"output":[{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"patch"}]
		}
	}`)

	chatResponse := ConvertCodexResponseToOpenAINonStream(
		context.Background(),
		"gpt-5.6-sol",
		originalRequest,
		nil,
		upstreamResponse,
		nil,
	)
	assistantMessage := gjson.GetBytes(chatResponse, "choices.0.message")
	if got := assistantMessage.Get("tool_calls.0.type").String(); got != "function" {
		t.Fatalf("normalized response type = %q, want function; response=%s", got, chatResponse)
	}
	if got := assistantMessage.Get("tool_calls.0.function.arguments").String(); got != "patch" {
		t.Fatalf("normalized arguments = %q, want patch; response=%s", got, chatResponse)
	}

	followUpRequest := []byte(`{
		"messages":[
			{"role":"user","content":"Apply the patch."},
			` + assistantMessage.Raw + `,
			{"role":"tool","tool_call_id":"call_patch","content":"patched"}
		],
		"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch."}]
	}`)
	out := ConvertOpenAIRequestToCodex("gpt-5.6-sol", followUpRequest, true)
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 3 {
		t.Fatalf("input items = %d, want 3; output=%s", len(items), out)
	}
	if got := items[1].Get("type").String(); got != "custom_tool_call" {
		t.Fatalf("round-trip call type = %q, want custom_tool_call; item=%s", got, items[1].Raw)
	}
	if got := items[2].Get("type").String(); got != "custom_tool_call_output" {
		t.Fatalf("round-trip output type = %q, want custom_tool_call_output; item=%s", got, items[2].Raw)
	}
}
