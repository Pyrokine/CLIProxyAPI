package openai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketStopsAtFirstProtocolTerminal(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete"} {
		for _, suffix := range []struct{ name, payload string }{
			{name: "no late frame"},
			{name: "late text", payload: `{"type":"response.output_text.delta","delta":"late"}`},
			{
				name:    "duplicate completion",
				payload: `{"type":"response.completed","response":{"id":"late-response","output":[]}}`,
			},
			{name: "late error", payload: `{"type":"error","error":{"message":"late failure"}}`},
			{
				name: "late tool call",
				payload: `{"type":"response.output_item.done","output_index":0,"item":{"id":"lat` +
					`e-item","type":"function_call","call_id":"late-call","name":"lookup","` +
					`arguments":"{}"}}`,
			},
		} {
			for _, transportTerminal := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/%s/transport_terminal=%t", terminal, suffix.name, transportTerminal),
					func(t *testing.T) {
						payload := fmt.Sprintf(
							"data: {\"type\":%q,\"response\":{\"id\":\"first-response\",\"output\":[]}}\n\n", terminal,
						)
						if suffix.payload != "" {
							payload += "data: " + suffix.payload + "\n\n"
						}
						frames, result, _ := runResponsesWebsocketForwardError(
							t, []byte(payload), nil, false, transportTerminal,
						)
						if result.errMsg != nil || result.forward != nil || result.ackCount != 1 ||
							result.ackErr != nil {
							t.Fatalf("terminal delivery=%+v", result)
						}
						wantType := terminal
						if len(frames) != 1 || gjson.GetBytes(
							frames[0], "type",
						).String() != wantType || gjson.GetBytes(
							frames[0], "response.id",
						).String() != "first-response" {
							var got []string
							for _, frame := range frames {
								got = append(got, string(frame))
							}
							t.Fatalf("frames=%s, want one %s for first-response", strings.Join(got, ", "), wantType)
						}
					},
				)
			}
		}
	}
}
