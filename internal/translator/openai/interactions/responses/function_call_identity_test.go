package responses

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestInteractionsFunctionCallPreservesItemAndCallIDs(t *testing.T) {
	raw := []byte(`{"id":"response-1","model":"test","steps":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":{}}]}`)
	out := ConvertInteractionsResponseToOpenAIResponsesNonStream(
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
	start := ConvertInteractionsResponseToOpenAIResponses(
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

	ConvertInteractionsResponseToOpenAIResponses(
		context.Background(),
		"test",
		nil,
		nil,
		[]byte(`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"value\":1}"}}`),
		&state,
	)
	stop := ConvertInteractionsResponseToOpenAIResponses(
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

func responseEventPayload(raw []byte) gjson.Result {
	marker := []byte("data: ")
	index := bytes.Index(raw, marker)
	if index < 0 {
		return gjson.Result{}
	}
	payload := raw[index+len(marker):]
	if end := bytes.IndexByte(payload, '\n'); end >= 0 {
		payload = payload[:end]
	}
	return gjson.ParseBytes(payload)
}
