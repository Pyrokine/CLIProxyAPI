package responses

import (
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const interactionsPatchSourceRequest = `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
const interactionsPatchSourceStep = `{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":{"input":"pq"}}`
const interactionsPatchSourceTerminal = `{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed"}}`

func runInteractionsPatchSource(t *testing.T, request string, frames []string) ([][]byte, error) {
	t.Helper()
	var state any
	var output [][]byte
	for _, frame := range frames {
		output = append(output, sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatInteractions,
			sdktranslator.FormatOpenAIResponse, "devin/swe-2", []byte(request), nil, []byte(frame), &state)...)
	}
	return output, sdktranslator.ResponseTranslationError(&state)
}

func assertInteractionsPatchSourceOutput(t *testing.T, output [][]byte, itemID, callID string) {
	t.Helper()
	counts := make(map[string]int)
	var input strings.Builder
	for _, frame := range output {
		root := responseEventPayload(frame)
		kind := root.Get("type").String()
		counts[kind]++
		if kind == "response.failed" || strings.HasPrefix(kind, "response.function_call_arguments.") {
			t.Fatalf("patch source produced an invalid client event: %s", frame)
		}
		identity, idKey := root, "item_id"
		if root.Get("item").Exists() {
			identity, idKey = root.Get("item"), "id"
		}
		if kind == "response.completed" {
			identity, idKey = root.Get("response.output.0"), "id"
		}
		if identity.Get(idKey).String() != itemID || identity.Get("call_id").String() != callID {
			t.Fatalf("source identity changed: %s", frame)
		}
		if identity.Get("type").String() == "custom_tool_call" && identity.Get("name").String() != "apply_patch" {
			t.Fatalf("source tool name changed: %s", frame)
		}
		if kind == "response.custom_tool_call_input.delta" {
			input.WriteString(root.Get("delta").String())
		}
		if kind == "response.custom_tool_call_input.done" && root.Get("input").String() != "pq" {
			t.Fatalf("source input completion changed: %s", frame)
		}
		if kind == "response.completed" && identity.Get("input").String() != "pq" {
			t.Fatalf("source terminal snapshot changed: %s", frame)
		}
	}
	if input.String() != "pq" {
		t.Fatalf("source fragments = %q, want pq", input.String())
	}
	for _, kind := range []string{"response.output_item.added", "response.custom_tool_call_input.done", "response.output_item.done", "response.completed"} {
		if counts[kind] != 1 {
			t.Fatalf("source lifecycle counts = %v", counts)
		}
	}
}

func TestRegisteredInteractionsPatchSourceLateIdentity(t *testing.T) {
	for _, first := range []string{"item", "call", "neither", "unnamed"} {
		for _, boundary := range []string{"start", "delta", "stop", "terminal"} {
			t.Run(first+"/"+boundary, func(t *testing.T) {
				initial := []byte(interactionsPatchSourceStep)
				initial, _ = sjson.SetRawBytes(initial, "arguments", []byte(`{}`))
				if first == "item" || first == "neither" {
					initial, _ = sjson.DeleteBytes(initial, "call_id")
				}
				if first == "call" || first == "neither" {
					initial, _ = sjson.DeleteBytes(initial, "id")
				}
				if first == "unnamed" {
					initial, _ = sjson.DeleteBytes(initial, "name")
				}
				frames := []string{
					`{"event_type":"step.start","index":0,"step":` + string(initial) + `}`,
					`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"p"}}`,
				}
				update := interactionsPatchSourceStep
				if boundary == "start" {
					frames = append(frames, `{"event_type":"step.start","index":0,"step":`+update+`}`)
				}
				lastDelta := `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"q\"}"}}`
				if boundary == "delta" {
					body, _ := sjson.SetRawBytes([]byte(lastDelta), "step", []byte(update))
					lastDelta = string(body)
				}
				frames = append(frames, lastDelta)
				stop := `{"event_type":"step.stop","index":0}`
				if boundary == "stop" {
					stop = `{"event_type":"step.stop","index":0,"step":` + update + `}`
				}
				frames = append(frames, stop)
				terminal := interactionsPatchSourceTerminal
				if boundary == "terminal" {
					body, _ := sjson.SetRawBytes([]byte(terminal), "interaction.steps", []byte(`[`+update+`]`))
					terminal = string(body)
				}
				frames = append(frames, terminal)
				output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, frames)
				if err != nil {
					t.Fatalf("late source identity was rejected: %v; output=%q", err, output)
				}
				assertInteractionsPatchSourceOutput(t, output, "item-1", "call-1")
			})
		}
	}
}

func TestRegisteredInteractionsPatchSourceSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"initial snapshot", []string{`{"event_type":"step.start","index":0,"step":` + interactionsPatchSourceStep + `}`, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal}},
		{"stop snapshot", []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`, `{"event_type":"step.stop","index":0,"step":` + interactionsPatchSourceStep + `}`, interactionsPatchSourceTerminal}},
		{"terminal snapshot", []string{`{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed","steps":[` + interactionsPatchSourceStep + `]}}`}},
		{"arguments before start", []string{`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"pq\"}"}}`, `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal}},
		{"terminal array position is not source index", []string{`{"event_type":"step.start","index":3,"step":` + interactionsPatchSourceStep + `}`, `{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed","steps":[` + interactionsPatchSourceStep + `]}}`}},
		{"large sparse source index", []string{`{"event_type":"step.start","index":1073741824,"step":` + interactionsPatchSourceStep + `}`, interactionsPatchSourceTerminal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, tc.frames)
			if err != nil {
				t.Fatalf("source snapshot was rejected: %v; output=%q", err, output)
			}
			assertInteractionsPatchSourceOutput(t, output, "item-1", "call-1")
		})
	}
}

func TestRegisteredInteractionsPatchSourceRejectsContradictions(t *testing.T) {
	start := `{"event_type":"step.start","index":0,"step":` + interactionsPatchSourceStep + `}`
	stop := `{"event_type":"step.stop","index":0}`
	cases := []struct {
		name   string
		frames []string
	}{
		{"delta after source stop", []string{start, stop, `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":" "}}`, interactionsPatchSourceTerminal}},
		{"legacy freeform delta", []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`, `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"pq\"}","invalid_json_str":"legacy"}}`, stop, interactionsPatchSourceTerminal}},
		{"malformed envelope", []string{`{"event_type":"interaction.created","interaction":{"id":"interaction-1"}`, start, stop, interactionsPatchSourceTerminal}},
		{"malformed source step", []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":{"input":"pq"}}`, stop, interactionsPatchSourceTerminal}},
		{"noninteger source index", []string{`{"event_type":"step.start","index":0.5,"step":` + interactionsPatchSourceStep + `}`, stop, interactionsPatchSourceTerminal}},
		{"nonstring source id", []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":23,"call_id":"call-1","name":"apply_patch","arguments":{"input":"pq"}}}`, stop, interactionsPatchSourceTerminal}},
		{"unnamed source never resolved", []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","arguments":{"input":"pq"}}}`, stop, interactionsPatchSourceTerminal}},
		{"upstream failed", []string{start, `{"event_type":"interaction.failed","error":{"message":"synthetic rejection"}}`}},
		{"done before JSON terminal", []string{start, stop, `[DONE]`}},
		{"late patch after unresolved source stop", []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1"}}`, `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"pq\"}"}}`, stop, `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":" "}}`, start, interactionsPatchSourceTerminal}},
		{"source type before function", []string{`{"event_type":"step.start","index":0,"step":{"type":"model_output","id":"item-1"}}`, start, stop, interactionsPatchSourceTerminal}},
	}
	for _, boundary := range []string{"start", "stop", "terminal"} {
		for _, field := range []string{"id", "call_id", "name", "type", "arguments"} {
			changed := []byte(interactionsPatchSourceStep)
			if field == "arguments" {
				changed, _ = sjson.SetRawBytes(changed, field, []byte(`{"input":"different"}`))
			} else {
				changed, _ = sjson.SetBytes(changed, field, "conflicting")
			}
			frames := []string{start}
			switch boundary {
			case "start":
				frames = append(frames, `{"event_type":"step.start","index":0,"step":`+string(changed)+`}`, stop, interactionsPatchSourceTerminal)
			case "stop":
				frames = append(frames, `{"event_type":"step.stop","index":0,"step":`+string(changed)+`}`, interactionsPatchSourceTerminal)
			case "terminal":
				frames = append(frames, stop, `{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed","steps":[`+string(changed)+`]}}`)
			}
			cases = append(cases, struct {
				name   string
				frames []string
			}{boundary + "/" + field, frames})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, tc.frames)
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != 502 {
				t.Fatalf("source contradiction was accepted: err=%v; output=%q", err, output)
			}
			failed := 0
			for _, frame := range output {
				kind := responseEventPayload(frame).Get("type").String()
				if kind == "response.completed" {
					t.Fatalf("source contradiction produced success: %s", frame)
				}
				if kind == "response.failed" {
					failed++
				}
			}
			if failed != 1 {
				t.Fatalf("source failure count = %d: %q", failed, output)
			}
		})
	}
}

func TestRegisteredInteractionsPatchSourceTerminalFallback(t *testing.T) {
	for _, missing := range []string{"id", "call_id", "both"} {
		t.Run(missing, func(t *testing.T) {
			step := []byte(interactionsPatchSourceStep)
			if missing == "id" || missing == "both" {
				step, _ = sjson.DeleteBytes(step, "id")
			}
			if missing == "call_id" || missing == "both" {
				step, _ = sjson.DeleteBytes(step, "call_id")
			}
			frames := []string{`{"event_type":"step.start","index":0,"step":` + string(step) + `}`, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal}
			output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, frames)
			if err != nil {
				t.Fatal(err)
			}
			id := "item-1"
			if missing == "id" {
				id = "call-1"
			} else if missing == "both" {
				id = "item_0"
			}
			assertInteractionsPatchSourceOutput(t, output, id, id)
		})
	}
}

func TestRegisteredInteractionsPatchSourceAliasConflicts(t *testing.T) {
	for _, field := range []string{"index", "id", "call_id"} {
		t.Run(field, func(t *testing.T) {
			second := `{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"item-2","call_id":"call-2","name":"apply_patch","arguments":{"input":"pq"}}}`
			conflict := []byte(`{"event_type":"step.stop","index":0,"step":` + interactionsPatchSourceStep + `}`)
			path, value := "step."+field, any("item-2")
			if field == "index" {
				path, value = "step.index", 1
			} else if field == "call_id" {
				value = "call-2"
			}
			conflict, _ = sjson.SetBytes(conflict, path, value)
			output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, []string{`{"event_type":"step.start","index":0,"step":` + interactionsPatchSourceStep + `}`, second, string(conflict), interactionsPatchSourceTerminal})
			if err == nil {
				t.Fatalf("contradictory source aliases were accepted: %q", output)
			}
		})
	}
}

func TestRegisteredInteractionsPatchSourceNamespacedDeclaration(t *testing.T) {
	for _, additional := range []bool{false, true} {
		t.Run(fmt.Sprint(additional), func(t *testing.T) {
			tools := `[{"type":"namespace","name":"files","tools":[{"type":"custom","name":"apply_patch"}]}]`
			request := `{"tools":` + tools + `}`
			if additional {
				request = `{"input":[{"type":"additional_tools","tools":` + tools + `}]}`
			}
			step, _ := sjson.SetBytes([]byte(interactionsPatchSourceStep), "name", "files__apply_patch")
			output, err := runInteractionsPatchSource(t, request, []string{`{"event_type":"step.start","index":0,"step":` + string(step) + `}`, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal})
			if err != nil {
				t.Fatal(err)
			}
			assertInteractionsPatchSourceOutput(t, output, "item-1", "call-1")
			for _, frame := range output {
				root := responseEventPayload(frame)
				item := root.Get("item")
				if root.Get("type").String() == "response.completed" {
					item = root.Get("response.output.0")
				}
				if item.Exists() && item.Get("namespace").String() != "files" {
					t.Fatalf("source namespace changed: %s", frame)
				}
			}
		})
	}
}

func TestRegisteredInteractionsPatchSourcePreservesOrdinaryTools(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, lateName := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, lateName), func(t *testing.T) {
				request := `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"` + kind + `","name":"lookup"}]}`
				step := []byte(`{"type":"function_call","id":"item-1","call_id":"call-1","name":"lookup"}`)
				if lateName {
					step, _ = sjson.DeleteBytes(step, "name")
				}
				frames := []string{`{"event_type":"step.start","index":0,"step":` + string(step) + `}`,
					`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"pq\"}","invalid_json_str":"ordinary compatibility"}}`,
					`{"event_type":"step.stop","index":0}`,
					`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"ignored after ordinary stop"}}`,
				}
				if lateName {
					frames = append(frames, `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"lookup"}}`)
				}
				frames = append(frames, interactionsPatchSourceTerminal)
				output, err := runInteractionsPatchSource(t, request, frames)
				if err != nil {
					t.Fatalf("ordinary tool was subjected to patch-only rejection: %v", err)
				}
				added, completed := 0, 0
				for _, frame := range output {
					root := responseEventPayload(frame)
					if root.Get("type").String() == "response.output_item.added" {
						added++
					}
					if root.Get("type").String() != "response.completed" {
						continue
					}
					completed++
					item := root.Get("response.output.0")
					wantType, field, want := "function_call", "arguments", `{"input":"pq"}`
					if kind == "custom" {
						wantType, field, want = "custom_tool_call", "input", "pq"
					}
					if item.Get("type").String() != wantType || item.Get(field).String() != want {
						t.Fatalf("ordinary source result changed: %s", frame)
					}
				}
				if added != 1 || completed != 1 {
					t.Fatalf("ordinary source lifecycle duplicated: added=%d completed=%d", added, completed)
				}
			})
		}
	}
}

func TestRegisteredInteractionsPatchSourceRejectsInvalidSnapshots(t *testing.T) {
	for _, arguments := range []string{`{"input":23}`, `{"input":"pq","extra":1}`, `{"input":"partial`, `{"input":"\ud800"}`} {
		for _, boundary := range []string{"start", "stop", "terminal", "before name"} {
			t.Run(arguments+"/"+boundary, func(t *testing.T) {
				step, _ := sjson.SetBytes([]byte(interactionsPatchSourceStep), "arguments", arguments)
				frames := []string{`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`}
				switch boundary {
				case "start":
					frames = []string{`{"event_type":"step.start","index":0,"step":` + string(step) + `}`, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal}
				case "stop":
					frames = append(frames, `{"event_type":"step.stop","index":0,"step":`+string(step)+`}`, interactionsPatchSourceTerminal)
				case "terminal":
					frames = append(frames, `{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed","steps":[`+string(step)+`]}}`)
				case "before name":
					step, _ = sjson.DeleteBytes(step, "name")
					frames = []string{`{"event_type":"step.start","index":0,"step":` + string(step) + `}`, `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal}
				}
				output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, frames)
				if err == nil {
					t.Fatalf("invalid full source snapshot was accepted: %q", output)
				}
			})
		}
	}
}

func TestRegisteredInteractionsPatchSourceRejectsEmptySnapshotAfterFragments(t *testing.T) {
	for _, boundary := range []string{"start", "stop", "terminal", "before name"} {
		t.Run(boundary, func(t *testing.T) {
			step := []byte(`{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}`)
			if boundary == "before name" {
				step, _ = sjson.DeleteBytes(step, "name")
			}
			frames := []string{`{"event_type":"step.start","index":0,"step":` + string(step) + `}`,
				`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"pq\"}"}}`,
			}
			snapshot, _ := sjson.SetBytes(step, "arguments", "")
			switch boundary {
			case "start":
				frames = append(frames, `{"event_type":"step.start","index":0,"step":`+string(snapshot)+`}`, `{"event_type":"step.stop","index":0}`)
			case "stop":
				frames = append(frames, `{"event_type":"step.stop","index":0,"step":`+string(snapshot)+`}`)
			case "terminal":
				terminal, _ := sjson.SetRawBytes([]byte(interactionsPatchSourceTerminal), "interaction.steps", []byte(`[`+string(snapshot)+`]`))
				frames = append(frames, string(terminal))
			case "before name":
				frames = append(frames, `{"event_type":"step.start","index":0,"step":`+string(snapshot)+`}`, `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`, `{"event_type":"step.stop","index":0}`)
			}
			frames = append(frames, interactionsPatchSourceTerminal)
			output, err := runInteractionsPatchSource(t, interactionsPatchSourceRequest, frames)
			if err == nil {
				t.Fatalf("empty complete source snapshot was hidden by valid fragments: %q", output)
			}
		})
	}
}

func TestRegisteredInteractionsPatchPreservesOrdinaryArgumentPrefixes(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, named := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, named), func(t *testing.T) {
				request := `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"` + kind + `","name":"lookup"}]}`
				name := ""
				if named {
					name = `,"name":"lookup"`
				}
				frames := []string{
					`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1"` + name + `,"arguments":"{\"input\":\"p"}}`,
					`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"q\"}"}}`,
				}
				if !named {
					frames = append(frames, `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"lookup"}}`)
				}
				frames = append(frames, `{"event_type":"step.stop","index":0}`, interactionsPatchSourceTerminal)
				output, err := runInteractionsPatchSource(t, request, frames)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, frame := range output {
					root := responseEventPayload(frame)
					if root.Get("type").String() != "response.completed" {
						continue
					}
					found = true
					field, want := "arguments", `{"input":"pq"}`
					if kind == "custom" {
						field, want = "input", "pq"
					}
					if root.Get("response.output.0."+field).String() != want {
						t.Fatalf("ordinary argument prefix was lost: %s", frame)
					}
				}
				if !found {
					t.Fatalf("ordinary argument stream did not complete: %q", output)
				}
			})
		}
	}
}

func TestRegisteredInteractionsPatchRetainsTypeEvidenceBeforeArguments(t *testing.T) {
	var state any
	frames := []string{
		`{"event_type":"step.start","index":0,"step":{"type":"model_output","id":"item-1"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"pq\"}"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch"}}`,
		`{"event_type":"step.stop","index":0}`,
		interactionsPatchSourceTerminal,
	}
	var output [][]byte
	for _, frame := range frames {
		output = append(output, sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatInteractions,
			sdktranslator.FormatOpenAIResponse, "devin/swe-2", []byte(interactionsPatchSourceRequest), nil, []byte(frame), &state)...)
	}
	if sdktranslator.ResponseTranslationError(&state) == nil {
		t.Fatalf("source type contradiction disappeared after arguments arrived: %q", output)
	}
}

func TestRegisteredInteractionsPatchSourceNonStream(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			body := []byte(`{"id":"interaction-1","status":"completed","steps":[` + interactionsPatchSourceStep + `]}`)
			if !valid {
				body = body[:len(body)-1]
			}
			var state any
			out := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatInteractions, sdktranslator.FormatOpenAIResponse,
				"devin/swe-2", []byte(interactionsPatchSourceRequest), nil, body, &state)
			err := sdktranslator.ResponseTranslationError(&state)
			if valid {
				if err != nil || gjson.GetBytes(out, "output.0.input").String() != "pq" {
					t.Fatalf("source nonstream result changed: %s; err=%v", out, err)
				}
			} else if err == nil {
				t.Fatalf("malformed source nonstream envelope was accepted: %s", out)
			}
		})
	}
}
