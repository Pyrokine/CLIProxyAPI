package responses_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/interactions/chat-completions"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/interactions/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsSDKInterleavedToolArgumentsRoundTrip(t *testing.T) {
	for _, source := range []sdk.Format{sdk.FormatOpenAI, sdk.FormatOpenAIResponse} {
		var events []string
		if source == sdk.FormatOpenAI {
			events = []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a",` +
					`"function":{"name":"alpha","arguments":"{\"a\":` +
					`"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call-b",` +
					`"function":{"name":"beta","arguments":"{\"b\":` +
					`"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `[DONE]`,
			}
		} else {
			events = []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"` +
					`item-a","call_id":"call-a","name":"alpha","arguments":""}}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"` +
					`item-b","call_id":"call-b","name":"beta","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","item_id":"item-a","delta":"{\"a\":"}`,
				`{"type":"response.function_call_arguments.delta","item_id":"item-b","delta":"{\"b\":"}`,
				`{"type":"response.function_call_arguments.delta","item_id":"item-a","delta":"1}"}`,
				`{"type":"response.function_call_arguments.delta","item_id":"item-b","delta":"2}"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"` +
					`item-a","call_id":"call-a","name":"alpha","arguments":"{\"a\":1}"}}`,
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"f` +
					`unction_call","id":"item-b","call_id":"call-b","name":"beta","argument` +
					`s":"{\"b\":2}"}}`,
				`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
			}
		}
		var forward, reverse any
		ids := map[int]string{}
		args := map[int]string{}
		starts, stops := 0, 0
		for _, event := range events {
			for _, frame := range sdk.Default().TranslateStream(
				context.Background(), source, sdk.FormatInteractions, "fixture", nil, nil, []byte(event), &forward,
			) {
				for line := range strings.SplitSeq(string(frame), "\n") {
					if payload, ok := strings.CutPrefix(line, "data: "); ok {
						switch gjson.Get(payload, "event_type").String() {
						case "step.start":
							starts++
						case "step.stop":
							stops++
						}
					}
				}
				for _, back := range sdk.Default().TranslateStream(
					context.Background(), sdk.FormatInteractions, sdk.FormatOpenAI, "fixture", nil, nil,
					frame, &reverse,
				) {
					for _, call := range gjson.GetBytes(back, "choices.0.delta.tool_calls").Array() {
						index := int(call.Get("index").Int())
						if id := call.Get("id").String(); id != "" {
							ids[index] = id
						}
						args[index] += call.Get("function.arguments").String()
					}
				}
			}
		}
		if starts != 2 || stops != 2 || ids[0] != "call-a" || ids[1] != "call-b" || args[0] != `{"a":1}` ||
			args[1] != `{"b":2}` {
			t.Fatalf(
				"%s identity/arguments/lifecycle changed: starts=%d stops=%d ids=%v args=%v",
				source,
				starts,
				stops,
				ids,
				args,
			)
		}
	}
}

func TestInteractionsSDKResponsesIgnoresEventsAfterCompletion(t *testing.T) {
	var state any
	sdk.Default().TranslateStream(
		context.Background(), sdk.FormatOpenAIResponse, sdk.FormatInteractions, "fixture", nil, nil,
		[]byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`), &state,
	)
	for _, event := range []string{
		`{"type":"response.output_text.delta","delta":"late"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","name":"late"}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`, `[DONE]`,
	} {
		if out := sdk.Default().TranslateStream(
			context.Background(), sdk.FormatOpenAIResponse, sdk.FormatInteractions, "fixture", nil, nil, []byte(event),
			&state,
		); len(out) != 0 {
			t.Fatalf("late event emitted: %s", out)
		}
	}
}

func TestInteractionsSDKNormalizesAttachmentAndMessageTypes(t *testing.T) {
	for _, tc := range []struct {
		format sdk.Format
		body   string
	}{
		{sdk.FormatOpenAI, `{"messages":[{"role":"user","content":[{"type":" INPUT_AUDIO "}]}]}`},
		{sdk.FormatOpenAIResponse, `{"input":[{"role":"user","content":[{"type":"input_image"}]}]}`},
	} {
		out, err := sdk.TranslateRequestChecked(
			context.Background(), tc.format, sdk.FormatInteractions, "fixture", []byte(tc.body), false,
		)
		var unsupported *common.UnsupportedPartError
		if !errors.As(err, &unsupported) || len(out) != 0 {
			t.Fatalf("%s attachment accepted: error=%v body=%s", tc.format, err, out)
		}
	}
	out, err := sdk.TranslateRequestChecked(
		context.Background(),
		sdk.FormatOpenAIResponse,
		sdk.FormatInteractions,
		"fixture",
		[]byte(
			`{"input":[{"role":"user","content":[{"type":"input_text","text":"visible"},{"type":"input_image"}]}]}`,
		),
		false,
	)
	if err != nil || gjson.GetBytes(out, "input.0.content.0.text").String() != "visible" {
		t.Fatalf("mixed text control rejected: error=%v body=%s", err, out)
	}
}
