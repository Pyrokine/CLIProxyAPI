package common_test

import (
	"context"
	"fmt"
	"testing"

	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResponsesPatchSDKRestoresTerminalOutputIndexOrder(t *testing.T) {
	for _, patchIndex := range []int{0, 2} {
		r := sdk.NewRegistry()
		r.Register(
			sdk.FormatOpenAIResponse,
			sdk.FormatOpenAI,
			func(_ string, body []byte, _ bool) []byte { return body },
			sdk.ResponseTransform{
				Stream: func(
					_ context.Context,
					_ string,
					_, _, body []byte,
					_ *any,
				) [][]byte {
					return [][]byte{body}
				},
			},
		)
		original := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
		var state any
		patch := `{"id":"patch-item","type":"function_call","call_id":"call-a","name":` +
			`"apply_patch","arguments":"{\"input\":` +
			`\"patch\"}"}`
		for _, event := range []string{
			fmt.Sprintf(
				`{"type":"response.output_item.done","output_index":%d,"item":%s}`, patchIndex, patch,
			), fmt.Sprintf(
				`{"type":"response.output_item.added","output_index":%d,"item":{"id":"message-item","type":"`+
					`message","role":"assistant","content":[]}}`,
				patchIndex+1,
			),
		} {
			r.TranslateStream(
				context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", original, nil,
				[]byte(event), &state,
			)
		}
		frames := r.TranslateStream(
			context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", original, nil,
			[]byte(`{"type":"response.completed","response":{"status":"completed","output"`+
				`:[{"id":"message-item","type":"message","role":"assistant","content":[`+
				`{"type":"output_text","text":"answer"}]}]}}`),
			&state,
		)
		if err := sdk.ResponseTranslationError(&state); err != nil {
			t.Fatal(err)
		}
		if len(frames) == 0 {
			t.Fatal("missing terminal")
		}
		terminal := gjson.GetBytes(frames[len(frames)-1], "response.output")
		if terminal.Get("0.id").String() != "patch-item" || terminal.Get("1.id").String() != "message-item" ||
			terminal.Get("0.input").String() != "patch" {
			t.Fatalf("terminal output_index order changed: %s", terminal.Raw)
		}
	}
}
