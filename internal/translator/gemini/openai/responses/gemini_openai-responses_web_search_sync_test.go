package responses

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesSyncNonStreamCitationUsesRuneOffsets(t *testing.T) {
	provider := []byte(`{
		"responseId":"ref-1",
		"candidates":[{
			"finishReason":"STOP",
			"content":{"parts":[{"text":"中文回答"}]},
			"groundingMetadata":{
				"webSearchQueries":["查询"],
				"groundingChunks":[{"web":{"uri":"https://example.com/zh","title":"中文来源"}}],
				"groundingSupports":[{
					"segment":{"partIndex":0,"startIndex":0,"endIndex":6},
					"groundingChunkIndices":[0]
				}]
			}
		}]
	}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(
		context.Background(), "gemini-3.1-flash-image", nil, nil, provider, nil,
	)
	annotation := gjson.GetBytes(result, "output.1.content.0.annotations.0")
	if annotation.Get("type").String() != "url_citation" ||
		annotation.Get("url").String() != "https://example.com/zh" ||
		annotation.Get("start_index").Int() != 0 || annotation.Get("end_index").Int() != 2 {
		t.Fatalf("citation must reference two Chinese runes: %s", result)
	}
}

func TestResponsesSyncFlatPartsSearchQuery(t *testing.T) {
	root := gjson.Parse(`{"input":[{"type":"input_text","text":"first"},{"type":"input_text","text":"second"}]}`)
	if got := responsesWebSearchQuery(root); got != "first\nsecond" {
		t.Fatalf("query = %q, want both input text parts", got)
	}
}

func TestResponsesSyncNonStreamGroundingOnlyCitesFirstCandidate(t *testing.T) {
	provider := []byte(`{"responseId":"candidate-grounding","candidates":[
		{"content":{"parts":[{"text":"甲"}]},"groundingMetadata":{
			"webSearchQueries":["query"],
			"groundingChunks":[{"web":{"uri":"https://example.com/first"}}],
			"groundingSupports":[{"segment":{"partIndex":0,"startIndex":0,"endIndex":6},"groundingChunkIndices":[0]}]
		},"finishReason":"STOP"},
		{"content":{"parts":[{"text":"乙"}]},"finishReason":"STOP"}
	]}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(
		context.Background(), "gemini-3.1-flash-image", nil, nil, provider, nil,
	)
	if gjson.GetBytes(result, "output.1.content.0.annotations.0.url").String() != "https://example.com/first" ||
		gjson.GetBytes(result, "output.2.content.0.annotations.#").Int() != 0 {
		t.Fatalf("search grounding of first candidate was applied to another candidate: %s", result)
	}
}

type responsesSearchEvent struct {
	name string
	data gjson.Result
}

func responsesSyncSearchStreamEvents(t *testing.T, request, translated []byte, frames ...string) []responsesSearchEvent {
	t.Helper()
	var state any
	var events []responsesSearchEvent
	for _, frame := range append(frames, "[DONE]") {
		for _, chunk := range ConvertGeminiResponseToOpenAIResponses(
			context.Background(), "gemini-3.1-flash-image", request, translated, []byte(frame), &state,
		) {
			name, data := parseSSEEvent(t, chunk)
			events = append(events, responsesSearchEvent{name, data})
		}
	}
	return events
}

func TestResponsesSyncStreamSearchOrderAndLateGrounding(t *testing.T) {
	request := []byte(`{"model":"gemini-3.1-flash-image","input":"查询","tools":[{"type":"web_search"}]}`)
	var state any
	first := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.1-flash-image", request, request,
		[]byte(`{"responseId":"search-order","candidates":[{"content":{"parts":[{"text":"中文"}]}}]}`), &state)
	for _, chunk := range first {
		if name, _ := parseSSEEvent(t, chunk); name == "response.output_text.delta" {
			t.Fatal("answer text was emitted before search grounding could be processed")
		}
	}
	second := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.1-flash-image", request, request,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"回答"}]},"groundingMetadata":{
			"webSearchQueries":["查询中文"],
			"groundingChunks":[{"web":{"uri":"https://example.com/zh","title":"中文来源"}}],
			"groundingSupports":[{"segment":{"partIndex":0,"startIndex":0,"endIndex":6},"groundingChunkIndices":[0]}
			]}}]}`), &state)
	third := translateGeminiFrameWithEOF(context.Background(), "gemini-3.1-flash-image", request, request,
		[]byte(`{"candidates":[{"finishReason":"STOP"}]}`), &state)
	var events []responsesSearchEvent
	for _, chunk := range append(append(first, second...), third...) {
		name, data := parseSSEEvent(t, chunk)
		events = append(events, responsesSearchEvent{name, data})
	}
	order := []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.web_search_call.searching", "response.web_search_call.completed",
		"response.output_item.done", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.annotation.added",
		"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
	}
	if len(events) != len(order) {
		t.Fatalf("event count = %d, want %d: %+v", len(events), len(order), events)
	}
	for i, event := range events {
		if event.name != order[i] || event.data.Get("sequence_number").Int() != int64(i+1) {
			t.Fatalf("event %d = %s seq %d, want %s seq %d", i, event.name, event.data.Get("sequence_number").Int(), order[i], i+1)
		}
	}
	search := events[5].data.Get("item")
	if search.Get("type").String() != "web_search_call" || search.Get("action.query").String() != "查询中文" ||
		search.Get("action.sources.0.url").String() != "https://example.com/zh" {
		t.Fatalf("completed web search item = %s", search.Raw)
	}
	annotation := events[10].data.Get("annotation")
	if annotation.Get("start_index").Int() != 0 || annotation.Get("end_index").Int() != 2 ||
		annotation.Get("url").String() != "https://example.com/zh" {
		t.Fatalf("CJK citation = %s", annotation.Raw)
	}
	completed := events[len(events)-1].data.Get("response")
	if completed.Get("output.0.type").String() != "web_search_call" ||
		completed.Get("output.1.content.0.text").String() != "中文回答" ||
		completed.Get("output.1.content.0.annotations.0.end_index").Int() != 2 ||
		completed.Get("tool_usage.web_search.num_requests").Int() != 1 {
		t.Fatalf("completed response = %s", completed.Raw)
	}
}

func TestResponsesSyncStreamSearchLateSourcesAndCitations(t *testing.T) {
	request := []byte(`{"model":"gemini-3.1-flash-image","input":"search","tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`)
	events := responsesSyncSearchStreamEvents(t, request, request,
		`{"responseId":"late-source","candidates":[{"content":{"parts":[{"text":"甲乙"}]},"groundingMetadata":{"webSearchQueries":["first"]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`,
		`{"candidates":[{"groundingMetadata":{"webSearchQueries":["second"],"groundingChunks":[{"web":{"uri":"https://example.com/late","title":"延迟来源"}}]}}]}`,
		`{"candidates":[{"groundingMetadata":{"groundingSupports":[{"segment":{"partIndex":0,"startIndex":0,"endIndex":3},"groundingChunkIndices":[0]}]},"finishReason":"STOP"}]}`,
	)
	searchCompletions, annotations := 0, 0
	messageDone, annotationAdded, responseDone := -1, -1, -1
	var earlySearch, completed gjson.Result
	for index, event := range events {
		switch event.name {
		case "response.web_search_call.completed":
			searchCompletions++
		case "response.output_item.done":
			if event.data.Get("item.type").String() == "web_search_call" {
				earlySearch = event.data.Get("item")
			}
			if event.data.Get("item.type").String() == "message" {
				messageDone = index
			}
		case "response.output_text.annotation.added":
			annotationAdded = index
			annotations++
			if event.data.Get("annotation.start_index").Int() != 0 ||
				event.data.Get("annotation.end_index").Int() != 1 ||
				event.data.Get("annotation.url").String() != "https://example.com/late" {
				t.Fatalf("late annotation = %s", event.data.Raw)
			}
		case "response.completed":
			responseDone = index
			completed = event.data.Get("response")
		}
	}
	if searchCompletions != 1 || annotations != 1 || earlySearch.Get("action.sources").Exists() ||
		messageDone < 0 || annotationAdded <= messageDone || responseDone <= annotationAdded {
		t.Fatalf("expected one early search completion and one late citation: %+v", events)
	}
	if completed.Get("output.0.action.queries.#").Int() != 2 ||
		completed.Get("output.0.action.sources.0.url").String() != "https://example.com/late" ||
		completed.Get("output.1.content.0.annotations.0.title").String() != "延迟来源" ||
		completed.Get("output.2.type").String() != "function_call" {
		t.Fatalf("late source was not included in final response: %s", completed.Raw)
	}
}

func TestResponsesSyncStreamGroundingChunkIndicesAndPartMapping(t *testing.T) {
	request := []byte(`{"model":"gemini-3.1-flash-image","input":"search","tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`)
	events := responsesSyncSearchStreamEvents(t, request, request,
		`{"responseId":"raw-indices","candidates":[{"content":{"parts":[{"text":"甲"}]},"groundingMetadata":{"webSearchQueries":["search"]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.com/a"}},{"web":{"uri":"https://example.com/a"}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"乙"}]},"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.com/b","title":"B"}}],
			"groundingSupports":[{"segment":{"partIndex":2,"startIndex":0,"endIndex":3},"groundingChunkIndices":[2]}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
	)
	var completed gjson.Result
	for _, event := range events {
		if event.name == "response.completed" {
			completed = event.data.Get("response")
		}
	}
	if completed.Get("output.0.action.sources.#").Int() != 2 ||
		completed.Get("output.0.action.sources.1.url").String() != "https://example.com/b" ||
		completed.Get("output.1.content.0.annotations.#").Int() != 0 ||
		completed.Get("output.3.content.0.annotations.0.url").String() != "https://example.com/b" ||
		completed.Get("output.3.content.0.annotations.0.end_index").Int() != 1 {
		t.Fatalf("cumulative source index or message part mapping incorrect: %s", completed.Raw)
	}
}

func TestResponsesSyncStreamCitationAcrossMessages(t *testing.T) {
	request := []byte(`{"model":"gemini-3.1-flash-image","input":"search","tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`)
	events := responsesSyncSearchStreamEvents(t, request, request,
		`{"responseId":"cross-message","candidates":[{"content":{"parts":[{"text":"甲"}]},"groundingMetadata":{"webSearchQueries":["query"]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"乙"}]}}]}`,
		`{"candidates":[{"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.com/two"}}],
			"groundingSupports":[{"segment":{"startIndex":0,"endIndex":6},"groundingChunkIndices":[0]}]},"finishReason":"STOP"}]}`,
	)
	var completed gjson.Result
	var citedMessages []int64
	for _, event := range events {
		if event.name == "response.output_text.annotation.added" {
			citedMessages = append(citedMessages, event.data.Get("output_index").Int())
			if event.data.Get("annotation.start_index").Int() != 0 || event.data.Get("annotation.end_index").Int() != 1 {
				t.Fatalf("cross-message rune range = %s", event.data.Raw)
			}
		}
		if event.name == "response.completed" {
			completed = event.data.Get("response")
		}
	}
	if len(citedMessages) != 2 || citedMessages[0] != 1 || citedMessages[1] != 3 ||
		completed.Get("output.1.content.0.annotations.0.url").String() != "https://example.com/two" ||
		completed.Get("output.3.content.0.annotations.0.url").String() != "https://example.com/two" {
		t.Fatalf("citation span not split at message boundary: %+v", events)
	}
}

func TestResponsesSyncStreamInterleavedFunctionWithoutGrounding(t *testing.T) {
	request := []byte(`{"model":"gemini-3.1-flash-image","input":"search then calculate","tools":[{"type":"web_search"},{"type":"function","name":"calculate"}]}`)
	events := responsesSyncSearchStreamEvents(t, request, request,
		`{"responseId":"interleaved","candidates":[{"content":{"parts":[{"text":"first"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"calculate","args":{"expr":"1+1"}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"second"}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
	)
	var completed gjson.Result
	var deltas []string
	for _, event := range events {
		if event.name == "response.output_text.delta" {
			deltas = append(deltas, event.data.Get("delta").String())
		}
		if event.name == "response.completed" {
			completed = event.data.Get("response")
		}
		if event.name == "response.web_search_call.searching" {
			t.Fatal("no grounding metadata was returned, search must not be claimed")
		}
	}
	if len(deltas) != 2 || deltas[0] != "first" || deltas[1] != "second" ||
		completed.Get("output.0.content.0.text").String() != "first" ||
		completed.Get("output.1.type").String() != "function_call" ||
		completed.Get("output.2.content.0.text").String() != "second" {
		t.Fatalf("text/function/text sequence lost content: %+v", events)
	}
}

func TestResponsesSyncStreamSearchDisabledAndNoGrounding(t *testing.T) {
	request := []byte(`{"model":"gemini-3.1-flash-image","input":"search","tools":[{"type":"web_search"}]}`)
	translated := []byte(`{"contents":[{"role":"user","parts":[{"text":"search"}]}]}`)
	for _, pair := range [][2][]byte{
		{[]byte(`{"model":"gemini-3.1-flash-image","tools":[{"type":"web_search"}],"tool_choice":"none"}`), request},
		{request, translated},
	} {
		var state any
		first := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.1-flash-image", pair[0], pair[1],
			[]byte(`{"responseId":"disabled-search","candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`), &state)
		hasDelta := false
		for _, chunk := range first {
			if name, _ := parseSSEEvent(t, chunk); name == "response.output_text.delta" {
				hasDelta = true
			}
		}
		if !hasDelta {
			t.Fatalf("search-disabled response buffered ordinary text: %s", pair[0])
		}
	}
	events := responsesSyncSearchStreamEvents(t, request, request,
		`{"responseId":"no-grounding","candidates":[{"content":{"parts":[{"text":"answer"}]},"groundingMetadata":{"groundingChunks":[{"retrievedContext":{"uri":"internal-doc"}}],"groundingSupports":[{"segment":{"startIndex":0,"endIndex":6},"groundingChunkIndices":[0]}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
	)
	for _, event := range events {
		if event.name == "response.web_search_call.searching" || event.data.Get("item.type").String() == "web_search_call" {
			t.Fatalf("non-web grounding claimed web search: %+v", events)
		}
	}
}
