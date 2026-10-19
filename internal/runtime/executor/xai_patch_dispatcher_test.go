package executor

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func xaiPatchNamespaceNormalizer(t *testing.T) *xaiPreparedResponseNormalizer {
	t.Helper()
	tools := []map[string]any{{"type": "custom", "name": "apply_patch"}, {"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
	for index := 0; index < xaiMaxTools; index++ {
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("tool_%d", index), "parameters": map[string]any{"type": "object"}})
	}
	payload, err := json.Marshal(map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "editor", "tools": tools}}, "input": "patch"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(cliproxyexecutor.Request{Model: "grok-4.7", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(prepared.body, "tools.0.name").String() != "editor" {
		t.Fatalf("namespace was not folded: %s", prepared.body)
	}
	return prepared.newResponseNormalizer()
}

func TestXAIApplyPatchDispatcherLateIdentity(t *testing.T) {
	for _, first := range []string{"item", "call", "neither", "unnamed"} {
		for _, boundary := range []string{"item", "terminal"} {
			t.Run(first+"/"+boundary, func(t *testing.T) {
				normalizer := xaiPatchNamespaceNormalizer(t)
				itemID, callID, name := "", "", "editor"
				if first == "item" {
					itemID = "item-1"
				} else if first == "call" {
					callID = "call-1"
				} else if first == "unnamed" {
					name = ""
				}
				index := 2
				if boundary == "terminal" && itemID == "" && callID == "" {
					index = 0
				}
				const wrapper = `{"name":"apply_patch","arguments":{"input":"patch"}}`
				for _, event := range []string{
					fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"type":"function_call","id":%q,"call_id":%q,"name":%q,"arguments":""}}`, index, itemID, callID, name),
					fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,"delta":"{\"name\":\"apply_patch\","}`, index),
					fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":%d,"arguments":%q}`, index, wrapper),
				} {
					if events := normalizer.normalize([]byte(event)); len(events) != 0 {
						t.Fatalf("unresolved dispatcher was delivered: %q", events)
					}
				}
				var events [][]byte
				if boundary == "item" {
					events = append(events, normalizer.normalize([]byte(`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor"}}`))...)
				}
				events = append(events, normalizer.normalize([]byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor"}]}}`))...)
				counts := make(map[string]int)
				for _, event := range events {
					root := gjson.ParseBytes(event)
					kind := root.Get("type").String()
					counts[kind]++
					if kind == "response.failed" || kind == "response.custom_tool_call_input.delta" {
						t.Fatalf("dispatcher failed or fabricated a patch preview: %s", event)
					}
					if kind == "response.custom_tool_call_input.done" && root.Get("input").String() != "patch" {
						t.Fatalf("dispatcher input changed: %s", event)
					}
					if kind == "response.completed" && (root.Get("response.output.0.input").String() != "patch" || root.Get("response.output.0.namespace").String() != "editor") {
						t.Fatalf("sparse dispatcher terminal changed: %s", event)
					}
				}
				for _, kind := range []string{"response.output_item.added", "response.custom_tool_call_input.done", "response.output_item.done", "response.completed"} {
					if counts[kind] != 1 {
						t.Fatalf("dispatcher lifecycle counts=%v", counts)
					}
				}
			})
		}
	}
}

func TestXAIApplyPatchDispatcherRetainsSourceEvidence(t *testing.T) {
	const patch = `{"name":"apply_patch","arguments":{"input":"p"}}`
	const ordinary = `{"name":"lookup","arguments":{"value":1}}`
	for _, tc := range []struct {
		name    string
		first   string
		second  string
		final   string
		invalid bool
	}{
		{"changed input", patch, `{"name":"apply_patch","arguments":{"input":"q"}}`, "editor", true},
		{"changed child", patch, ordinary, "editor", true},
		{"ordinary to patch", ordinary, patch, "editor", true},
		{"late child", patch, patch, "apply_patch", false},
		{"ordinary child", ordinary, ordinary, "lookup", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			normalizer := xaiPatchNamespaceNormalizer(t)
			normalizer.normalize([]byte(`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"item-1","name":"editor"}}`))
			var events [][]byte
			for _, wrapper := range []string{tc.first, tc.second} {
				events = append(events, normalizer.normalize([]byte(fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":"item-1","arguments":%q}`, wrapper)))...)
			}
			events = append(events, normalizer.normalize([]byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":%q}}`, tc.final)))...)
			failed := 0
			for _, event := range events {
				if gjson.GetBytes(event, "type").String() == "response.failed" {
					failed++
				}
			}
			if tc.invalid {
				if failed != 1 || len(normalizer.normalize([]byte(`{"type":"response.completed","response":{"output":[]}}`))) != 0 {
					t.Fatalf("dispatcher failure was lost or repeated: %q", events)
				}
				return
			}
			if failed != 0 || len(events) == 0 {
				t.Fatalf("valid child was lost: %q", events)
			}
			last := gjson.ParseBytes(events[len(events)-1]).Get("item")
			if last.Get("name").String() != tc.final || last.Get("namespace").String() != "editor" {
				t.Fatalf("late child identity changed: %s", last.Raw)
			}
			if tc.final == "lookup" && (last.Get("type").String() != "function_call" || last.Get("arguments").String() != `{"value":1}`) {
				t.Fatalf("ordinary child acquired patch conversion: %s", last.Raw)
			}
		})
	}
}

func TestXAIApplyPatchDispatcherRootLateName(t *testing.T) {
	normalizer := xaiPatchNamespaceNormalizer(t)
	if events := normalizer.normalize([]byte(`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1"}}`)); len(events) != 0 {
		t.Fatalf("unresolved source was delivered: %q", events)
	}
	events := normalizer.normalize([]byte(`{"type":"response.function_call_arguments.done","item_id":"item-1","name":"editor","namespace":"editor","arguments":"{\"name\":\"apply_patch\",\"arguments\":{\"input\":\"patch\"}}"}`))
	if len(events) != 2 || gjson.GetBytes(events[0], "item.namespace").String() != "editor" || gjson.GetBytes(events[1], "type").String() != "response.custom_tool_call_input.done" || gjson.GetBytes(events[1], "input").String() != "patch" {
		t.Fatalf("root source name was lost: %q", events)
	}
}

func TestXAIApplyPatchDispatcherPreservesOrdinaryProgress(t *testing.T) {
	normalizer := xaiPatchNamespaceNormalizer(t)
	for _, event := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"ordinary-item","call_id":"ordinary-call","name":"lookup","namespace":"editor","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"ordinary-item","delta":"{\"value\":1}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"ordinary-item","arguments":"{\"value\":1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"ordinary-item","call_id":"ordinary-call","name":"lookup","namespace":"editor","arguments":"{\"value\":1}"}}`,
	} {
		events := normalizer.normalize([]byte(event))
		if len(events) != 1 || gjson.GetBytes(events[0], "type").String() != gjson.Get(event, "type").String() {
			t.Fatalf("ordinary progress was delayed or rewritten: %q", events)
		}
	}
}

func TestXAIApplyPatchDispatcherRejectsConflictingAliases(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"item-1","call_id":"call-2","arguments":"{\"name\":\"apply_patch\",\"arguments\":{\"input\":\"p\"}}"}`,
		`{"type":"response.output_item.done","output_index":0,"item_id":"different","item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor"}}`,
		`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor","namespace":"other","arguments":"{\"name\":\"apply_patch\",\"arguments\":{\"input\":\"p\"}}"}]}}`,
	} {
		normalizer := xaiPatchNamespaceNormalizer(t)
		for index := 0; index < 2; index++ {
			normalizer.normalize([]byte(fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"type":"function_call","id":"item-%d","call_id":"call-%d","name":"editor"}}`, index, index+1, index+1)))
		}
		events := normalizer.normalize([]byte(event))
		if len(events) != 1 || gjson.GetBytes(events[0], "type").String() != "response.failed" {
			t.Fatalf("contradictory source identity was accepted: %q", events)
		}
	}
}
