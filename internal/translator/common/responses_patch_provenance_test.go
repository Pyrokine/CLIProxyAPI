package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesPatchBridgeLateNameRetainsFragments(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, boundary := range []string{"item", "terminal", "root"} {
		t.Run(boundary, func(t *testing.T) {
			bridge := NewResponsesPatchBridge([]byte(request))
			for _, event := range []string{
				`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"input\":\"p"}`,
				`{"type":"response.function_call_arguments.delta","item_id":"item-1","delta":"q\"}"}`,
			} {
				if events, err := bridge.Transform([]byte(event)); err != nil || len(events) != 0 {
					t.Fatalf("unresolved name was delivered: events=%q error=%v", events, err)
				}
			}
			const item = `{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"pq\"}"}`
			event := `{"type":"response.output_item.done","output_index":2,"item":` + item + `}`
			if boundary == "terminal" {
				event = `{"type":"response.completed","response":{"output":[` + item + `]}}`
			} else if boundary == "root" {
				event = `{"type":"response.function_call_arguments.done","item_id":"item-1","name":"apply_patch","arguments":"{\"input\":\"pq\"}"}`
			}
			events, err := bridge.Transform([]byte(event))
			if err != nil {
				t.Fatal(err)
			}
			var fragments []string
			added, inputDone := 0, 0
			for _, output := range events {
				root := gjson.ParseBytes(output)
				switch root.Get("type").String() {
				case "response.output_item.added":
					added++
					if root.Get("item.name").String() != "apply_patch" || root.Get("item.input").String() != "" {
						t.Fatalf("late name or added input changed: %s", output)
					}
				case "response.custom_tool_call_input.delta":
					fragments = append(fragments, root.Get("delta").String())
				case "response.custom_tool_call_input.done":
					inputDone++
					if root.Get("input").String() != "pq" {
						t.Fatalf("late input changed: %s", output)
					}
				}
			}
			if added != 1 || inputDone != 1 || strings.Join(fragments, "|") != "p|q" {
				t.Fatalf("late lifecycle: added=%d done=%d fragments=%q", added, inputDone, fragments)
			}
		})
	}
}

func TestResponsesPatchBridgeRetainsPreNameEvidence(t *testing.T) {
	const request = `{"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}]}`
	for _, tc := range []struct {
		name string
		item string
	}{
		{"invalid snapshot", `{"type":"function_call","id":"item-1","call_id":"call-1","arguments":"{\"input\":\"p\",\"extra\":1}"}`},
		{"partial snapshot", `{"type":"function_call","id":"item-1","call_id":"call-1","arguments":"{\"input\":\"p"}`},
		{"changed type", `{"type":"message","id":"item-1","call_id":"call-1"}`},
		{"changed call ID", `{"type":"function_call","id":"item-1","call_id":"original"}`},
		{"changed namespace", `{"type":"function_call","id":"item-1","call_id":"call-1","namespace":"other"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridge := NewResponsesPatchBridge([]byte(request))
			if _, err := bridge.Transform([]byte(`{"type":"response.output_item.added","output_index":0,"item":` + tc.item + `}`)); err != nil {
				t.Fatalf("evidence was rejected before acquiring patch provenance: %v", err)
			}
			const completion = `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch","arguments":"{\"input\":\"p\"}"}}`
			if events, err := bridge.Transform([]byte(completion)); err == nil || len(events) != 0 {
				t.Fatalf("pre-name evidence was overwritten: events=%q error=%v", events, err)
			}
			if events, err := bridge.Transform([]byte(`{"type":"response.completed","response":{"output":[]}}`)); err == nil || len(events) != 0 {
				t.Fatal("failure was cleared by a later terminal")
			}
		})
	}
}

func TestResponsesPatchBridgeTerminalOnlyPreservesMetadata(t *testing.T) {
	bridge := NewResponsesPatchBridge([]byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`))
	events, err := bridge.Transform([]byte(`{"type":"response.completed","opaque":{"marker":"synthetic"},"source_index":9,"response":{"id":"resp-1","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}]}}`))
	if err != nil || len(events) != 5 {
		t.Fatalf("terminal-only lifecycle: %q %v", events, err)
	}
	for _, event := range events {
		root := gjson.ParseBytes(event)
		if root.Get("opaque.marker").String() != "synthetic" || root.Get("source_index").Int() != 9 {
			t.Fatalf("terminal source metadata was lost: %s", event)
		}
		if root.Get("type").String() != "response.completed" && root.Get("response_id").String() != "resp-1" {
			t.Fatalf("terminal source response identity was lost: %s", event)
		}
	}
}

func TestResponsesPatchBridgeCompleteDeltaMustMatchSnapshot(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, snapshot := range []string{`{"input":"pq"}`, `{"input":"pqr"}`} {
		bridge := NewResponsesPatchBridge([]byte(request))
		for _, event := range []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"input\":\"pq\"}"}`,
		} {
			if _, err := bridge.Transform([]byte(event)); err != nil {
				t.Fatal(err)
			}
		}
		events, err := bridge.Transform([]byte(fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":0,"arguments":%q}`, snapshot)))
		if (err != nil) != (snapshot == `{"input":"pqr"}`) {
			t.Fatalf("complete stream evidence changed: events=%q error=%v", events, err)
		}
	}
}
