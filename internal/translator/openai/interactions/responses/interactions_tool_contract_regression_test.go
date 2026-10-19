package responses

import (
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRegisteredInteractionsRejectsConflictingToolIdentity(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, field := range []string{"id", "call_id", "name"} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				name := "tool"
				request := []byte(`{"tools":[{"type":"function","name":"tool","parameters":{"type":"object"}}]}`)
				if kind == "custom" {
					name = "apply_patch"
					request = []byte(`{"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}]}`)
				}
				start := []byte(`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","arguments":{}}}`)
				start, _ = sjson.SetBytes(start, "step.name", name)
				conflict, _ := sjson.SetBytes(start, "step."+field, "conflicting-value")
				var state any
				var output [][]byte
				for _, raw := range [][]byte{start, conflict,
					[]byte(`{"event_type":"step.stop","index":0}`),
					[]byte(`{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed"}}`),
				} {
					output = append(output, sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatInteractions,
						sdktranslator.FormatOpenAIResponse, "devin/swe-2", request, nil, raw, &state)...)
				}
				if err := sdktranslator.ResponseTranslationError(&state); err == nil {
					t.Fatalf("conflicting %s was accepted: %q", field, output)
				}
				for _, chunk := range output {
					if strings.Contains(string(chunk), `"type":"response.completed"`) {
						t.Fatalf("identity failure produced successful completion: %s", chunk)
					}
				}
			})
		}
	}
}

func TestRegisteredInteractionsCustomToolAndReasoningLifecycle(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"write","format":{"type":"text"}}]}`)
	var state any
	var events [][]byte
	for _, raw := range []string{
		`{"event_type":"step.start","index":0,"step":{"type":"thought","id":"reason-1"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","text":"one"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","text":"two"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","text":"ignored"}}`,
		`{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"write"}}`,
		`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"<body> & data\"}"}}`,
		`{"event_type":"step.stop","index":1}`,
		`{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"cached_tokens":0}}}`,
	} {
		events = append(events, sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatInteractions,
			sdktranslator.FormatOpenAIResponse, "devin/swe-2", request, nil, []byte(raw), &state)...)
	}
	if err := sdktranslator.ResponseTranslationError(&state); err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	var completed gjson.Result
	for _, event := range events {
		payload := responseEventPayload(event)
		kind := payload.Get("type").String()
		counts[kind]++
		if kind == "response.function_call_arguments.delta" || kind == "response.function_call_arguments.done" {
			t.Fatalf("custom input emitted function arguments: %s", event)
		}
		if kind == "response.custom_tool_call_input.delta" {
			if !strings.HasPrefix(string(event), "event: "+kind+"\n") || payload.Get("delta").String() != "<body> & data" {
				t.Fatalf("custom delta event changed: %s", event)
			}
		}
		if kind == "response.reasoning_summary_text.done" && payload.Get("text").String() != "onetwo" {
			t.Fatalf("reasoning summary changed: %s", event)
		}
		if kind == "response.completed" {
			completed = payload.Get("response")
		}
	}
	for _, kind := range []string{"response.reasoning_summary_part.added", "response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.completed"} {
		if counts[kind] != 1 {
			t.Fatalf("%s count = %d, want 1: %q", kind, counts[kind], events)
		}
	}
	if completed.Get("output.0.summary.0.text").String() != "onetwo" || completed.Get("output.1.type").String() != "custom_tool_call" ||
		completed.Get("output.1.input").String() != "<body> & data" || completed.Get("usage.total_tokens").Int() != 5 ||
		!completed.Get("usage.input_tokens_details.cached_tokens").Exists() {
		t.Fatalf("completed output or usage changed: %s", completed.Raw)
	}
}
