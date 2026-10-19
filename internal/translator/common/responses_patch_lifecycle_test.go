package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesPatchBridgeNamedLateIdentity(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, first := range []string{"item", "call", "neither"} {
		for _, boundary := range []string{"item", "terminal"} {
			t.Run(first+"/"+boundary, func(t *testing.T) {
				bridge := NewResponsesPatchBridge([]byte(request))
				itemID, callID := "", ""
				if first == "item" {
					itemID = "item-1"
				}
				if first == "call" {
					callID = "call-1"
				}
				pending := []string{
					fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":%q,"call_id":%q,"name":"apply_patch","arguments":""}}`, itemID, callID),
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"input\":\"p"}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"q\"}"}`,
					`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"input\":\"pq\"}"}`,
				}
				for _, event := range pending {
					if events, err := bridge.Transform([]byte(event)); err != nil || len(events) != 0 {
						t.Fatalf("published unresolved identity: events=%q error=%v", events, err)
					}
				}
				const item = `{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"pq\"}"}`
				var events [][]byte
				if boundary == "item" {
					out, err := bridge.Transform([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`))
					if err != nil {
						t.Fatal(err)
					}
					events = append(events, out...)
				}
				out, err := bridge.Transform([]byte(`{"type":"response.completed","response":{"output":[` + item + `]}}`))
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, out...)
				counts := make(map[string]int)
				var fragments []string
				for _, event := range events {
					root := gjson.ParseBytes(event)
					kind := root.Get("type").String()
					counts[kind]++
					identity, idKey := root, "item_id"
					if root.Get("item").Exists() {
						identity, idKey = root.Get("item"), "id"
					}
					if kind == "response.completed" {
						identity, idKey = root.Get("response.output.0"), "id"
					}
					if identity.Get(idKey).String() != "item-1" || identity.Get("call_id").String() != "call-1" {
						t.Fatalf("unresolved or changed identity: %s", event)
					}
					if kind == "response.custom_tool_call_input.delta" {
						fragments = append(fragments, root.Get("delta").String())
					}
				}
				if strings.Join(fragments, "|") != "p|q" {
					t.Fatalf("buffered fragments were changed: %q", fragments)
				}
				for _, kind := range []string{"response.output_item.added", "response.custom_tool_call_input.done", "response.output_item.done", "response.completed"} {
					if counts[kind] != 1 {
						t.Fatalf("lifecycle counts=%v", counts)
					}
				}
			})
		}
	}
}

func TestResponsesPatchBridgeSparseTerminal(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, snapshot := range []string{
		`{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}`,
		`{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}`,
	} {
		bridge := NewResponsesPatchBridge([]byte(request))
		for _, event := range []string{
			`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":""}}`,
			`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}`,
		} {
			if _, err := bridge.Transform([]byte(event)); err != nil {
				t.Fatal(err)
			}
		}
		events, err := bridge.Transform([]byte(`{"type":"response.completed","response":{"output":[` + snapshot + `]}}`))
		if err != nil || len(events) != 1 || gjson.GetBytes(events[0], "response.output.0.input").String() != "patch" {
			t.Fatalf("sparse terminal changed identity or repeated progress: events=%q error=%v", events, err)
		}
	}
}

func TestResponsesPatchBridgeRetainsAddedSnapshotEvidence(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, arguments := range []string{`{"input":"patch","extra":1}`, `{"input":"partial`, `{"input":"\ud800"}`} {
		bridge := NewResponsesPatchBridge([]byte(request))
		event := fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":%q}}`, arguments)
		if events, err := bridge.Transform([]byte(event)); err == nil || len(events) != 0 {
			t.Fatalf("invalid added snapshot was ignored: events=%q error=%v", events, err)
		}
	}
}
