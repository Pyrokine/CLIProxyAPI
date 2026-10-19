package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeResponsesPatchHistoryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		call    string
		invalid bool
	}{
		{name: "qualified namespace", call: `{"type":"custom_tool_call","call_id":"call-1","name":"editor__apply_patch","input":"patch"}`},
		{name: "qualified invalid input", call: `{"type":"custom_tool_call","call_id":"call-1","name":"editor__apply_patch","input":{}}`, invalid: true},
		{name: "qualified invalid Unicode", call: `{"type":"custom_tool_call","call_id":"call-1","name":"editor__apply_patch","input":"\ud800"}`, invalid: true},
		{name: "missing call ID", call: `{"type":"custom_tool_call","namespace":"editor","name":"apply_patch","input":"patch"}`, invalid: true},
		{name: "blank call ID", call: `{"type":"custom_tool_call","call_id":" ","namespace":"editor","name":"apply_patch","input":"patch"}`, invalid: true},
		{name: "numeric call ID", call: `{"type":"custom_tool_call","call_id":1,"namespace":"editor","name":"apply_patch","input":"patch"}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := `{"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}],"input":[` + tc.call + `,{"type":"custom_tool_call_output","call_id":"call-1","output":"done"}]}`
			body, err := NormalizeResponsesPatchTools([]byte(request))
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v, want invalid=%t; body=%s", err, tc.invalid, body)
			}
			if !tc.invalid && (gjson.GetBytes(body, "input.0.arguments").String() != `{"input":"patch"}` || gjson.GetBytes(body, "input.1.type").String() != "function_call_output") {
				t.Fatalf("qualified history was not converted: %s", body)
			}
		})
	}
}

func TestResponsesPatchBridgeRejectsInvalidEventIdentity(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"ordinary","parameters":{"type":"object"}}]}`
	const added = `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`
	for _, tc := range []struct {
		name     string
		event    string
		deferred bool
	}{
		{name: "unresolved argument identity", event: `{"type":"response.function_call_arguments.delta","item_id":"unknown","output_index":2,"delta":"{\"input\":\"patch\"}"}`, deferred: true},
		{name: "duplicate call ID", event: `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"item-2","call_id":"call-1","name":"apply_patch"}}`},
		{name: "mismatched event item ID", event: `{"type":"response.output_item.done","item_id":"different","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}`},
		{name: "negative output index", event: `{"type":"response.output_item.added","output_index":-1,"item":{"type":"function_call","id":"item-2","call_id":"call-2","name":"apply_patch"}}`},
		{name: "string output index", event: `{"type":"response.function_call_arguments.delta","output_index":"0","item_id":"item-1","delta":"{\"input\":\"patch\"}"}`},
		{name: "unexpected native custom call", event: `{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"item-1","call_id":"call-1","name":"apply_patch","input":"patch"}}`},
		{name: "unexpected native custom terminal", event: `{"type":"response.completed","response":{"output":[{"type":"custom_tool_call","id":"item-1","call_id":"call-1","name":"apply_patch","input":"patch"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridge := NewResponsesPatchBridge([]byte(request))
			if _, err := bridge.Transform([]byte(added)); err != nil {
				t.Fatal(err)
			}
			events, err := bridge.Transform([]byte(tc.event))
			if tc.deferred {
				if err != nil || len(events) != 0 {
					t.Fatalf("unresolved evidence was delivered or rejected: events=%q error=%v", events, err)
				}
				events, err = bridge.Transform([]byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}]}}`))
			}
			if err == nil || len(events) != 0 {
				t.Fatalf("accepted invalid identity: events=%q error=%v", events, err)
			}
		})
	}
}

func TestResponsesPatchBridgeRejectsInvalidEnvelopeUnicode(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	const added = `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`
	for _, raw := range []string{`"{\"input\":\"\ud800\"}"`, `"{\"input\":\"\udc00\"}"`, "\"{\\\"input\\\":\\\"" + string([]byte{0xff}) + "\\\"}\""} {
		for _, kind := range []string{"delta", "done"} {
			t.Run(fmt.Sprintf("%s/%x", kind, raw), func(t *testing.T) {
				bridge := NewResponsesPatchBridge([]byte(request))
				if _, err := bridge.Transform([]byte(added)); err != nil {
					t.Fatal(err)
				}
				field := "delta"
				if kind == "done" {
					field = "arguments"
				}
				event := fmt.Sprintf(`{"type":"response.function_call_arguments.%s","output_index":0,"item_id":"item-1","%q:%s}`, kind, field, raw)
				if events, err := bridge.Transform([]byte(event)); err == nil || len(events) != 0 {
					t.Fatalf("accepted invalid envelope Unicode: events=%q error=%v", events, err)
				} else if strings.Contains(err.Error(), raw) {
					t.Fatal("error disclosed upstream input")
				}
			})
		}
	}
}

func TestResponsesPatchBridgePreservesOrdinaryCalls(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"ordinary","parameters":{"type":"object"}}]}`
	bridge := NewResponsesPatchBridge([]byte(request))
	for _, event := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"ordinary-item","call_id":"ordinary-call","name":"ordinary"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"ordinary-item","delta":"{\"value\":1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"ordinary-item","call_id":"ordinary-call","name":"ordinary","arguments":"{\"value\":1}"}}`,
		`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"ordinary-item","call_id":"ordinary-call","name":"ordinary","arguments":"{\"value\":1}"}]}}`,
	} {
		events, err := bridge.Transform([]byte(event))
		if err != nil || len(events) != 1 || gjson.GetBytes(events[0], "type").String() != gjson.Get(event, "type").String() {
			t.Fatalf("ordinary event changed: events=%q error=%v", events, err)
		}
	}
}
