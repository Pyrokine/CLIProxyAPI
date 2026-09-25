package responses

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestInteractionsFunctionCallPreservesItemAndCallIDs(t *testing.T) {
	raw := []byte(`{"id":"response-1","model":"test","steps":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":{}}]}`)
	out := convertInteractionsResponseToOpenAIResponsesNonStream(
		context.Background(), "test", nil, nil, raw, nil,
	)
	if got := gjson.GetBytes(out, "output.0.id").String(); got != "item-1" {
		t.Fatalf("output item id = %q, want item-1", got)
	}
	if got := gjson.GetBytes(out, "output.0.call_id").String(); got != "call-1" {
		t.Fatalf("output call id = %q, want call-1", got)
	}
	if got := gjson.GetBytes(out, "output.0.status").String(); got != "completed" {
		t.Fatalf("output status = %q, want completed", got)
	}
}

func TestResponsesFunctionCallPreservesItemAndCallIDs(t *testing.T) {
	item := gjson.Parse(`{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":"{}"}`)
	out := responsesFunctionCallToInteractions(item, false)
	if got := gjson.GetBytes(out, "id").String(); got != "item-1" {
		t.Fatalf("interaction item id = %q, want item-1", got)
	}
	if got := gjson.GetBytes(out, "call_id").String(); got != "call-1" {
		t.Fatalf("interaction call id = %q, want call-1", got)
	}
}

func TestInteractionsFunctionCallStreamEmitsArgumentsDoneAndCompletedItem(t *testing.T) {
	var state any
	start := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"test",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool"}}`),
		&state,
	)
	if len(start) != 1 {
		t.Fatalf("step.start events = %d, want 1", len(start))
	}
	added := responseEventPayload(start[0])
	if got := added.Get("type").String(); got != "response.output_item.added" {
		t.Fatalf("added event type = %q", got)
	}
	if got := added.Get("item.status").String(); got != "in_progress" {
		t.Fatalf("added item status = %q, want in_progress", got)
	}
	if got := added.Get("item.id").String(); got != "item-1" {
		t.Fatalf("added item id = %q, want item-1", got)
	}
	if got := added.Get("item.call_id").String(); got != "call-1" {
		t.Fatalf("added call id = %q, want call-1", got)
	}

	convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"test",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"value\":1}"}}`),
		&state,
	)
	stop := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"test",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.stop","index":0}`),
		&state,
	)
	if len(stop) != 2 {
		t.Fatalf("step.stop events = %d, want 2", len(stop))
	}
	argumentsDone := responseEventPayload(stop[0])
	if got := argumentsDone.Get("type").String(); got != "response.function_call_arguments.done" {
		t.Fatalf("arguments done type = %q", got)
	}
	if got := argumentsDone.Get("item_id").String(); got != "item-1" {
		t.Fatalf("arguments done item id = %q, want item-1", got)
	}
	if got := argumentsDone.Get("arguments").String(); got != `{"value":1}` {
		t.Fatalf("arguments done payload = %q", got)
	}
	itemDone := responseEventPayload(stop[1])
	if got := itemDone.Get("type").String(); got != "response.output_item.done" {
		t.Fatalf("item done type = %q", got)
	}
	if got := itemDone.Get("item.status").String(); got != "completed" {
		t.Fatalf("item done status = %q, want completed", got)
	}
	if got := itemDone.Get("item.id").String(); got != "item-1" {
		t.Fatalf("item done id = %q, want item-1", got)
	}
	if got := itemDone.Get("item.call_id").String(); got != "call-1" {
		t.Fatalf("item done call id = %q, want call-1", got)
	}
}

func TestInteractionsFunctionCallStreamInitialAndEmptyArguments(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		want      string
		wantTypes []string
	}{
		{
			name:      "initial arguments",
			arguments: `{"value":1}`,
			want:      `{"value":1}`,
			wantTypes: []string{
				"response.output_item.added",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.completed",
			},
		},
		{
			name:      "empty arguments",
			arguments: `{}`,
			want:      `{}`,
			wantTypes: []string{
				"response.output_item.added",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.completed",
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				var state any
				var events [][]byte
				for _, input := range [][]byte{
					[]byte(`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":` + test.arguments + `}}`),
					[]byte(`data: {"event_type":"step.stop","index":0}`),
					[]byte(`data: {"event_type":"interaction.completed","interaction":{"id":"response-1","model":"test"}}`),
				} {
					events = append(
						events, convertInteractionsResponseToOpenAIResponses(
							context.Background(), "test", nil, nil, input, &state,
						)...,
					)
				}

				if len(events) != len(test.wantTypes) {
					t.Fatalf("event count = %d, want %d; events=%q", len(events), len(test.wantTypes), events)
				}
				var delta, argumentsDone, itemDone, completed gjson.Result
				for i, event := range events {
					payload := responseEventPayload(event)
					if got := payload.Get("type").String(); got != test.wantTypes[i] {
						t.Fatalf("event %d type = %q, want %q; event=%s", i, got, test.wantTypes[i], event)
					}
					switch payload.Get("type").String() {
					case "response.function_call_arguments.delta":
						delta = payload
					case "response.function_call_arguments.done":
						argumentsDone = payload
					case "response.output_item.done":
						itemDone = payload
					case "response.completed":
						completed = payload
					}
				}
				if test.arguments != `{}` && delta.Get("delta").String() != test.want {
					t.Fatalf("initial arguments delta = %q, want %q", delta.Get("delta").String(), test.want)
				}
				if got := argumentsDone.Get("arguments").String(); got != test.want {
					t.Fatalf("arguments done = %q, want %q", got, test.want)
				}
				if got := itemDone.Get("item.arguments").String(); got != test.want {
					t.Fatalf("item done arguments = %q, want %q", got, test.want)
				}
				if got := completed.Get("response.output.0.arguments").String(); got != test.want {
					t.Fatalf("completed arguments = %q, want %q", got, test.want)
				}
			},
		)
	}
}

func TestInteractionsFunctionCallStreamEventsAreIdempotent(t *testing.T) {
	var state any
	if events := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"test",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"ignored"}}`),
		&state,
	); len(events) != 0 {
		t.Fatalf("arguments before step.start emitted %d events, want none", len(events))
	}

	var events [][]byte
	for _, input := range [][]byte{
		[]byte(`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":{"value":1}}}`),
		[]byte(`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-2","call_id":"call-2","name":"other","arguments":{"value":2}}}`),
		[]byte(`data: {"event_type":"step.stop","index":0}`),
		[]byte(`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":",\"late\":true"}}`),
		[]byte(`data: {"event_type":"step.stop","index":0}`),
	} {
		events = append(
			events, convertInteractionsResponseToOpenAIResponses(
				context.Background(), "test", nil, nil, input, &state,
			)...,
		)
	}

	wantTypes := []string{
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d, want %d; events=%q", len(events), len(wantTypes), events)
	}
	for i, event := range events {
		payload := responseEventPayload(event)
		if got := payload.Get("type").String(); got != wantTypes[i] {
			t.Fatalf("event %d type = %q, want %q; event=%s", i, got, wantTypes[i], event)
		}
	}
	itemDone := responseEventPayload(events[len(events)-1])
	if got := itemDone.Get("item.id").String(); got != "item-1" {
		t.Fatalf("item id = %q, want first start identity", got)
	}
	if got := itemDone.Get("item.call_id").String(); got != "call-1" {
		t.Fatalf("call id = %q, want first start identity", got)
	}
	if got := itemDone.Get("item.arguments").String(); got != `{"value":1}` {
		t.Fatalf("item arguments = %q, want late delta ignored", got)
	}
}

func TestInteractionsCreatedUsesOriginalRequestModel(t *testing.T) {
	var state any
	outputs := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"fallback-model",
		[]byte(`{"request":{"model":"original-interactions-model"}}`),
		[]byte(`{"model":"translated-interactions-model"}`),
		[]byte(`data: {"interaction":{"id":"interaction_1","model":"source-model"},"event_type":"interaction.created"}`),
		&state,
	)
	if len(outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(outputs))
	}
	payload := responseEventPayload(outputs[0])
	if got := payload.Get("response.model").String(); got != "original-interactions-model" {
		t.Fatalf("response.created model = %q, want original-interactions-model", got)
	}
	if !payload.Get("response.output").IsArray() {
		t.Fatalf("response.created output = %s, want array", payload.Get("response.output").Raw)
	}
}

func TestIncompleteResponseMetadataRoundTripsNonStream(t *testing.T) {
	interactionsRaw := []byte(`{
		"interaction":{
			"id":"interaction-1",
			"model":"antigravity-model",
			"status":"incomplete",
			"incomplete_details":{"reason":"max_output_tokens","limit":128},
			"environment":{"id":"environment-1"},
			"steps":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"external_external_read_file","arguments":{}}]
		}
	}`)
	response := convertInteractionsResponseToOpenAIResponsesNonStream(
		context.Background(), "", nil, nil, interactionsRaw, nil,
	)
	if got := gjson.GetBytes(response, "status").String(); got != "incomplete" {
		t.Fatalf("response status = %q, want incomplete; response=%s", got, response)
	}
	if got := gjson.GetBytes(response, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("response incomplete reason = %q; response=%s", got, response)
	}
	if got := gjson.GetBytes(response, "incomplete_details.limit").Int(); got != 128 {
		t.Fatalf("response incomplete limit = %d, want 128; response=%s", got, response)
	}
	if got := gjson.GetBytes(response, "environment_id").String(); got != "environment-1" {
		t.Fatalf("response environment = %q; response=%s", got, response)
	}
	if got := gjson.GetBytes(response, "output.0.name").String(); got != "external_read_file" {
		t.Fatalf("response tool name = %q, want external_read_file; response=%s", got, response)
	}

	interactions := convertOpenAIResponsesResponseToInteractionsNonStream(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`{
			"id":"response-1",
			"status":"incomplete",
			"incomplete_details":{"reason":"max_output_tokens","limit":256},
			"environment":{"id":"environment-2"},
			"output":[{"type":"function_call","id":"item-2","call_id":"call-2","name":"external_read_file","arguments":"{}"}]
		}`),
		nil,
	)
	if got := gjson.GetBytes(interactions, "status").String(); got != "incomplete" {
		t.Fatalf("interaction status = %q, want incomplete; interaction=%s", got, interactions)
	}
	if got := gjson.GetBytes(interactions, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("interaction incomplete reason = %q; interaction=%s", got, interactions)
	}
	if got := gjson.GetBytes(interactions, "incomplete_details.limit").Int(); got != 256 {
		t.Fatalf("interaction incomplete limit = %d, want 256; interaction=%s", got, interactions)
	}
	if got := gjson.GetBytes(interactions, "environment_id").String(); got != "environment-2" {
		t.Fatalf("interaction environment = %q; interaction=%s", got, interactions)
	}
	if got := gjson.GetBytes(interactions, "steps.0.name").String(); got != "external_external_read_file" {
		t.Fatalf("interaction tool name = %q, want external_external_read_file; interaction=%s", got, interactions)
	}
}

func TestInteractionsIncompleteStreamPreservesDetailsAndEnvironment(t *testing.T) {
	var state any
	created := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"event_type":"interaction.created","interaction":{"id":"interaction-1","model":"antigravity-model","environment":{"id":"environment-stream"}}}`),
		&state,
	)
	if len(created) != 1 {
		t.Fatalf("created events = %d, want 1", len(created))
	}
	start := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"external_external_read_file","arguments":{}}}`),
		&state,
	)
	if len(start) != 1 {
		t.Fatalf("function start events = %d, want 1", len(start))
	}
	if got := responseEventPayload(start[0]).Get("item.name").String(); got != "external_read_file" {
		t.Fatalf("stream response tool name = %q, want external_read_file", got)
	}
	convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.stop","index":0}`),
		&state,
	)
	terminal := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"event_type":"interaction.completed","interaction":{"id":"interaction-1","model":"antigravity-model","status":"incomplete","incomplete_details":{"reason":"max_output_tokens","limit":512}}}`),
		&state,
	)
	if len(terminal) != 1 {
		t.Fatalf("terminal events = %d, want 1", len(terminal))
	}
	payload := responseEventPayload(terminal[0])
	if got := payload.Get("type").String(); got != "response.incomplete" {
		t.Fatalf("terminal type = %q, want response.incomplete", got)
	}
	if got := payload.Get("response.status").String(); got != "incomplete" {
		t.Fatalf("terminal status = %q, want incomplete", got)
	}
	if got := payload.Get("response.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("terminal incomplete reason = %q", got)
	}
	if got := payload.Get("response.incomplete_details.limit").Int(); got != 512 {
		t.Fatalf("terminal incomplete limit = %d, want 512", got)
	}
	if got := payload.Get("response.environment_id").String(); got != "environment-stream" {
		t.Fatalf("terminal environment = %q, want environment-stream", got)
	}
	duplicate := convertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"incomplete"}}`),
		&state,
	)
	if len(duplicate) != 0 {
		t.Fatalf("duplicate terminal emitted %d events, want none", len(duplicate))
	}
}

func TestResponsesIncompleteStreamPreservesDetailsAndEnvironment(t *testing.T) {
	var state any
	created := convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"type":"response.created","response":{"id":"response-1","model":"antigravity-model","status":"in_progress","environment":{"id":"environment-stream"},"output":[]}}`),
		&state,
	)
	if len(created) != 2 {
		t.Fatalf("created events = %d, want 2", len(created))
	}
	start := convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"external_read_file","arguments":""}}`),
		&state,
	)
	if len(start) != 1 {
		t.Fatalf("function start events = %d, want 1", len(start))
	}
	if got := responseEventPayload(start[0]).Get("step.name").String(); got != "external_external_read_file" {
		t.Fatalf("stream interaction tool name = %q, want external_external_read_file", got)
	}
	terminal := convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"type":"response.incomplete","response":{"id":"response-1","model":"antigravity-model","status":"incomplete","incomplete_details":{"reason":"max_output_tokens","limit":1024},"output":[],"usage":{}}}`),
		&state,
	)
	var completed gjson.Result
	for _, event := range terminal {
		payload := responseEventPayload(event)
		if payload.Get("event_type").String() == "interaction.completed" {
			completed = payload
		}
	}
	if !completed.Exists() {
		t.Fatalf("missing interaction.completed event: %q", terminal)
	}
	if got := completed.Get("interaction.status").String(); got != "incomplete" {
		t.Fatalf("interaction terminal status = %q, want incomplete", got)
	}
	if got := completed.Get("interaction.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("interaction incomplete reason = %q", got)
	}
	if got := completed.Get("interaction.incomplete_details.limit").Int(); got != 1024 {
		t.Fatalf("interaction incomplete limit = %d, want 1024", got)
	}
	if got := completed.Get("interaction.environment_id").String(); got != "environment-stream" {
		t.Fatalf("interaction terminal environment = %q, want environment-stream", got)
	}
	duplicate := convertOpenAIResponsesResponseToInteractions(
		context.Background(),
		"antigravity-model",
		nil,
		nil,
		[]byte(`data: {"type":"response.incomplete","response":{"id":"response-1","status":"incomplete","output":[]}}`),
		&state,
	)
	if len(duplicate) != 0 {
		t.Fatalf("duplicate terminal emitted %d events, want none", len(duplicate))
	}
}

func responseEventPayload(raw []byte) gjson.Result {
	marker := []byte("data: ")
	_, after, ok := bytes.Cut(raw, marker)
	if !ok {
		return gjson.Result{}
	}
	payload := after
	if end := bytes.IndexByte(payload, '\n'); end >= 0 {
		payload = payload[:end]
	}
	return gjson.ParseBytes(payload)
}
