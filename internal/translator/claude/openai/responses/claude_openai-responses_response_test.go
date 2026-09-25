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
	for line := range strings.SplitSeq(string(chunk), "\n") {
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			event = after
			continue
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			data = after
		}
	}
	if data == "" {
		t.Fatalf("SSE chunk has no data line: %s", string(chunk))
	}

	return event, gjson.Parse(data)
}

func TestConvertClaudeResponseToOpenAIResponsesCreatedIncludesOriginalRequestModel(t *testing.T) {
	originalRequest := []byte(`{"request":{"model":"original-claude-model"}}`)
	translatedRequest := []byte(`{"model":"translated-claude-model"}`)
	chunk := []byte(`data: {"type":"message_start","message":{"id":"msg_123"}}`)

	var state any
	outputs := convertClaudeResponseToOpenAIResponses(
		context.Background(),
		"fallback-model",
		originalRequest,
		translatedRequest,
		chunk,
		&state,
	)

	models := make(map[string]string)
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		models[event] = data.Get("response.model").String()
	}
	for _, event := range []string{"response.created", "response.in_progress"} {
		if got := models[event]; got != "original-claude-model" {
			t.Fatalf("%s model = %q, want original-claude-model", event, got)
		}
	}
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
			convertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param)...,
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
	outputs := convertClaudeResponseToOpenAIResponses(
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
			convertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param)...,
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

	if counts["response.output_item.added"] != 3 {
		t.Fatalf("response.output_item.added count = %d, want 3", counts["response.output_item.added"])
	}
	if counts["response.content_part.added"] != 2 {
		t.Fatalf("response.content_part.added count = %d, want 2", counts["response.content_part.added"])
	}
	if counts["response.output_text.done"] != 2 {
		t.Fatalf("response.output_text.done count = %d, want 2", counts["response.output_text.done"])
	}
	if counts["response.content_part.done"] != 2 {
		t.Fatalf("response.content_part.done count = %d, want 2", counts["response.content_part.done"])
	}
	if counts["response.output_item.done"] != 3 {
		t.Fatalf("response.output_item.done count = %d, want 3", counts["response.output_item.done"])
	}
	if counts["response.function_call_arguments.delta"] != 0 {
		t.Fatalf(
			"response.function_call_arguments.delta count = %d, want 0",
			counts["response.function_call_arguments.delta"],
		)
	}

	wantText := "Qwen 3.7 Max leads."
	if got := outputTextDone.Get("text").String(); got != wantText {
		t.Fatalf("last output_text.done text = %q, want %q", got, wantText)
	}
	if got := contentPartDone.Get("part.annotations.0.type").String(); got != "web_search_result_location" {
		t.Fatalf("content_part.done annotation type = %q", got)
	}
	if got := completed.Get("response.output.0.content.0.text").String(); got != "**对比竞品**\n- " {
		t.Fatalf("first completed message text = %q", got)
	}
	if got := completed.Get("response.output.1.type").String(); got != "web_search_call" {
		t.Fatalf("completed output[1].type = %q, want web_search_call", got)
	}
	if got := completed.Get("response.output.2.content.0.text").String(); got != wantText {
		t.Fatalf("last completed message text = %q, want %q", got, wantText)
	}
	if got := completed.Get("response.output.2.content.0.annotations.0.type").String(); got != "web_search_result_location" {
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

	out := convertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.#").Int(); got != 2 {
		t.Fatalf("output count = %d, want 2", got)
	}
	if got := root.Get("output.0.type").String(); got != "message" {
		t.Fatalf("output[0].type = %q, want message", got)
	}
	if got := root.Get("output.1.type").String(); got != "web_search_call" {
		t.Fatalf("output[1].type = %q, want web_search_call", got)
	}
	if got := root.Get("output.1.action.query").String(); got != "Qwen3" {
		t.Fatalf("web search query = %q, want Qwen3", got)
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

	out := convertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
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
		for _, output := range convertClaudeResponseToOpenAIResponses(
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

	out := convertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
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

func TestConvertClaudeResponseToOpenAIResponsesIncludesZeroReasoningTokens(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_usage","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":2}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range convertClaudeResponseToOpenAIResponses(
			context.Background(), "claude-test", nil, nil, chunk, &param,
		) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				completed = data
			}
		}
	}

	reasoningTokens := completed.Get("response.usage.output_tokens_details.reasoning_tokens")
	if !reasoningTokens.Exists() || reasoningTokens.Int() != 0 {
		t.Fatalf("reasoning_tokens = %s, want present with value 0", reasoningTokens.Raw)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStreamIncludesZeroReasoningTokens(t *testing.T) {
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_usage","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"message_delta","usage":{"output_tokens":2}}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	out := convertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	reasoningTokens := gjson.GetBytes(out, "usage.output_tokens_details.reasoning_tokens")
	if !reasoningTokens.Exists() || reasoningTokens.Int() != 0 {
		t.Fatalf("reasoning_tokens = %s, want present with value 0; output: %s", reasoningTokens.Raw, out)
	}
}
