package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

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
