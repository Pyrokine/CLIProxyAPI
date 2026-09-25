package interactions

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
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

func TestCodexResponseToInteractionsValidatesReasoningSignatures(t *testing.T) {
	valid := codexSourceSignatureForTest()
	for _, testCase := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "valid", signature: valid, want: valid},
		{name: "foreign", signature: codexClaudeSignatureForTest()},
		{name: "invalid", signature: "gpt#invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"id":"response-1","model":"gpt-test","output":[{"type":"reasoning","encrypted_content":"` + testCase.signature + `","summary":[]}]}`)
			out := convertCodexResponseToInteractionsNonStream(
				context.Background(), "gpt-test", nil, nil, raw, nil,
			)
			if got := gjson.GetBytes(out, "steps.0.signature").String(); got != testCase.want {
				t.Fatalf("signature = %q, want %q; output=%s", got, testCase.want, out)
			}
			if testCase.want == "" && gjson.GetBytes(out, "steps.#").Int() != 0 {
				t.Fatalf("invalid signature-only reasoning produced a step: %s", out)
			}
		})
	}
}

func TestCodexResponseToInteractionsStreamPreservesSignatureOnlyReasoning(t *testing.T) {
	valid := codexSourceSignatureForTest()
	var state any
	events := convertCodexResponseToInteractions(
		context.Background(),
		"gpt-test",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"`+valid+`","summary":[]}}`),
		&state,
	)
	for _, event := range events {
		payload := codexInteractionEventPayload(event)
		if got := payload.Get("step.signature").String(); got == valid {
			return
		}
		if got := payload.Get("delta.signature").String(); got == valid {
			return
		}
	}
	t.Fatalf("valid reasoning signature missing: %q", events)
}

func codexSourceSignatureForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(payload)
}

func codexClaudeSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
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
