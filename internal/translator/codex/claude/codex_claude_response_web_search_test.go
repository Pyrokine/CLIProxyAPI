package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToClaudeStreamWebSearchCallEmitsServerToolBlocks(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`,
		`{"type":"response.output_item.added","item":{"id":"ws_123","type":"web_search_call","status":"in_progress"}}`,
		`{"type":"response.web_search_call.searching","item_id":"ws_123"}`,
		`{"type":"response.web_search_call.completed","item_id":"ws_123"}`,
		`{"type":"response.output_item.done","item":{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"search weather"},"results":[{"title":"Weather","url":"https://example.com/weather"}]}}`,
		`{"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":3,"output_tokens":2}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 2 {
		t.Fatalf("content block starts = %d, want server tool use and result", len(starts))
	}
	if got := starts[0].Data.Get("content_block.type").String(); got != "server_tool_use" {
		t.Fatalf("first block type = %q, want server_tool_use", got)
	}
	if got := starts[0].Data.Get("content_block.id").String(); got != "ws_123" {
		t.Fatalf("server tool ID = %q, want ws_123", got)
	}
	if got := starts[1].Data.Get("content_block.type").String(); got != "web_search_tool_result" {
		t.Fatalf("second block type = %q, want web_search_tool_result", got)
	}
	if got := starts[1].Data.Get("content_block.tool_use_id").String(); got != "ws_123" {
		t.Fatalf("tool result ID = %q, want ws_123", got)
	}
	if got := starts[1].Data.Get("content_block.content.0.url").String(); got != "https://example.com/weather" {
		t.Fatalf("web result URL = %q, want example weather URL", got)
	}

	var query string
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		if event.Data.Get("delta.type").String() != "input_json_delta" {
			continue
		}
		partial := gjson.Parse(event.Data.Get("delta.partial_json").String())
		query = partial.Get("query").String()
	}
	if query != "search weather" {
		t.Fatalf("web search query = %q, want search weather", query)
	}
	messageDeltas := codexClaudeEventsByName(events, "message_delta")
	if len(messageDeltas) != 1 || messageDeltas[0].Data.Get("delta.stop_reason").String() != "end_turn" {
		t.Fatalf("server web search changed stop reason: %#v", messageDeltas)
	}
}

func TestConvertCodexResponseToClaudeStreamClosesTextBeforeWebSearch(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"before search"}`,
		`{"type":"response.output_item.done","item":{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather"}}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 3 {
		t.Fatalf("content block starts = %d, want text, server tool use, and result", len(starts))
	}
	wantTypes := []string{"text", "server_tool_use", "web_search_tool_result"}
	for index, wantType := range wantTypes {
		if got := starts[index].Data.Get("content_block.type").String(); got != wantType {
			t.Fatalf("block %d type = %q, want %q", index, got, wantType)
		}
		if got := starts[index].Data.Get("index").Int(); got != int64(index) {
			t.Fatalf("block %d index = %d, want %d", index, got, index)
		}
	}
}

func TestConvertCodexResponseToClaudeStreamDefersWebSearchUntilFunctionCallCloses(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`{"type":"response.output_item.done","item":{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather"}}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 3 {
		t.Fatalf("content block starts = %d, want function and web search blocks", len(starts))
	}
	wantTypes := []string{"tool_use", "server_tool_use", "web_search_tool_result"}
	for index, wantType := range wantTypes {
		if got := starts[index].Data.Get("content_block.type").String(); got != wantType {
			t.Fatalf("block %d type = %q, want %q", index, got, wantType)
		}
	}
}

func TestConvertCodexResponseToClaudeStreamDeduplicatesWebSearchDone(t *testing.T) {
	webSearchDone := `{"type":"response.output_item.done","item":{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather"}}}`
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`,
		webSearchDone,
		webSearchDone,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 2 {
		t.Fatalf("duplicate web search emitted %d content blocks, want 2", len(starts))
	}
}

func TestConvertCodexResponseToClaudeStreamPairsFallbackWebSearchID(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.output_item.done","item":{"type":"web_search_call","status":"completed","action":{"type":"search","query":"weather"}}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 2 {
		t.Fatalf("content block starts = %d, want server tool use and result", len(starts))
	}
	toolUseID := starts[0].Data.Get("content_block.id").String()
	if toolUseID == "" || !strings.HasPrefix(toolUseID, "web_search_") {
		t.Fatalf("fallback tool use ID = %q, want web_search_ prefix", toolUseID)
	}
	if got := starts[1].Data.Get("content_block.tool_use_id").String(); got != toolUseID {
		t.Fatalf("tool result ID = %q, want %q", got, toolUseID)
	}
}

func TestConvertCodexResponseToClaudeNonStreamWebSearchCallEmitsServerToolBlocks(t *testing.T) {
	response := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5.4",
			"stop_reason":"stop",
			"usage":{"input_tokens":3,"output_tokens":2},
			"output":[
				{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"search weather"}},
				{"type":"message","content":[{"type":"output_text","text":"done"}]}
			]
		}
	}`)
	out := ConvertCodexResponseToClaudeNonStream(
		context.Background(),
		"",
		nil,
		nil,
		response,
		nil,
	)

	if got := gjson.GetBytes(out, "content.0.type").String(); got != "server_tool_use" {
		t.Fatalf("content.0.type = %q, want server_tool_use; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "content.0.input.query").String(); got != "search weather" {
		t.Fatalf("web search query = %q, want search weather; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "content.1.type").String(); got != "web_search_tool_result" {
		t.Fatalf("content.1.type = %q, want web_search_tool_result; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "content.2.text").String(); got != "done" {
		t.Fatalf("content.2.text = %q, want done; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn; output=%s", got, out)
	}
}

func TestConvertCodexResponseToClaudeNonStreamDeduplicatesEmptyOpenPage(t *testing.T) {
	response := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5.4",
			"usage":{"input_tokens":1,"output_tokens":1},
			"output":[
				{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"open_page"}},
				{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"weather"}}
			]
		}
	}`)
	out := ConvertCodexResponseToClaudeNonStream(
		context.Background(),
		"",
		nil,
		nil,
		response,
		nil,
	)

	if got := gjson.GetBytes(out, "content.#").Int(); got != 2 {
		t.Fatalf("content count = %d, want one web search pair; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "content.0.input.query").String(); got != "weather" {
		t.Fatalf("kept web search query = %q, want weather; output=%s", got, out)
	}
}
