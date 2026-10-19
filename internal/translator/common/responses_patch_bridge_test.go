package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeResponsesPatchToolsPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		request   string
		patch     bool
		path      string
		toolCount int
	}{
		{name: "custom", request: `{"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","definition":"start: /./"}}]}`, patch: true, path: "tools", toolCount: 1},
		{name: "ordinary function wins", request: `{"tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"patch":{"type":"string"}}}},{"type":"custom","name":"apply_patch"}]}`, path: "tools", toolCount: 1},
		{name: "custom wins same source", request: `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"apply_patch","parameters":{"type":"object"}}]}`, patch: true, path: "tools", toolCount: 1},
		{name: "identical duplicates", request: `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"custom","name":"apply_patch"}]}`, patch: true, path: "tools", toolCount: 1},
		{name: "additional source", request: `{"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch"}]}]}`, patch: true, path: "input.0.tools", toolCount: 1},
		{name: "top source wins", request: `{"tools":[{"type":"namespace","name":"ops","tools":[{"type":"custom","name":"apply_patch"}]}],"input":[{"type":"additional_tools","tools":[{"type":"function","name":"ops__apply_patch","parameters":{"type":"object"}}]}]}`, patch: true, path: "tools.0.tools", toolCount: 1},
		{name: "flat function wins", request: `{"tools":[{"type":"namespace","name":"ops","tools":[{"type":"custom","name":"apply_patch"}]},{"type":"function","name":"ops__apply_patch","parameters":{"type":"object"}}]}`, path: "tools.0.tools", toolCount: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			bridge := NewResponsesPatchBridge([]byte(test.request))
			if (bridge != nil) != test.patch {
				t.Fatalf("patch bridge = %v, want %t", bridge != nil, test.patch)
			}
			body, err := NormalizeResponsesPatchTools([]byte(test.request))
			if err != nil {
				t.Fatal(err)
			}
			if count := len(gjson.GetBytes(body, test.path).Array()); count != test.toolCount {
				t.Fatalf("tool count = %d, want %d; body=%s", count, test.toolCount, body)
			}
			if test.patch {
				tool := gjson.GetBytes(body, test.path+".0")
				if tool.Get("type").String() != "function" || tool.Get("parameters.properties.input.type").String() != "string" || tool.Get("format").Exists() {
					t.Fatalf("patch schema: %s", body)
				}
			}
		})
	}
}

func TestResponsesPatchDeclarationContract(t *testing.T) {
	body, err := NormalizeResponsesPatchTools([]byte(`{"tools":[{"type":"custom","name":"apply_patch","description":"Edit files. This is a FREEFORM tool, so do not wrap the patch in JSON.","format":{"type":"grammar","definition":"start: patch"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tool := gjson.GetBytes(body, "tools.0")
	for _, instruction := range []string{"*** Begin Patch", "*** End Patch", "*** Add File:", "*** Delete File:", "*** Update File:", "*** Move to:", "*** End of File", "@@", "start: patch", "JSON object"} {
		if !strings.Contains(tool.Get("description").String(), instruction) {
			t.Errorf("missing patch instruction %q: %s", instruction, tool.Raw)
		}
	}
	if strings.Contains(tool.Get("description").String(), "do not wrap the patch in JSON") || tool.Get("format").Exists() || tool.Get("parameters").Raw != patchFunctionParameters || tool.Get("cache_control.ttl").String() != "1h" {
		t.Fatalf("patch declaration changed its contract: %s", tool.Raw)
	}
}

func TestResponsesPatchDescriptionGuidesCompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		grammar string
	}{
		{name: "without grammar"},
		{name: "plain grammar", grammar: " \nstart: begin_patch hunk+ end_patch\r\n"},
		{name: "environment grammar", grammar: "start: begin_patch environment_id? hunk+ end_patch\nenvironment_id: \"*** Environment ID:\" /[^\\n]+/\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := fmt.Sprintf(`{"tools":[{"type":"custom","name":"apply_patch","format":{"definition":%q}}]}`, tc.grammar)
			out, err := NormalizeResponsesPatchTools([]byte(request))
			if err != nil {
				t.Fatal(err)
			}
			tool := gjson.GetBytes(out, "tools.0")
			if got := tool.Get("parameters.properties.input.description").String(); got != "The complete apply_patch patch text." {
				t.Fatalf("input description = %q", got)
			}
			description := tool.Get("description").String()
			for _, instruction := range []string{"JSON object whose input field contains the complete patch text", "not a conventional git unified diff", "Example input:\n*** Begin Patch\n*** Update File:", "Every added-file content line starts with +."} {
				if !strings.Contains(description, instruction) {
					t.Errorf("missing %q: %q", instruction, description)
				}
			}
			if !strings.HasSuffix(description, tc.grammar) {
				t.Fatalf("grammar was not preserved: %q", description)
			}
			instructions := strings.TrimSuffix(description, tc.grammar)
			if got, want := strings.Contains(instructions, "*** Environment ID:"), strings.Contains(tc.grammar, "*** Environment ID:"); got != want {
				t.Fatalf("environment instruction = %t, want %t", got, want)
			}
		})
	}
}

func TestNormalizeResponsesPatchHistoryAndChoice(t *testing.T) {
	const request = `{"tools":[{"type":"namespace","name":"outer","children":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}]}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"custom","namespace":"outer__editor","name":"apply_patch"}]},"input":[{"type":"custom_tool_call","call_id":"call-1","namespace":"outer__editor","name":"apply_patch","input":" {\"raw\":true} "},{"type":"custom_tool_call_output","call_id":"call-1","output":"done"}]}`
	body, err := NormalizeResponsesPatchTools([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "tool_choice.tools.0.type").String() != "function" || gjson.GetBytes(body, "input.0.name").String() != "outer__editor__apply_patch" || gjson.GetBytes(body, "input.0.namespace").Exists() {
		t.Fatalf("identity or choice: %s", body)
	}
	if gjson.GetBytes(body, "input.0.arguments").String() != `{"input":" {\"raw\":true} "}` || gjson.GetBytes(body, "input.1.type").String() != "function_call_output" {
		t.Fatalf("history wrapper: %s", body)
	}
	for _, invalid := range []string{`not JSON`, `{"input":[{"type":"custom_tool_call","name":"apply_patch","input":{}}]}`, `{"input":[{"type":"custom_tool_call","name":"apply_patch","input":"\ud800"}]}`} {
		if _, err = NormalizeResponsesPatchTools([]byte(invalid)); err == nil {
			t.Errorf("accepted invalid history %s", invalid)
		}
	}
}

func TestResponsesPatchBridgeTerminalAndIdentity(t *testing.T) {
	const request = `{"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}]}`
	for _, test := range []struct {
		name    string
		added   string
		done    string
		invalid bool
	}{
		{name: "namespace", added: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch"}}`, done: `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch","arguments":"{\"input\":\"patch 😀\"}"}}`},
		{name: "wrong item ID", added: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch"}}`, done: `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"item-2","call_id":"call-1","name":"editor__apply_patch","arguments":"{\"input\":\"patch\"}"}}`, invalid: true},
		{name: "wrong index", added: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch"}}`, done: `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch","arguments":"{\"input\":\"patch\"}"}}`, invalid: true},
		{name: "wrong call ID", added: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor__apply_patch"}}`, done: `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-2","name":"editor__apply_patch","arguments":"{\"input\":\"patch\"}"}}`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bridge := NewResponsesPatchBridge([]byte(request))
			if _, err := bridge.Transform([]byte(test.added)); err != nil {
				t.Fatal(err)
			}
			events, err := bridge.Transform([]byte(test.done))
			if (err != nil) != test.invalid {
				t.Fatalf("error=%v, want invalid=%t; events=%q", err, test.invalid, events)
			}
			if !test.invalid {
				item := gjson.GetBytes(events[len(events)-1], "item")
				if item.Get("name").String() != "apply_patch" || item.Get("namespace").String() != "editor" || item.Get("input").String() != "patch 😀" {
					t.Fatalf("reversible patch: %s", item.Raw)
				}
			}
		})
	}
}

func TestResponsesPatchBridgeRejectsUnfinishedAndConflictingCalls(t *testing.T) {
	const request = `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, terminal := range []string{
		`{"type":"response.completed","response":{"output":[]}}`,
		`{"type":"response.incomplete","response":{"output":[]}}`,
		`{"type":"response.completed","response":{"output":[{"id":"item-1","call_id":"call-1","type":"function_call","name":"apply_patch","arguments":"{\"input\":\"different\"}"}]}}`,
	} {
		bridge := NewResponsesPatchBridge([]byte(request))
		if _, err := bridge.Transform([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := bridge.Transform([]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"item-1","delta":"{\"input\":\"partial"}`)); err != nil {
			t.Fatal(err)
		}
		if events, err := bridge.Transform([]byte(terminal)); err == nil || len(events) != 0 {
			t.Errorf("accepted terminal %s: events=%q error=%v", terminal, events, err)
		}
		if events, err := bridge.Transform([]byte(`{"type":"response.completed","response":{"output":[]}}`)); err == nil || len(events) != 0 {
			t.Fatal("cleared retained input failure")
		}
	}
	bridge := NewResponsesPatchBridge([]byte(request))
	const arguments = `{"input":"patch"}`
	terminal := fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"id":"item-1","call_id":"call-1","type":"function_call","name":"apply_patch","arguments":%q}]}}`, arguments)
	events, err := bridge.Transform([]byte(terminal))
	if err != nil || len(events) != 5 || gjson.GetBytes(events[4], "response.output.0.input").String() != "patch" {
		t.Fatalf("terminal-only bridge: %q %v", events, err)
	}
	wantTypes := []string{"response.output_item.added", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.output_item.done", "response.completed"}
	for index, event := range events {
		if kind := gjson.GetBytes(event, "type").String(); kind != wantTypes[index] {
			t.Fatalf("terminal-only lifecycle: %s", event)
		}
		if sequence := gjson.GetBytes(event, "sequence_number").Int(); sequence != int64(index+1) || strings.Contains(gjson.GetBytes(event, "type").String(), "function_call_arguments") {
			t.Fatalf("event sequence or type: %s", event)
		}
	}
}
