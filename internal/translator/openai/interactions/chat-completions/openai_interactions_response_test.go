package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestInteractionsFailedEventToOpenAIChatError(t *testing.T) {
	for _, event := range []string{
		`{"event_type":"response.failed","error":{"type":"api_error","code":"re` +
			`source_exhausted","message":"model capacity exhausted"}}`,
		`{"event_type":"interaction.failed","response":{"error":{"type":"api_er` +
			`ror","code":"resource_exhausted","message":"model capacity exhausted"}` +
			`}}`,
	} {
		var state any
		out := convertInteractionsResponseToOpenAI(
			context.Background(),
			"test-model",
			nil,
			nil,
			[]byte(event),
			&state,
		)
		if len(out) != 1 || gjson.GetBytes(out[0], "error.type").String() != "api_error" ||
			gjson.GetBytes(out[0], "error.message").String() != "model capacity exhausted" ||
			gjson.GetBytes(out[0], "error.code").String() != "resource_exhausted" {
			t.Fatalf("failed event = %q, want OpenAI error", out)
		}
		completed := convertInteractionsResponseToOpenAI(
			context.Background(), "test-model", nil, nil,
			[]byte(`{"event_type":"interaction.completed"}`), &state,
		)
		if len(completed) != 0 {
			t.Fatalf("completed after failure = %q", completed)
		}
	}
}

func TestInteractionsToolCallUsesDenseOpenAIStreamIndex(t *testing.T) {
	var state any
	convertInteractionsResponseToOpenAI(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.start","index":0,"step":{"type":"thought"}}`),
		&state,
	)
	start := convertInteractionsResponseToOpenAI(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(
			`{"event_type":"step.start","index":1,"step":{"type":"function_call",`+
				`"id":"item-1","call_id":"call-1","name":`+
				`"tool"}}`,
		),
		&state,
	)
	if len(start) != 1 {
		t.Fatalf("function start chunks = %d, want 1", len(start))
	}
	if got := gjson.GetBytes(start[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("function start index = %d, want 0", got)
	}

	delta := convertInteractionsResponseToOpenAI(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{}"}}`),
		&state,
	)
	if len(delta) != 1 {
		t.Fatalf("function delta chunks = %d, want 1", len(delta))
	}
	if got := gjson.GetBytes(delta[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("function delta index = %d, want 0", got)
	}
}

func TestOpenAIChatRequestPreservesContextAndAntigravityToolIdentity(t *testing.T) {
	body := []byte(("{\n\t\t\"model\":\"antigravity-model\",\n\t\t\"previous_response_id\":\"response-prev\"," +
		"\n\t\t\"environment\":{\"id\":\"environment-1\"},\n\t\t\"agent_config\":{\"max_total_tokens\":321,\"" +
		"mode\":\"careful\"},\n\t\t\"max_completion_tokens\":999,\n\t\t\"tool_choice\":{\"type\":\"function\"" +
		",\"function\":{\"name\":\"read_file\"}},\n\t\t\"tools\":[\n\t\t\t{\"type\":\"function\",\"function\"" +
		":{\"name\":\"read_file\"}},\n\t\t\t{\"type\":\"function\",\"function\":{\"name\":\"" +
		"external_read_file\"}},\n\t\t\t{\"type\":\"function\",\"function\":{\"name\":\"external_lookup\"}}\n" +
		"\t\t],\n\t\t\"messages\":[\n\t\t\t{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"call-1\",\"" +
		"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{}\"}}]},\n\t\t\t{\"role\":" +
		"\"tool\",\"tool_call_id\":\"call-1\",\"name\":\"external_read_file\",\"content\":\"ok\"}\n\t\t]\n\t}"))
	out := convertOpenAIRequestToInteractions("", body, false)
	checks := map[string]string{
		"previous_interaction_id":                     "response-prev",
		"environment_id":                              "environment-1",
		"agent_config.mode":                           "careful",
		"generation_config.tool_choice.function.name": "external_read_file",
		"tools.0.name":                                "external_read_file",
		"tools.1.name":                                "external_external_read_file",
		"tools.2.name":                                "external_lookup",
		"input.0.name":                                "external_read_file",
		"input.1.name":                                "external_external_read_file",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; output=%s", path, got, want, out)
		}
	}
	if got := gjson.GetBytes(out, "agent_config.max_total_tokens").Int(); got != 321 {
		t.Fatalf("explicit max_total_tokens = %d, want 321; output=%s", got, out)
	}

	mapped := convertOpenAIRequestToInteractions(
		"antigravity-model",
		[]byte(`{"max_tokens":999,"messages":[{"role":"user","content":"hello"}]}`),
		false,
	)
	if got := gjson.GetBytes(mapped, "agent_config.max_total_tokens").Int(); got != 999 {
		t.Fatalf("mapped max_total_tokens = %d, want 999; output=%s", got, mapped)
	}

	standard := convertOpenAIRequestToInteractions(
		"standard-model",
		[]byte(
			`{"max_completion_tokens":777,"temperature":0.4,"messages":[{"role":"user","content":"hello"}]}`,
		),
		false,
	)
	if got := gjson.GetBytes(standard, "generation_config.max_output_tokens").Int(); got != 777 {
		t.Fatalf("standard max_output_tokens = %d, want 777; output=%s", got, standard)
	}
	if got := gjson.GetBytes(standard, "generation_config.temperature").Float(); got != 0.4 {
		t.Fatalf("standard temperature = %v, want 0.4; output=%s", got, standard)
	}
	if gjson.GetBytes(standard, "agent_config.max_total_tokens").Exists() {
		t.Fatalf("standard request unexpectedly set max_total_tokens; output=%s", standard)
	}
}

func TestInteractionsRequestRestoresChatContextAndAntigravityToolIdentity(t *testing.T) {
	body := []byte(("{\n\t\t\"model\":\"antigravity-model\",\n\t\t\"previous_interaction_id\":\"interaction-" +
		"prev\",\n\t\t\"environment\":{\"id\":\"environment-2\"},\n\t\t\"agent_config\":{\"max_total_tokens\"" +
		":654,\"mode\":\"careful\"},\n\t\t\"generation_config\":{\"tool_choice\":{\"type\":\"function\",\"" +
		"name\":\"external_external_read_file\"}},\n\t\t\"tools\":[\n\t\t\t{\"type\":\"function\",\"name\":\"" +
		"external_read_file\"},\n\t\t\t{\"type\":\"function\",\"name\":\"external_external_read_file\"},\n\t" +
		"\t\t{\"type\":\"function\",\"name\":\"external_lookup\"}\n\t\t],\n\t\t\"input\":[\n\t\t\t{\"type\":" +
		"\"function_call\",\"id\":\"item-1\",\"call_id\":\"call-1\",\"name\":\"external_external_read_file\"," +
		"\"arguments\":{}},\n\t\t\t{\"type\":\"function_result\",\"call_id\":\"call-1\",\"name\":\"" +
		"external_external_read_file\",\"result\":\"ok\"}\n\t\t]\n\t}"))
	out := convertInteractionsRequestToOpenAI("", body, false)
	checks := map[string]string{
		"previous_response_id":                  "interaction-prev",
		"environment_id":                        "environment-2",
		"agent_config.mode":                     "careful",
		"tool_choice.name":                      "external_read_file",
		"tools.0.function.name":                 "read_file",
		"tools.1.function.name":                 "external_read_file",
		"tools.2.function.name":                 "external_lookup",
		"messages.0.tool_calls.0.function.name": "external_read_file",
		"messages.1.name":                       "external_read_file",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; output=%s", path, got, want, out)
		}
	}
	if got := gjson.GetBytes(out, "agent_config.max_total_tokens").Int(); got != 654 {
		t.Fatalf("max_total_tokens = %d, want 654; output=%s", got, out)
	}
}

func TestChatResponseRestoresAntigravityToolIdentity(t *testing.T) {
	interactions := convertOpenAIResponseToInteractionsNonStream(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`{"id":"chat-1","choices":[{"message":{"role":"assistant","tool_calls":`+
			`[{"id":"call-1","type":"function","function":{"name":"external_read_fi`+
			`le","arguments":"{}"}}]}}]}`),
		nil,
	)
	if got := gjson.GetBytes(interactions, "steps.0.name").String(); got != "external_external_read_file" {
		t.Fatalf("non-stream interaction tool name = %q; interaction=%s", got, interactions)
	}

	chat := convertInteractionsResponseToOpenAINonStream(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte((`{"interaction":{"id":"interaction-1","model":"antigravity-model","environment":{"id":"` +
			`environment-response"},"steps":[{"type":"function_call","id":"item-1","call_id":"call-1","name":` +
			`"external_external_read_file","arguments":{}}]}}`)),
		nil,
	)
	if got := gjson.GetBytes(
		chat, "choices.0.message.tool_calls.0.function.name",
	).String(); got != "external_read_file" {
		t.Fatalf("non-stream chat tool name = %q; chat=%s", got, chat)
	}
	if got := gjson.GetBytes(chat, "environment_id").String(); got != "environment-response" {
		t.Fatalf("non-stream chat environment = %q; chat=%s", got, chat)
	}
}

func TestChatStreamRestoresAntigravityToolIdentityAndEnvironment(t *testing.T) {
	var openAIState any
	interactionEvents := convertOpenAIResponseToInteractions(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte((`{"id":"chat-1","model":"antigravity-model","choices":[{"index":0,"delta":{"tool_calls":[{"` +
			`index":0,"id":"call-1","type":"function","function":{"name":"external_read_file","arguments":""}` +
			`}]}}]}`)),
		&openAIState,
	)
	var functionStart gjson.Result
	for _, event := range interactionEvents {
		payload := gjson.ParseBytes(openAIChatInteractionsPayload(event))
		if payload.Get("event_type").String() == "step.start" {
			functionStart = payload
		}
	}
	if got := functionStart.Get("step.name").String(); got != "external_external_read_file" {
		t.Fatalf("stream interaction tool name = %q; events=%q", got, interactionEvents)
	}

	var interactionsState any
	convertInteractionsResponseToOpenAI(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`{"event_type":"interaction.created","interaction":{"id":"interaction-1`+
			`","model":"antigravity-model","environment":{"id":"environment-stream"`+
			`}}}`),
		&interactionsState,
	)
	chatChunks := convertInteractionsResponseToOpenAI(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.start","index":0,"step":{"type":"function_call","i`+
			`d":"item-1","call_id":"call-1","name":"external_external_read_file"}}`),
		&interactionsState,
	)
	if len(chatChunks) != 1 {
		t.Fatalf("stream chat chunks = %d, want 1", len(chatChunks))
	}
	if got := gjson.GetBytes(
		chatChunks[0], "choices.0.delta.tool_calls.0.function.name",
	).String(); got != "external_read_file" {
		t.Fatalf("stream chat tool name = %q; chunk=%s", got, chatChunks[0])
	}
	if got := gjson.GetBytes(chatChunks[0], "environment_id").String(); got != "environment-stream" {
		t.Fatalf("stream chat environment = %q; chunk=%s", got, chatChunks[0])
	}

	terminal := convertInteractionsResponseToOpenAI(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(
			`{"event_type":"interaction.completed","interaction":{"id":"interaction-1","model":"antigravity-model"}}`,
		),
		&interactionsState,
	)
	if len(terminal) != 1 {
		t.Fatalf("stream terminal chunks = %d, want 1", len(terminal))
	}
	if got := gjson.GetBytes(terminal[0], "environment_id").String(); got != "environment-stream" {
		t.Fatalf("stream terminal environment = %q; chunk=%s", got, terminal[0])
	}
}
