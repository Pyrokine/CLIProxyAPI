package responses_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsSDKPreNameOrdinaryTypeCompatibility(t *testing.T) {
	for _, declarations := range []struct{ tools, name string }{
		{
			`[{"type":"custom","name":"apply_patch"},{"type":"function","name":` +
				`"lookup","parameters":{"type":` +
				`"object"}}]`,
			"lookup",
		},
		{
			`[{"type":"function","name":"apply_patch","parameters":{"type":"object"}},{"type":"namespace","` +
				`name":"ns","tools":[{"type":"custom","name":"apply_patch"}]}]`,
			"apply_patch",
		},
	} {
		for _, kind := range []string{"", "model_output", "thought", "custom_tool_call"} {
			for _, boundary := range []string{"step.start", "step.stop", "interaction.completed", "finish"} {
				t.Run(
					declarations.name+"/"+kind+"/"+boundary, func(t *testing.T) {
						original := []byte(`{"tools":` + declarations.tools + `}`)
						var state any
						step := fmt.Sprintf(
							`{"index":2,"id":"ordinary","call_id":"ordinary-call","name":%q}`,
							declarations.name,
						)
						if kind != "" {
							step = strings.TrimSuffix(step, "}") + fmt.Sprintf(`,"type":%q}`, kind)
						}
						events := []string{
							`{"event_type":"step.start","index":2,"step":{"type":"function_call","i` +
								`d":"ordinary","call_id":"ordinary-call","arguments":{}}}`,
							`{"event_type":"step.delta","index":2,"delta":{"type":` +
								`"arguments_delta","arguments":"{\"x\":` +
								`1}"}}`,
						}
						if boundary == "step.start" || boundary == "step.stop" {
							events = append(
								events, `{"event_type":"`+boundary+`","index":2,"step":`+step+`}`,
								`{"event_type":"step.stop","index":2}`,
								`{"event_type":"interaction.completed","interaction":{"status":"completed"}}`,
							)
						} else {
							events = append(
								events,
								`{"event_type":"`+boundary+`","interaction":{"status":"completed","steps":[`+step+`]}}`,
							)
						}
						var terminal gjson.Result
						for _, event := range events {
							for _, frame := range sdk.Default().TranslateStream(
								context.Background(), sdk.FormatInteractions, sdk.FormatOpenAIResponse, "fixture",
								original, nil, []byte(event), &state,
							) {
								for line := range strings.SplitSeq(string(frame), "\n") {
									if data, ok := strings.CutPrefix(line, "data: "); ok {
										item := gjson.Parse(data)
										if item.Get("type").String() == "response.failed" {
											t.Fatalf("ordinary type rejected: %s", data)
										}
										if item.Get("type").String() == "response.completed" {
											terminal = item.Get("response")
										}
									}
								}
							}
						}
						if err := sdk.ResponseTranslationError(&state); err != nil {
							t.Fatal(err)
						}
						if terminal.Get("output.0.type").String() != "function_call" ||
							terminal.Get("output.0.name").String() != declarations.name ||
							terminal.Get("output.0.arguments").String() != `{"x":1}` {
							t.Fatalf("ordinary tool changed: %s", terminal.Raw)
						}
					},
				)
			}
		}
	}
}

func TestInteractionsSDKOrdinaryExplicitIndexSeparatesReusedIDs(t *testing.T) {
	original := []byte(
		`{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function",` +
			`"name":"lookup","parameters":{"type":` +
			`"object"}}]}`,
	)
	var state any
	var frames [][]byte
	for _, index := range []int{2, 3} {
		for _, event := range []string{
			fmt.Sprintf(
				`{"event_type":"step.start","index":%d,"step":{"type":"function_call","`+
					`id":"ordinary","call_id":"ordinary-call","name":"lookup","arguments":{`+
					`"x":%d}}}`,
				index, index,
			), fmt.Sprintf(`{"event_type":"step.stop","index":%d}`, index),
		} {
			frames = append(
				frames, sdk.Default().TranslateStream(
					context.Background(),
					sdk.FormatInteractions, sdk.FormatOpenAIResponse, "fixture", original, nil,
					[]byte(event), &state,
				)...,
			)
		}
	}
	frames = append(
		frames, sdk.Default().TranslateStream(
			context.Background(), sdk.FormatInteractions, sdk.FormatOpenAIResponse, "fixture", original, nil,
			[]byte(`{"event_type":"interaction.completed","interaction":{"status":"completed"}}`), &state,
		)...,
	)
	if err := sdk.ResponseTranslationError(&state); err != nil {
		t.Fatal(err)
	}
	var terminal gjson.Result
	for _, frame := range frames {
		for line := range strings.SplitSeq(string(frame), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok && gjson.Get(
				data, "type",
			).String() == "response.completed" {
				terminal = gjson.Get(data, "response")
			}
		}
	}
	if terminal.Get("output.0.arguments").String() != `{"x":2}` ||
		terminal.Get("output.1.arguments").String() != `{"x":3}` {
		t.Fatalf("ordinary explicit indexes merged: %s", terminal.Raw)
	}
}
