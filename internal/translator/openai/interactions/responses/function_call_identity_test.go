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
	out := responsesFunctionCallToInteractions(item)
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
