package interactions

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexFunctionCallNonStreamPreservesItemAndCallIDs(t *testing.T) {
	raw := []byte(`{"id":"response-1","model":"test","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":"{}"}]}`)
	out := ConvertCodexResponseToInteractionsNonStream(
		context.Background(),
		"test",
		nil,
		nil,
		raw,
		nil,
	)
	if got := gjson.GetBytes(out, "steps.0.id").String(); got != "item-1" {
		t.Fatalf("function call item id = %q, want item-1", got)
	}
	if got := gjson.GetBytes(out, "steps.0.call_id").String(); got != "call-1" {
		t.Fatalf("function call call id = %q, want call-1", got)
	}
}

func TestCodexFunctionCallStreamPreservesItemAndCallIDs(t *testing.T) {
	var state any
	out := ConvertCodexResponseToInteractions(
		context.Background(),
		"test",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool"}}`),
		&state,
	)
	if len(out) == 0 {
		t.Fatal("function call output item produced no interaction events")
	}
	var stepStart gjson.Result
	for _, event := range out {
		payload := codexInteractionEventPayload(event)
		if payload.Get("event_type").String() == "step.start" {
			stepStart = payload
			break
		}
	}
	if !stepStart.Exists() {
		t.Fatalf("step.start not found in %q", out)
	}
	if got := stepStart.Get("step.id").String(); got != "item-1" {
		t.Fatalf("step item id = %q, want item-1", got)
	}
	if got := stepStart.Get("step.call_id").String(); got != "call-1" {
		t.Fatalf("step call id = %q, want call-1", got)
	}
}

func codexInteractionEventPayload(raw []byte) gjson.Result {
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
