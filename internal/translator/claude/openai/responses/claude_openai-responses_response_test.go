package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func parseClaudeResponsesSSEEvent(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	var event string
	var data string
	for _, line := range strings.Split(string(chunk), "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if data == "" {
		t.Fatalf("SSE chunk has no data line: %s", string(chunk))
	}

	return event, gjson.Parse(data)
}

func TestConvertClaudeResponseToOpenAIResponsesThinkingIncludesSignature(t *testing.T) {
	signature := "claude_sig_123"
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"internal "}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(
			outputs,
			ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param)...,
		)
	}

	var reasoningDone gjson.Result
	var completed gjson.Result
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		switch event {
		case "response.output_item.done":
			if data.Get("item.type").String() == "reasoning" {
				reasoningDone = data
			}
		case "response.completed":
			completed = data
		}
	}

	if !reasoningDone.Exists() {
		t.Fatal("expected reasoning output_item.done event")
	}
	if got := reasoningDone.Get("item.encrypted_content").String(); got != signature {
		t.Fatalf("reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := reasoningDone.Get("item.summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("reasoning summary text = %q", got)
	}
	if got := completed.Get("response.output.0.encrypted_content").String(); got != signature {
		t.Fatalf("completed reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := completed.Get("response.output.0.summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("completed reasoning summary text = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesSuppressesSignatureDeltaPassthrough(t *testing.T) {
	var param any
	outputs := ConvertClaudeResponseToOpenAIResponses(
		context.Background(),
		"claude-test",
		nil,
		nil,
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"claude_sig_123"}}`),
		&param,
	)
	if len(outputs) != 0 {
		t.Fatalf("expected signature_delta to be suppressed, got %d chunks", len(outputs))
	}
}

func TestConvertClaudeResponseToOpenAIResponsesAggregatesTextBlocksUntilMessageStop(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"**对比竞品**\n- "}}`),
		[]byte(`data: {"type":"content_block_stop","index":4}`),
		[]byte(`data: {"type":"content_block_start","index":5,"content_block":{"type":"server_tool_use","id":"srv_123","name":"web_search","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"Qwen3\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":5}`),
		[]byte(`data: {"type":"content_block_start","index":6,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_123","content":[{"type":"web_search_result","title":"Example","url":"https://example.com"}]}}`),
		[]byte(`data: {"type":"content_block_stop","index":6}`),
		[]byte(`data: {"type":"content_block_delta","index":5,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","cited_text":"Qwen 3.7 Max","url":"https://example.com","title":"Example"}}}`),
		[]byte(`data: {"type":"content_block_start","index":7,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"Qwen 3.7 Max leads."}}`),
		[]byte(`data: {"type":"content_block_stop","index":7}`),
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":12}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(
			outputs,
			ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param)...,
		)
	}

	counts := map[string]int{}
	var outputTextDone gjson.Result
	var contentPartDone gjson.Result
	var completed gjson.Result
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		counts[event]++
		switch event {
		case "response.output_text.done":
			outputTextDone = data
		case "response.content_part.done":
			contentPartDone = data
		case "response.completed":
			completed = data
		}
		if strings.HasPrefix(event, "content_block_") || event == "message_delta" {
			t.Fatalf("unexpected anthropic-native event leaked: %s", event)
		}
	}

	if counts["response.output_item.added"] != 1 {
		t.Fatalf("response.output_item.added count = %d, want 1", counts["response.output_item.added"])
	}
	if counts["response.content_part.added"] != 1 {
		t.Fatalf("response.content_part.added count = %d, want 1", counts["response.content_part.added"])
	}
	if counts["response.output_text.done"] != 1 {
		t.Fatalf("response.output_text.done count = %d, want 1", counts["response.output_text.done"])
	}
	if counts["response.content_part.done"] != 1 {
		t.Fatalf("response.content_part.done count = %d, want 1", counts["response.content_part.done"])
	}
	if counts["response.output_item.done"] != 1 {
		t.Fatalf("response.output_item.done count = %d, want 1", counts["response.output_item.done"])
	}
	if counts["response.function_call_arguments.delta"] != 0 {
		t.Fatalf(
			"response.function_call_arguments.delta count = %d, want 0",
			counts["response.function_call_arguments.delta"],
		)
	}

	wantText := "**对比竞品**\n- Qwen 3.7 Max leads."
	if got := outputTextDone.Get("text").String(); got != wantText {
		t.Fatalf("output_text.done text = %q, want %q", got, wantText)
	}
	if got := contentPartDone.Get("part.annotations.0.type").String(); got != "web_search_result_location" {
		t.Fatalf("content_part.done annotation type = %q", got)
	}
	if got := completed.Get("response.output.0.content.0.text").String(); got != wantText {
		t.Fatalf("completed message text = %q, want %q", got, wantText)
	}
	if got := completed.Get("response.output.0.content.0.annotations.0.type").String(); got != "web_search_result_location" {
		t.Fatalf("completed annotation type = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStreamAggregatesCitationsAndSuppressesServerTool(t *testing.T) {
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_nonstream_annotations","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Answer"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srv_123","name":"web_search","input":{}}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"Qwen3\"}"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com","title":"Example"}}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.#").Int(); got != 1 {
		t.Fatalf("output count = %d, want 1", got)
	}
	if got := root.Get("output.0.type").String(); got != "message" {
		t.Fatalf("output[0].type = %q, want message", got)
	}
	if got := root.Get("output.0.content.0.annotations.0.type").String(); got != "web_search_result_location" {
		t.Fatalf("non-stream annotation type = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStreamThinkingIncludesSignature(t *testing.T) {
	signature := "claude_sig_nonstream"
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"nonstream reasoning"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.0.encrypted_content").String(); got != signature {
		t.Fatalf("non-stream reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := root.Get("output.0.summary.0.text").String(); got != "nonstream reasoning" {
		t.Fatalf("non-stream reasoning summary text = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesKeepsOutputIndexOrder(t *testing.T) {
	signature := "claude_sig_second"
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_order","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"visible"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"later reasoning"}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(
			context.Background(), "claude-test", nil, nil, chunk, &param,
		) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				completed = data
			}
		}
	}

	if got := completed.Get("response.output.0.type").String(); got != "message" {
		t.Fatalf("output[0].type = %q, want message", got)
	}
	if got := completed.Get("response.output.0.content.0.text").String(); got != "visible" {
		t.Fatalf("output[0] text = %q, want visible", got)
	}
	if got := completed.Get("response.output.1.type").String(); got != "reasoning" {
		t.Fatalf("output[1].type = %q, want reasoning", got)
	}
	if got := completed.Get("response.output.1.encrypted_content").String(); got != signature {
		t.Fatalf("output[1].encrypted_content = %q, want %q", got, signature)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStreamKeepsOutputIndexOrder(t *testing.T) {
	signature := "claude_sig_nonstream_second"
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_nonstream_order","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"visible"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"later reasoning"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.0.type").String(); got != "message" {
		t.Fatalf("output[0].type = %q, want message", got)
	}
	if got := root.Get("output.0.content.0.text").String(); got != "visible" {
		t.Fatalf("output[0] text = %q, want visible", got)
	}
	if got := root.Get("output.1.type").String(); got != "reasoning" {
		t.Fatalf("output[1].type = %q, want reasoning", got)
	}
	if got := root.Get("output.1.encrypted_content").String(); got != signature {
		t.Fatalf("output[1].encrypted_content = %q, want %q", got, signature)
	}
}
