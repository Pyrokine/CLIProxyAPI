package openai

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketPreservesCompletionTypeAndRestoresOutput(t *testing.T) {
	for _, eventType := range []string{wsEventTypeCompleted, wsEventTypeDone} {
		for _, terminal := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/terminal=%t", eventType, terminal), func(t *testing.T) {
					payload := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"" +
						"id\":\"fixture-output\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"" +
						"type\":\"output_text\",\"text\":\"fixture answer\"}]}}\n\n"
					payload += fmt.Sprintf(
						"data: {\"type\":%q,\"sequence_number\":51,\"extension\":{\"fixture\":t"+
							"rue},\"response\":{\"id\":\"fixture-response\",\"output\":[]}}\n\n",
						eventType,
					)
					payload += "data: " +
						(`{"type":"response.completed","response":{"id":` +
							`"duplicate-response","output":` +
							`[]}}`) +
						"\n\n"
					frames, result, _ := runResponsesWebsocketForwardError(
						t,
						[]byte(payload),
						nil,
						false,
						terminal,
					)
					if result.errMsg != nil || result.forward != nil || result.ackCount != 1 ||
						result.ackErr != nil {
						t.Fatalf("completion delivery: %+v", result)
					}
					if len(frames) != 2 {
						t.Fatalf("downstream frames=%q, want output item and one completion", frames)
					}
					completion := gjson.ParseBytes(frames[1])
					if completion.Get("type").String() != eventType ||
						completion.Get("response.id").String() != "fixture-response" ||
						completion.Get("sequence_number").Int() != 51 ||
						!completion.Get("extension.fixture").Bool() {
						t.Errorf("original completion envelope changed: %s", frames[1])
					}
					if completion.Get("response.output.0.id").String() != "fixture-output" ||
						completion.Get("response.output.0.content.0.text").String() != "fixture answer" {
						t.Errorf("completion lost recovered output: %s", frames[1])
					}
				},
			)
		}
	}
}
