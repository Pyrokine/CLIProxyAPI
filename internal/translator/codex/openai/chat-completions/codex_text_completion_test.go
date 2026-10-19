package chat_completions

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexChatStreamCompletedTextDeliveredOnce(t *testing.T) {
	const part = `{"type":"output_text","text":"你好"}`
	const item = `{"type":"message","id":"msg_1","role":"assistant","content":[` + part + `]}`
	for _, test := range []struct {
		name   string
		frames []string
	}{
		{name: "text done", frames: []string{`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"你好"}`}},
		{name: "content part done", frames: []string{`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":` + part + `}`}},
		{name: "item done", frames: []string{`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`}},
		{name: "terminal only", frames: []string{`{"type":"response.completed","response":{"output":[` + item + `]}}`}},
		{name: "partial delta and repeated completion", frames: []string{
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"你"}`,
			`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"你好"}`,
			`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":` + part + `}`,
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		}},
		{name: "full delta and repeated completion", frames: []string{
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"你好"}`,
			`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"你好"}`,
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var param any
			var output strings.Builder
			terminals := 0
			frames := append(append([]string(nil), test.frames...), `{"type":"response.completed","response":{"output":[`+item+`]}}`)
			for _, frame := range frames {
				for _, chunk := range ConvertCodexResponseToOpenAI(context.Background(), "fixture-model", nil, nil, []byte("data: "+frame), &param) {
					output.WriteString(gjson.GetBytes(chunk, "choices.0.delta.content").String())
					if gjson.GetBytes(chunk, "choices.0.finish_reason").String() != "" {
						terminals++
					}
				}
			}
			if output.String() != "你好" || terminals != 1 {
				t.Fatalf("text=%q, terminals=%d; want complete text exactly once and one terminal", output.String(), terminals)
			}
		})
	}
}

func TestCodexChatStreamCompletedPartsRetainCitationOffsets(t *testing.T) {
	const citation = `{"type":"url_citation","url":"https://example.com/source","start_index":0,"end_index":1}`
	const item = `{"type":"message","content":[{"type":"output_text","text":"一a","annotations":[` + citation + `]},{"type":"output_text","text":"b二","annotations":[` + citation + `]}]}`
	for _, terminalType := range []string{"response.completed", "response.done", "response.incomplete"} {
		t.Run(terminalType, func(t *testing.T) {
			var param any
			var output strings.Builder
			var citations []gjson.Result
			for _, frame := range []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"` + terminalType + `","response":{"output":[` + item + `]}}`,
			} {
				for _, chunk := range ConvertCodexResponseToOpenAI(context.Background(), "fixture-model", nil, nil, []byte("data: "+frame), &param) {
					output.WriteString(gjson.GetBytes(chunk, "choices.0.delta.content").String())
					citations = append(citations, gjson.GetBytes(chunk, "choices.0.delta.annotations").Array()...)
				}
			}
			if output.String() != "一ab二" || len(citations) != 2 || citations[0].Get("start_index").Int() != 0 || citations[1].Get("start_index").Int() != 2 {
				t.Fatalf("text=%q, citations=%v; want all parts and Unicode citation offsets", output.String(), citations)
			}
		})
	}
}
