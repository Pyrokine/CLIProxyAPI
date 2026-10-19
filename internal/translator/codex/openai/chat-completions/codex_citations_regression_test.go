package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexChatPreservesAllTextAndCitationOffsets(t *testing.T) {
	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "fixture-model", nil, nil, []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"一a","annotations":[{"type":"url_citation","url":"https://example.com/source","title":"Source","start_index":0,"end_index":1}]},{"type":"output_text","text":"b二","annotations":[{"type":"url_citation","url":"https://example.com/source","title":"Source","start_index":0,"end_index":1}]}]}]}}`), nil)
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "一ab二" {
		t.Errorf("content = %q, want all output_text parts; %s", got, out)
	}
	annotations := gjson.GetBytes(out, "choices.0.message.annotations").Array()
	if len(annotations) != 2 {
		t.Fatalf("citations = %d, want 2; %s", len(annotations), out)
	}
	for index, citation := range annotations {
		if citation.Get("url").String() != "https://example.com/source" || citation.Get("start_index").Int() != int64(index*2) || citation.Get("end_index").Int() != int64(index*2+1) {
			t.Errorf("citation rune offsets are wrong: %s", citation.Raw)
		}
	}
}

func TestCodexChatStreamCitationsUsePartOffsetAndDeduplicate(t *testing.T) {
	var param any
	for _, frame := range []string{
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"一a"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"b二"}`,
	} {
		ConvertCodexResponseToOpenAI(context.Background(), "fixture-model", nil, nil, []byte("data: "+frame), &param)
	}
	annotation := `{"type":"url_citation","url":"https://example.com/source","title":"Source","start_index":0,"end_index":1}`
	frames := []string{
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":1,"annotation":` + annotation + `}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":1,"text":"b二","annotations":[` + annotation + `]}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"一a"},{"type":"output_text","text":"b二","annotations":[` + annotation + `]}]}}`,
	}
	var annotations []gjson.Result
	for _, frame := range frames {
		for _, chunk := range ConvertCodexResponseToOpenAI(context.Background(), "fixture-model", nil, nil, []byte("data: "+frame), &param) {
			annotations = append(annotations, gjson.GetBytes(chunk, "choices.0.delta.annotations").Array()...)
		}
	}
	if len(annotations) != 1 || annotations[0].Get("start_index").Int() != 2 || annotations[0].Get("end_index").Int() != 3 {
		t.Fatalf("stream citations duplicated or shifted: %+v", annotations)
	}
}
