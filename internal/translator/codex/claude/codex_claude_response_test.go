package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func collectCodexClaudeEvents(chunks ...string) string {
	var param any
	var builder strings.Builder
	for _, chunk := range chunks {
		for _, out := range ConvertCodexResponseToClaude(
			context.Background(), "gpt-5.5", nil, nil, []byte("data: "+chunk), &param,
		) {
			builder.Write(out)
		}
	}
	return builder.String()
}

type codexClaudeSSEEvent struct {
	Name string
	Data gjson.Result
}

func collectStructuredCodexClaudeEvents(t *testing.T, chunks ...string) []codexClaudeSSEEvent {
	t.Helper()
	stream := strings.TrimSpace(collectCodexClaudeEvents(chunks...))
	if stream == "" {
		return nil
	}

	blocks := strings.Split(stream, "\n\n")
	events := make([]codexClaudeSSEEvent, 0, len(blocks))
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("invalid SSE block %q", block)
		}
		data := strings.TrimPrefix(lines[1], "data: ")
		if !gjson.Valid(data) {
			t.Fatalf("invalid SSE JSON for %q: %q", lines[0], data)
		}
		events = append(
			events, codexClaudeSSEEvent{
				Name: strings.TrimPrefix(lines[0], "event: "),
				Data: gjson.Parse(data),
			},
		)
	}
	return events
}

func assertCodexClaudeBlockLifecycle(t *testing.T, events []codexClaudeSSEEvent) {
	t.Helper()
	open := make(map[int]bool)
	stopped := make(map[int]bool)
	messageStartCount := 0
	messageDeltaCount := 0
	messageStopCount := 0
	errorCount := 0
	messageStarted := false
	normalTerminalStarted := false
	terminalFinished := false

	assertMessageStarted := func(eventIndex int, eventName string) {
		if !messageStarted {
			t.Fatalf("event %d (%s) arrived before message_start", eventIndex, eventName)
		}
	}
	assertNoOpenBlocks := func(eventIndex int, eventName string) {
		for blockIndex, isOpen := range open {
			if isOpen {
				t.Fatalf("event %d (%s) arrived while block %d was open", eventIndex, eventName, blockIndex)
			}
		}
	}

	for eventIndex, event := range events {
		if terminalFinished {
			t.Fatalf("event %d (%s) arrived after the stream terminal event", eventIndex, event.Name)
		}
		if normalTerminalStarted && event.Name != "message_stop" {
			t.Fatalf("event %d (%s) arrived between message_delta and message_stop", eventIndex, event.Name)
		}
		if event.Data.Get("type").String() != event.Name {
			t.Fatalf(
				"event %d name %q does not match payload type %q", eventIndex, event.Name,
				event.Data.Get("type").String(),
			)
		}
		switch event.Name {
		case "message_start":
			messageStartCount++
			if messageStarted {
				t.Fatalf("event %d emitted duplicate message_start", eventIndex)
			}
			messageStarted = true
		case "content_block_start":
			assertMessageStarted(eventIndex, event.Name)
			blockIndex := int(event.Data.Get("index").Int())
			if open[blockIndex] || stopped[blockIndex] {
				t.Fatalf("event %d started block %d more than once", eventIndex, blockIndex)
			}
			for existingIndex, isOpen := range open {
				if isOpen {
					t.Fatalf(
						"event %d started block %d while block %d was still open",
						eventIndex,
						blockIndex,
						existingIndex,
					)
				}
			}
			open[blockIndex] = true
		case "content_block_delta":
			assertMessageStarted(eventIndex, event.Name)
			blockIndex := int(event.Data.Get("index").Int())
			if !open[blockIndex] || stopped[blockIndex] {
				t.Fatalf("event %d emitted delta for block %d before start or after stop", eventIndex, blockIndex)
			}
		case "content_block_stop":
			assertMessageStarted(eventIndex, event.Name)
			blockIndex := int(event.Data.Get("index").Int())
			if !open[blockIndex] || stopped[blockIndex] {
				t.Fatalf("event %d stopped block %d without one open start", eventIndex, blockIndex)
			}
			open[blockIndex] = false
			stopped[blockIndex] = true
		case "message_delta":
			assertMessageStarted(eventIndex, event.Name)
			messageDeltaCount++
			assertNoOpenBlocks(eventIndex, event.Name)
			normalTerminalStarted = true
		case "message_stop":
			assertMessageStarted(eventIndex, event.Name)
			if !normalTerminalStarted {
				t.Fatalf("event %d emitted message_stop without message_delta", eventIndex)
			}
			messageStopCount++
			assertNoOpenBlocks(eventIndex, event.Name)
			terminalFinished = true
		case "error":
			assertMessageStarted(eventIndex, event.Name)
			errorCount++
			assertNoOpenBlocks(eventIndex, event.Name)
			terminalFinished = true
		}
	}
	assertNoOpenBlocks(len(events), "end of stream")
	if messageStartCount != 1 {
		t.Fatalf("message_start count = %d, want 1", messageStartCount)
	}
	if messageDeltaCount > 1 || messageStopCount > 1 || errorCount > 1 {
		t.Fatalf(
			"duplicate terminal events: message_delta=%d message_stop=%d error=%d", messageDeltaCount, messageStopCount,
			errorCount,
		)
	}
	if messageStopCount != messageDeltaCount {
		t.Fatalf(
			"message terminal event mismatch: message_delta=%d message_stop=%d", messageDeltaCount, messageStopCount,
		)
	}
	if errorCount > 0 && messageStopCount > 0 {
		t.Fatalf("stream emitted both error and normal terminal events")
	}
}

func codexClaudeEventsByName(events []codexClaudeSSEEvent, name string) []codexClaudeSSEEvent {
	matched := make([]codexClaudeSSEEvent, 0)
	for _, event := range events {
		if event.Name == name {
			matched = append(matched, event)
		}
	}
	return matched
}

func TestConvertCodexResponseToClaude_DuplicateCreatedKeepsTextInOneMessage(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.created","response":{"id":"resp_2","model":"gpt-5.5"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
}

func TestConvertCodexResponseToClaude_LateCreatedPrecedesTextEvents(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
}

func TestConvertCodexResponseToClaude_DuplicateCreatedKeepsToolInOneMessage(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		`{"type":"response.created","response":{"id":"resp_2","model":"gpt-5.5"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":0,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
}

func TestConvertCodexResponseToClaude_OrphanTextEventsHaveOneLifecycle(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"type":"response.output_text.delta","item_id":"msg_2","output_index":1,"content_index":0,"delta":"late"}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	starts := codexClaudeEventsByName(events, "content_block_start")
	deltas := codexClaudeEventsByName(events, "content_block_delta")
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(starts) != 1 || starts[0].Data.Get("content_block.type").String() != "text" {
		t.Fatalf("text starts = %#v, want one text block", starts)
	}
	if len(deltas) != 1 || deltas[0].Data.Get("delta.text").String() != "hello" {
		t.Fatalf("text deltas = %#v, want one hello delta", deltas)
	}
	if len(stops) != 1 {
		t.Fatalf("text stops = %d, want 1", len(stops))
	}
}

func TestConvertCodexResponseToClaude_OrphanFunctionEventsWaitForIdentity(t *testing.T) {
	const arguments = `{"city":"Paris"}`
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":1,"delta":"{\"city\":"}`,
		`{"type":"response.function_call_arguments.done","item_id":"item_1","output_index":1,"arguments":"{\"city\":\"Paris\"}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 1 || starts[0].Data.Get("content_block.type").String() != "tool_use" {
		t.Fatalf("tool starts = %#v, want one tool_use block", starts)
	}
	var gotArguments strings.Builder
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		if event.Data.Get("delta.type").String() == "input_json_delta" {
			gotArguments.WriteString(event.Data.Get("delta.partial_json").String())
		}
	}
	if gotArguments.String() != arguments {
		t.Fatalf("tool arguments = %q, want %q", gotArguments.String(), arguments)
	}
	if len(codexClaudeEventsByName(events, "content_block_stop")) != 1 {
		t.Fatalf("tool block did not stop exactly once")
	}
}

func TestConvertCodexResponseToClaude_InterleavedItemsKeepTheirBlockIndexes(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":1,"delta":"{}"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"answer"}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 2 {
		t.Fatalf("content block starts = %d, want 2", len(starts))
	}
	if starts[0].Data.Get("index").Int() != 0 || starts[0].Data.Get("content_block.type").String() != "text" {
		t.Fatalf("first start = %s, want text index 0", starts[0].Data.Raw)
	}
	if starts[1].Data.Get("index").Int() != 1 || starts[1].Data.Get("content_block.type").String() != "tool_use" {
		t.Fatalf("second start = %s, want tool_use index 1", starts[1].Data.Raw)
	}
	var text strings.Builder
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		switch event.Data.Get("delta.type").String() {
		case "text_delta":
			if event.Data.Get("index").Int() != 0 {
				t.Fatalf("text delta used index %d, want 0", event.Data.Get("index").Int())
			}
			text.WriteString(event.Data.Get("delta.text").String())
		case "input_json_delta":
			if event.Data.Get("index").Int() != 1 {
				t.Fatalf("tool delta used index %d, want 1", event.Data.Get("index").Int())
			}
		}
	}
	if got := text.String(); got != "answer" {
		t.Fatalf("interleaved text = %q, want answer", got)
	}
}

func TestConvertCodexResponseToClaude_SeparateContentIndexesUseSeparateTextBlocks(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"first"}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`,
		`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":1,"delta":"second"}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":1}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	starts := codexClaudeEventsByName(events, "content_block_start")
	deltas := codexClaudeEventsByName(events, "content_block_delta")
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(starts) != 2 || starts[0].Data.Get("index").Int() != 0 || starts[1].Data.Get("index").Int() != 1 {
		t.Fatalf("text starts = %#v, want indexes 0 and 1", starts)
	}
	if len(deltas) != 2 || deltas[0].Data.Get("delta.text").String() != "first" || deltas[1].Data.Get("delta.text").String() != "second" {
		t.Fatalf("text deltas = %#v, want first and second", deltas)
	}
	if len(stops) != 2 {
		t.Fatalf("text stops = %d, want 2", len(stops))
	}
}

func TestConvertCodexResponseToClaude_DoneOnlyMessageUsesOneBlockPerTextPart(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.done","item_id":"msg_1","output_index":0,"item":{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":"second"}]}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	starts := codexClaudeEventsByName(events, "content_block_start")
	deltas := codexClaudeEventsByName(events, "content_block_delta")
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(starts) != 2 || starts[0].Data.Get("index").Int() != 0 || starts[1].Data.Get("index").Int() != 1 {
		t.Fatalf("text starts = %#v, want indexes 0 and 1", starts)
	}
	if len(deltas) != 2 || deltas[0].Data.Get("delta.text").String() != "first" || deltas[1].Data.Get("delta.text").String() != "second" {
		t.Fatalf("text deltas = %#v, want first and second", deltas)
	}
	if len(stops) != 2 {
		t.Fatalf("text stops = %d, want 2", len(stops))
	}
}

func TestConvertCodexResponseToClaude_CompletedClosesOpenBlocks(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":1,"delta":"{\"id\":"}`,
		`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]},{"type":"function_call","output_index":1,"id":"item_1","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}],"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(stops) != 2 || stops[0].Data.Get("index").Int() != 0 || stops[1].Data.Get("index").Int() != 1 {
		t.Fatalf("terminal stops = %#v, want indexes 0 then 1", stops)
	}
	var arguments strings.Builder
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		if event.Data.Get("index").Int() == 1 && event.Data.Get("delta.type").String() == "input_json_delta" {
			arguments.WriteString(event.Data.Get("delta.partial_json").String())
		}
	}
	if arguments.String() != `{"id":1}` {
		t.Fatalf("terminal tool arguments = %q, want %q", arguments.String(), `{"id":1}`)
	}
}

func TestConvertCodexResponseToClaude_ResponseDoneClosesBlocksAndEndsStream(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"complete"}`,
		`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	if len(codexClaudeEventsByName(events, "message_stop")) != 1 {
		t.Fatalf("response.done did not emit one message_stop")
	}
}

func TestConvertCodexResponseToClaude_ResponseFailedClosesBlocksAndEndsStream(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"type":"api_error","message":"provider failed"}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	errors := codexClaudeEventsByName(events, "error")
	if len(errors) != 1 || errors[0].Data.Get("error.message").String() != "provider failed" {
		t.Fatalf("response.failed errors = %#v, want provider failed", errors)
	}
}

func TestConvertCodexResponseToClaude_ErrorClosesBlocksAndEndsStream(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		`{"type":"response.error","error":{"type":"api_error","message":"upstream failed"}}`,
		`{"type":"response.error","error":{"type":"api_error","message":"upstream failed again"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_2","output_index":2,"content_index":0,"delta":"late"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	if len(codexClaudeEventsByName(events, "error")) != 1 {
		t.Fatalf("error event count mismatch")
	}
	if len(codexClaudeEventsByName(events, "message_delta")) != 0 || len(
		codexClaudeEventsByName(
			events, "message_stop",
		),
	) != 0 {
		t.Fatalf("normal terminal events followed upstream error")
	}
}

func TestConvertCodexResponseToClaude_DropsUnsignedReasoningSummary(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking without signature"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.content_part.added"}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.content_part.done"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	if strings.Contains(out, `"type":"thinking"`) || strings.Contains(out, "thinking_delta") {
		t.Fatalf("unsigned reasoning summary produced Claude thinking block: %s", out)
	}
	if !strings.Contains(out, `"type":"text_delta"`) {
		t.Fatalf("text output missing: %s", out)
	}
}

func TestConvertCodexResponseToClaude_EmitsReasoningSignature(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"`+signature+`"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"signed thinking"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"`+signature+`"}}`,
	)

	if !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("signed reasoning did not produce thinking block: %s", out)
	}
	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, signature) {
		t.Fatalf("signature delta missing: %s", out)
	}
}

func TestConvertCodexResponseToClaude_EmitsReasoningWhenSignatureArrivesAfterSummary(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"signed thinking"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"`+signature+`"}}`,
	)

	if !strings.Contains(out, `"type":"thinking"`) || !strings.Contains(out, "signed thinking") {
		t.Fatalf("signed reasoning summary missing when signature arrived late: %s", out)
	}
	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, signature) {
		t.Fatalf("late signature delta missing: %s", out)
	}
}

func TestConvertCodexResponseToClaudeNonStream_DropsUnsignedReasoning(t *testing.T) {
	out := ConvertCodexResponseToClaudeNonStream(
		context.Background(), "gpt-5.5", nil, nil, []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5.5",
			"usage":{"input_tokens":1,"output_tokens":2},
			"output":[{"type":"reasoning","summary":[{"text":"unsigned summary"}]}]
		}
	}`), nil,
	)

	if strings.Contains(string(out), `"type":"thinking"`) || strings.Contains(string(out), "unsigned summary") {
		t.Fatalf("unsigned nonstream reasoning produced Claude thinking block: %s", out)
	}
}

func TestConvertCodexResponseToClaude_EmitsDoneOnlyMessageText(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.done","item_id":"msg_1","output_index":0,"item":{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"done-only text"}]}}`,
		`{"type":"response.output_item.done","item_id":"msg_1","output_index":0,"item":{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"done-only text"}]}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	assertCodexClaudeBlockLifecycle(t, events)
	starts := codexClaudeEventsByName(events, "content_block_start")
	deltas := codexClaudeEventsByName(events, "content_block_delta")
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(starts) != 1 || starts[0].Data.Get("content_block.type").String() != "text" {
		t.Fatalf("text starts = %#v, want one text block", starts)
	}
	if len(deltas) != 1 || deltas[0].Data.Get("delta.type").String() != "text_delta" || deltas[0].Data.Get("delta.text").String() != "done-only text" {
		t.Fatalf("text deltas = %#v, want one done-only text delta", deltas)
	}
	if len(stops) != 1 {
		t.Fatalf("text stops = %d, want 1", len(stops))
	}
}

func TestConvertCodexResponseToClaudeNonStream_IncludesReasoningSignature(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	out := ConvertCodexResponseToClaudeNonStream(
		context.Background(), "gpt-5.5", nil, nil, []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5.5",
			"usage":{"input_tokens":1,"output_tokens":2},
			"output":[{"type":"reasoning","encrypted_content":"`+signature+`","summary":[{"text":"signed summary"}]}]
		}
	}`), nil,
	)

	content := gjson.GetBytes(out, "content.0")
	if content.Get("type").String() != "thinking" {
		t.Fatalf("content.0 type = %q, want thinking; output: %s", content.Get("type").String(), out)
	}
	if content.Get("signature").String() != signature {
		t.Fatalf("signature = %q, want %q; output: %s", content.Get("signature").String(), signature, out)
	}
}

func TestConvertCodexResponseToClaude_TranslatesCyberPolicyStreamError(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"response.error","error":{"type":"invalid_request","code":"cyber_policy","message":"blocked by policy"}}`,
	)

	if !strings.Contains(out, "event: error") {
		t.Fatalf("error event missing: %s", out)
	}
	if !strings.Contains(out, `"type":"invalid_request_error"`) {
		t.Fatalf("Claude error type missing: %s", out)
	}
	if !strings.Contains(out, `"code":"cyber_policy"`) || !strings.Contains(out, "blocked by policy") {
		t.Fatalf("Claude error payload incomplete: %s", out)
	}
}

func TestConvertCodexResponseToClaude_TranslatesStreamErrorWithoutMessage(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"error","error_type":"rate_limit_error"}`,
	)

	if !strings.Contains(out, "event: error") {
		t.Fatalf("error event missing: %s", out)
	}
	if !strings.Contains(out, `"type":"rate_limit_error"`) || !strings.Contains(out, `"message":"rate_limit_error"`) {
		t.Fatalf("fallback error payload missing: %s", out)
	}
}

func TestConvertCodexResponseToClaude_IncompleteMapsStopReason(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{name: "max output tokens", reason: "max_output_tokens", wantReason: "max_tokens"},
		{name: "content filter", reason: "content_filter", wantReason: "refusal"},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				events := collectStructuredCodexClaudeEvents(
					t,
					`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"`+testCase.reason+`"},"usage":{"input_tokens":1,"output_tokens":2}}}`,
				)
				assertCodexClaudeBlockLifecycle(t, events)
				messageDeltas := codexClaudeEventsByName(events, "message_delta")
				if len(messageDeltas) != 1 {
					t.Fatalf("message_delta count = %d, want 1", len(messageDeltas))
				}
				if got := messageDeltas[0].Data.Get("delta.stop_reason").String(); got != testCase.wantReason {
					t.Fatalf("stop_reason = %q, want %q", got, testCase.wantReason)
				}
			},
		)
	}
}

func TestConvertCodexResponseToClaudeNonStreamIncompleteMapsStopReason(t *testing.T) {
	output := ConvertCodexResponseToClaudeNonStream(
		context.Background(),
		"",
		nil,
		nil,
		[]byte(`{
			"type":"response.incomplete",
			"response":{
				"id":"resp_1",
				"model":"gpt-5.5",
				"incomplete_details":{"reason":"max_output_tokens"},
				"usage":{"input_tokens":1,"output_tokens":2},
				"output":[]
			}
		}`),
		nil,
	)

	if got := gjson.GetBytes(output, "stop_reason").String(); got != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens; output: %s", got, output)
	}
}

func TestConvertCodexResponseToClaudePreservesStopSequence(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.completed","response":{"stop_reason":"stop","stop_sequence":"\nEND","usage":{"input_tokens":1,"output_tokens":2}}}`,
	)
	messageDeltas := codexClaudeEventsByName(events, "message_delta")
	if len(messageDeltas) != 1 {
		t.Fatalf("message_delta count = %d, want 1", len(messageDeltas))
	}
	if got := messageDeltas[0].Data.Get("delta.stop_reason").String(); got != "stop_sequence" {
		t.Fatalf("stop_reason = %q, want stop_sequence", got)
	}
	if got := messageDeltas[0].Data.Get("delta.stop_sequence").String(); got != "\nEND" {
		t.Fatalf("stop_sequence = %q, want newline END", got)
	}
}

func TestConvertCodexResponseToClaudeQueuesFunctionCallsFIFO(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"first"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"item_2","type":"function_call","call_id":"call_2","name":"second"}}`,
		`{"type":"response.function_call_arguments.done","item_id":"item_2","output_index":1,"arguments":"{\"value\":2}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"item_2","type":"function_call","call_id":"call_2","name":"second","arguments":"{\"value\":2}"}}`,
		`{"type":"response.function_call_arguments.done","item_id":"item_1","output_index":0,"arguments":"{\"value\":1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"first","arguments":"{\"value\":1}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(starts) != 2 || len(stops) != 2 {
		t.Fatalf("tool lifecycle count = %d starts and %d stops, want 2 each", len(starts), len(stops))
	}
	if got := starts[0].Data.Get("content_block.name").String(); got != "first" {
		t.Fatalf("first queued tool = %q, want first", got)
	}
	if got := starts[1].Data.Get("content_block.name").String(); got != "second" {
		t.Fatalf("second queued tool = %q, want second", got)
	}
	if starts[0].Data.Get("index").Int() != 0 || starts[1].Data.Get("index").Int() != 1 {
		t.Fatalf(
			"tool start indexes = %d, %d, want 0, 1", starts[0].Data.Get("index").Int(),
			starts[1].Data.Get("index").Int(),
		)
	}
}

func TestConvertCodexResponseToClaudeDefersTextWhileFunctionBlockIsOpen(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"after tool"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
		`{"type":"response.content_part.done","item_id":"msg_1","output_index":1,"content_index":0}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 2 {
		t.Fatalf("content block starts = %d, want tool then text", len(starts))
	}
	if got := starts[0].Data.Get("content_block.type").String(); got != "tool_use" {
		t.Fatalf("first block type = %q, want tool_use", got)
	}
	if got := starts[1].Data.Get("content_block.type").String(); got != "text" {
		t.Fatalf("second block type = %q, want text", got)
	}
	var text string
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		if event.Data.Get("delta.type").String() == "text_delta" {
			text += event.Data.Get("delta.text").String()
		}
	}
	if text != "after tool" {
		t.Fatalf("text = %q, want after tool", text)
	}
}

func TestConvertCodexResponseToClaudeAggregatesReasoningSummaryParts(t *testing.T) {
	const earlySignature = "early-signature"
	const finalSignature = "final-signature"
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"`+earlySignature+`"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"second"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"`+finalSignature+`"}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	stops := codexClaudeEventsByName(events, "content_block_stop")
	if len(starts) != 1 || starts[0].Data.Get("content_block.type").String() != "thinking" {
		t.Fatalf("thinking starts = %#v, want one thinking block", starts)
	}
	if len(stops) != 1 {
		t.Fatalf("thinking stops = %d, want 1", len(stops))
	}

	var thinking strings.Builder
	var signatures []string
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		switch event.Data.Get("delta.type").String() {
		case "thinking_delta":
			thinking.WriteString(event.Data.Get("delta.thinking").String())
		case "signature_delta":
			signatures = append(signatures, event.Data.Get("delta.signature").String())
		}
	}
	if got := thinking.String(); got != "first\n\nsecond" {
		t.Fatalf("thinking = %q, want multipart summary", got)
	}
	if len(signatures) != 1 || signatures[0] != finalSignature {
		t.Fatalf("signatures = %v, want final signature only", signatures)
	}
	if strings.Contains(
		collectCodexClaudeEvents(
			`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"`+earlySignature+`"}}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"`+finalSignature+`"}}`,
		), earlySignature,
	) {
		t.Fatal("early reasoning signature leaked into output")
	}
}

func TestConvertCodexResponseToClaudeMapsCacheWriteAndReasoningUsage(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6"}}`,
		`{"type":"response.completed","response":{"usage":{`+
			`"input_tokens":1000,"output_tokens":50,`+
			`"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150},`+
			`"output_tokens_details":{"reasoning_tokens":163}`+
			`}}}`,
	)
	messageDeltas := codexClaudeEventsByName(events, "message_delta")
	if len(messageDeltas) != 1 {
		t.Fatalf("message_delta count = %d, want 1", len(messageDeltas))
	}
	usage := messageDeltas[0].Data.Get("usage")
	for path, want := range map[string]int64{
		"input_tokens":                          50,
		"output_tokens":                         50,
		"cache_read_input_tokens":               800,
		"cache_creation_input_tokens":           150,
		"output_tokens_details.thinking_tokens": 50,
	} {
		if got := usage.Get(path).Int(); got != want {
			t.Fatalf("usage.%s = %d, want %d; usage=%s", path, got, want, usage.Raw)
		}
	}
}

func TestConvertCodexResponseToClaudeNonStreamMapsReasoningUsage(t *testing.T) {
	tests := []struct {
		name       string
		detail     string
		wantExists bool
		want       int64
	}{
		{name: "positive", detail: "20", wantExists: true, want: 20},
		{name: "explicit zero", detail: "0", wantExists: true},
		{name: "oversized", detail: "999", wantExists: true, want: 50},
		{name: "negative", detail: "-1"},
		{name: "string", detail: `"20"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(`{"type":"response.completed","response":{` +
				`"id":"resp_1","model":"gpt-5.6",` +
				`"usage":{"input_tokens":100,"output_tokens":50,` +
				`"input_tokens_details":{"cache_creation_tokens":10},` +
				`"output_tokens_details":{"reasoning_tokens":` + test.detail + `}},` +
				`"output":[]}}`)
			output := ConvertCodexResponseToClaudeNonStream(context.Background(), "gpt-5.6", nil, nil, input, nil)
			if got := gjson.GetBytes(output, "usage.cache_creation_input_tokens").Int(); got != 10 {
				t.Fatalf("cache_creation_input_tokens = %d, want 10; output=%s", got, output)
			}
			thinking := gjson.GetBytes(output, "usage.output_tokens_details.thinking_tokens")
			if thinking.Exists() != test.wantExists {
				t.Fatalf("thinking_tokens exists = %v, want %v; output=%s", thinking.Exists(), test.wantExists, output)
			}
			if test.wantExists && thinking.Int() != test.want {
				t.Fatalf("thinking_tokens = %d, want %d; output=%s", thinking.Int(), test.want, output)
			}
		})
	}
}

func TestConvertCodexResponseToClaudeEmitsSignatureOnlyReasoning(t *testing.T) {
	events := collectStructuredCodexClaudeEvents(
		t,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"early-signature"}}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"final-signature"}}`,
	)
	assertCodexClaudeBlockLifecycle(t, events)

	starts := codexClaudeEventsByName(events, "content_block_start")
	if len(starts) != 1 || starts[0].Data.Get("content_block.type").String() != "thinking" {
		t.Fatalf("thinking starts = %#v, want signature-only thinking block", starts)
	}
	var thinkingDeltaCount int
	var signatures []string
	for _, event := range codexClaudeEventsByName(events, "content_block_delta") {
		switch event.Data.Get("delta.type").String() {
		case "thinking_delta":
			thinkingDeltaCount++
		case "signature_delta":
			signatures = append(signatures, event.Data.Get("delta.signature").String())
		}
	}
	if thinkingDeltaCount != 0 {
		t.Fatalf("thinking delta count = %d, want 0", thinkingDeltaCount)
	}
	if len(signatures) != 1 || signatures[0] != "final-signature" {
		t.Fatalf("signatures = %v, want final-signature", signatures)
	}
}
