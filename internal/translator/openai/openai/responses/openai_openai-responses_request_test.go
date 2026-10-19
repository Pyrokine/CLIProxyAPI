package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesToolDescriptionPreservesGrammarWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "top level", body: `{"description":" \nOriginal patch grammar:\nstart: patch\r\n\n "}`, want: " \nOriginal patch grammar:\nstart: patch\r\n\n "},
		{name: "nested", body: `{"function":{"description":" \nstart: patch\r\n\n "}}`, want: " \nstart: patch\r\n\n "},
		{name: "top level wins", body: `{"description":"  first\n","function":{"description":"second"}}`, want: "  first\n"},
		{name: "blank top level", body: `{"description":" \r\n","function":{"description":"  nested\n "}}`, want: "  nested\n "},
		{name: "both blank", body: `{"description":" \r\n","function":{"description":"\t "}}`},
		{name: "missing", body: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesToolDescription(gjson.Parse(tc.body)); got != tc.want {
				t.Fatalf("description = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsAggregatesContinuousFunctionCalls(t *testing.T) {
	inputJSON := []byte(`{
		"input": [
			{"type":"message","role":"user","content":"run tools"},
			{"type":"function_call","call_id":"call_1","name":"first","arguments":"{\"a\":1}"},
			{"type":"function_call","call_id":"call_2","name":"second","arguments":"{\"b\":2}"},
			{"type":"function_call_output","call_id":"call_1","output":"one"},
			{"type":"message","role":"user","content":"after tools"},
			{"type":"function_call_output","call_id":"call_2","output":"two"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-4.1", inputJSON, false)
	messages := gjson.GetBytes(output, "messages").Array()
	if len(messages) != 5 {
		t.Fatalf("message count = %d, want 5; output: %s", len(messages), output)
	}

	assistant := messages[1]
	if assistant.Get("role").String() != "assistant" {
		t.Fatalf("messages[1].role = %q, want assistant", assistant.Get("role").String())
	}
	if got := len(assistant.Get("tool_calls").Array()); got != 2 {
		t.Fatalf("tool_calls count = %d, want 2; output: %s", got, output)
	}
	if messages[2].Get("role").String() != "tool" || messages[3].Get("role").String() != "tool" {
		t.Fatalf("tool outputs must immediately follow assistant tool_calls; output: %s", output)
	}
	if messages[4].Get("content").String() != "after tools" {
		t.Fatalf("deferred user message not emitted after tool outputs; output: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsMatchesMissingOutputBeforeFutureExplicitID(t *testing.T) {
	inputJSON := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call-a","name":"same","arguments":"{}"},
			{"type":"function_call","name":"same","arguments":"{}"},
			{"type":"function_call_output","name":"same","output":"generated-result"},
			{"type":"function_call_output","call_id":"call-a","output":"explicit-result"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-4.1", inputJSON, false)
	messages := gjson.GetBytes(output, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("message count = %d, want 3; output: %s", len(messages), output)
	}
	toolCalls := messages[0].Get("tool_calls").Array()
	if got := toolCalls[0].Get("id").String(); got != "call-a" {
		t.Fatalf("explicit tool call ID = %q, want call-a; output: %s", got, output)
	}
	const generated = "call_responses_0000000000000001"
	if got := toolCalls[1].Get("id").String(); got != generated {
		t.Fatalf("generated tool call ID = %q, want %q; output: %s", got, generated, output)
	}
	if got := messages[1].Get("tool_call_id").String(); got != generated {
		t.Fatalf("named output ID = %q, want %q; output: %s", got, generated, output)
	}
	if got := messages[2].Get("tool_call_id").String(); got != "call-a" {
		t.Fatalf("future explicit output ID = %q, want call-a; output: %s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsPreservesStringServiceTier(t *testing.T) {
	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions(
		"gpt-4.1",
		[]byte(`{"input":"hello","service_tier":" priority "}`),
		false,
	)

	if got := gjson.GetBytes(output, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsIgnoresNonStringServiceTier(t *testing.T) {
	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions(
		"gpt-4.1",
		[]byte(`{"input":"hello","service_tier":true}`),
		false,
	)

	if gjson.GetBytes(output, "service_tier").Exists() {
		t.Fatalf("service_tier should be omitted; output=%s", output)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsMapsTextFormat(t *testing.T) {
	tests := []struct {
		name     string
		request  string
		wantType string
	}{
		{
			name:     "text",
			request:  `{"text":{"format":{"type":"text"}}}`,
			wantType: "text",
		},
		{
			name:     "json object",
			request:  `{"text":{"format":{"type":"json_object"}}}`,
			wantType: "json_object",
		},
		{
			name: "json schema",
			request: `{
				"text":{"format":{
					"type":"json_schema",
					"name":"answer",
					"description":"Structured answer",
					"strict":false,
					"schema":{
						"type":"object",
						"properties":{"ok":{"type":"boolean"}},
						"required":["ok"],
						"additionalProperties":false
					}
				}}
			}`,
			wantType: "json_schema",
		},
		{
			name:    "unsupported",
			request: `{"text":{"format":{"type":"yaml"}}}`,
		},
		{
			name:    "missing",
			request: `{"input":"Return plain text."}`,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions(
					"gpt-4.1",
					[]byte(test.request),
					false,
				)
				responseFormat := gjson.GetBytes(output, "response_format")
				if test.wantType == "" {
					if responseFormat.Exists() {
						t.Fatalf("response_format should be omitted; output=%s", output)
					}
					return
				}
				if got := responseFormat.Get("type").String(); got != test.wantType {
					t.Fatalf("response_format.type = %q, want %q; output=%s", got, test.wantType, output)
				}
				if test.wantType != "json_schema" {
					if responseFormat.Get("json_schema").Exists() {
						t.Fatalf("response_format.json_schema should be omitted; output=%s", output)
					}
					return
				}
				if got := responseFormat.Get("json_schema.name").String(); got != "answer" {
					t.Fatalf("json_schema.name = %q, want answer; output=%s", got, output)
				}
				if got := responseFormat.Get("json_schema.description").String(); got != "Structured answer" {
					t.Fatalf("json_schema.description = %q, want Structured answer; output=%s", got, output)
				}
				if strict := responseFormat.Get("json_schema.strict"); !strict.Exists() || strict.Bool() {
					t.Fatalf("json_schema.strict = %v, want false; output=%s", strict.Value(), output)
				}
				if got := responseFormat.Get("json_schema.schema.properties.ok.type").String(); got != "boolean" {
					t.Fatalf("schema ok type = %q, want boolean; output=%s", got, output)
				}
				if additional := responseFormat.Get("json_schema.schema.additionalProperties"); !additional.Exists() || additional.Bool() {
					t.Fatalf("schema additionalProperties = %v, want false; output=%s", additional.Value(), output)
				}
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsPreservesAssistantContentWithToolCalls(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"inspect the next step"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Step 3 completed; continue to step 4."}]},
			{"type":"function_call","call_id":"call_4","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_4","output":"ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k3", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if got := len(messages); got != 2 {
		t.Fatalf("messages count = %d, want 2; output=%s", got, out)
	}
	assistant := messages[0]
	if got := assistant.Get("content.0.text").String(); got != "Step 3 completed; continue to step 4." {
		t.Fatalf("assistant content = %q, want preserved text; output=%s", got, out)
	}
	if got := assistant.Get("reasoning_content").String(); got != "inspect the next step" {
		t.Fatalf("assistant reasoning_content = %q, want inspect the next step; output=%s", got, out)
	}
	if got := assistant.Get("tool_calls.0.id").String(); got != "call_4" {
		t.Fatalf("assistant tool call ID = %q, want call_4; output=%s", got, out)
	}
	if got := messages[1].Get("tool_call_id").String(); got != "call_4" {
		t.Fatalf("tool output call ID = %q, want call_4; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsDoesNotMergeToolCallsAcrossUserMessage(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},
			{"type":"function_call","call_id":"call_next","name":"exec_command","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_next","output":"ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k3", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if got := len(messages); got != 4 {
		t.Fatalf("messages count = %d, want 4; output=%s", got, out)
	}
	if messages[0].Get("tool_calls").Exists() {
		t.Fatalf("messages.0 unexpectedly contains tool calls; output=%s", out)
	}
	if got := messages[1].Get("role").String(); got != "user" {
		t.Fatalf("messages.1 role = %q, want user; output=%s", got, out)
	}
	if got := messages[2].Get("tool_calls.0.id").String(); got != "call_next" {
		t.Fatalf("messages.2 tool call ID = %q, want call_next; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsMergesDistinctReasoningWithinAssistantTurn(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"reasoning","summary":[{"type":"summary_text","text":"first"}]},
			{"type":"message","role":"assistant","reasoning_content":"first","content":[{"type":"output_text","text":"working"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"second"}]},
			{"type":"function_call","call_id":"call_reasoning","name":"exec_command","arguments":"{}"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k3", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if got := len(messages); got != 1 {
		t.Fatalf("messages count = %d, want 1; output=%s", got, out)
	}
	if got := messages[0].Get("reasoning_content").String(); got != "first\n\nsecond" {
		t.Fatalf("reasoning_content = %q, want %q; output=%s", got, "first\n\nsecond", out)
	}
	if got := messages[0].Get("tool_calls.0.id").String(); got != "call_reasoning" {
		t.Fatalf("tool call ID = %q, want call_reasoning; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletionsReplacesUnavailableReasoningWithinAssistantTurn(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"assistant","reasoning_content":"real reasoning","content":[{"type":"output_text","text":"working"}]},
			{"type":"function_call","call_id":"call_real_reasoning","name":"exec_command","arguments":"{}"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k3", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if got := len(messages); got != 1 {
		t.Fatalf("messages count = %d, want 1; output=%s", got, out)
	}
	if got := messages[0].Get("reasoning_content").String(); got != "real reasoning" {
		t.Fatalf("reasoning_content = %q, want real reasoning; output=%s", got, out)
	}
}
