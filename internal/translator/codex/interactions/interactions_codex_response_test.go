package interactions

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexFunctionCallNonStreamPreservesItemAndCallIDs(t *testing.T) {
	raw := []byte(`{"id":"response-1","model":"test","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool","arguments":"{}"}]}`)
	out := convertCodexResponseToInteractionsNonStream(
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
	out := convertCodexResponseToInteractions(
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

func TestConvertCodexResponseToInteractionsIncompleteTerminal(t *testing.T) {
	raw := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	nonStreamOut := convertCodexResponseToInteractionsNonStream(
		context.Background(), "codex-test", nil, nil, raw, nil,
	)
	if got := gjson.GetBytes(nonStreamOut, "status").String(); got != "incomplete" {
		t.Fatalf("non-stream status = %q, want incomplete. Output: %s", got, nonStreamOut)
	}

	var param any
	streamOut := convertCodexResponseToInteractions(
		context.Background(), "codex-test", nil, nil, append([]byte("data: "), raw...), &param,
	)
	for _, event := range streamOut {
		payload := codexInteractionEventPayload(event)
		if payload.Get("event_type").String() != "interaction.completed" {
			continue
		}
		if got := payload.Get("interaction.status").String(); got != "incomplete" {
			t.Fatalf("stream status = %q, want incomplete. Payload: %s", got, payload.Raw)
		}
		return
	}
	t.Fatalf("interaction.completed event missing: %q", streamOut)
}

func codexInteractionEventPayload(raw []byte) gjson.Result {
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
