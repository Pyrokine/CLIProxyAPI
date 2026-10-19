package chat_completions

import (
	"math"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func codexTextPartKey(event gjson.Result, contentIndex int64) string {
	return strconv.FormatInt(event.Get("output_index").Int(), 10) + ":" + strconv.FormatInt(contentIndex, 10)
}

func codexChatURLCitations(value gjson.Result, offset int64, seen map[string]struct{}) [][]byte {
	var annotations []gjson.Result
	if value.IsArray() {
		annotations = value.Array()
	} else if value.IsObject() {
		annotations = []gjson.Result{value}
	}
	var citations [][]byte
	for _, annotation := range annotations {
		if annotation.Get("type").String() != "url_citation" {
			continue
		}
		startValue, endValue := annotation.Get("start_index"), annotation.Get("end_index")
		if startValue.Type != gjson.Number || endValue.Type != gjson.Number {
			continue
		}
		start, end := startValue.Int(), endValue.Int()
		if offset < 0 || start < 0 || end < start || end > math.MaxInt64-offset {
			continue
		}
		start += offset
		end += offset
		url := annotation.Get("url").String()
		if url == "" {
			continue
		}
		key := url + "\x00" + strconv.FormatInt(start, 10) + "\x00" + strconv.FormatInt(end, 10)
		if _, exists := seen[key]; exists {
			continue
		}
		citation := []byte(`{"type":"url_citation","url":"","title":"","start_index":0,"end_index":0}`)
		citation, _ = sjson.SetBytes(citation, "url", url)
		citation, _ = sjson.SetBytes(citation, "title", annotation.Get("title").String())
		citation, _ = sjson.SetBytes(citation, "start_index", start)
		citation, _ = sjson.SetBytes(citation, "end_index", end)
		if seen != nil {
			seen[key] = struct{}{}
		}
		citations = append(citations, citation)
	}
	return citations
}

func codexStreamURLCitations(event gjson.Result, params *convertCliToOpenAIParams) [][]byte {
	var citations [][]byte
	appendAt := func(value gjson.Result, contentIndex int64) {
		offset := params.textPartOffsets[codexTextPartKey(event, contentIndex)]
		citations = append(citations, codexChatURLCitations(value, offset, params.citationKeys)...)
	}
	contentIndex := event.Get("content_index").Int()
	for _, path := range []string{"annotation", "annotations", "part.annotations", "item.annotations"} {
		appendAt(event.Get(path), contentIndex)
	}
	for index, content := range event.Get("item.content").Array() {
		appendAt(content.Get("annotations"), int64(index))
	}
	return citations
}
