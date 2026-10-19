package claude

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

type sseEvent struct {
	Type    string
	Payload string
}

func runOpenAIClaudeStream(t *testing.T, chunks ...string) []sseEvent {
	t.Helper()
	var param any
	var emitted [][]byte
	for _, chunk := range chunks {
		emitted = append(
			emitted, convertOpenAIResponseToClaude(
				context.Background(), "test-model", []byte(`{"stream":true}`), nil, []byte("data: "+chunk), &param,
			)...,
		)
	}
	emitted = append(
		emitted, convertOpenAIResponseToClaude(
			context.Background(), "test-model", []byte(`{"stream":true}`), nil, []byte("data: [DONE]"), &param,
		)...,
	)

	events := make([]sseEvent, 0, len(emitted))
	for _, raw := range emitted {
		s := string(raw)
		if !strings.HasPrefix(s, "event: ") {
			continue
		}
		before, after, ok := strings.Cut(s, "\n")
		if !ok {
			continue
		}
		typ := strings.TrimPrefix(before, "event: ")
		rest := after
		if !strings.HasPrefix(rest, "data: ") {
			continue
		}
		events = append(
			events, sseEvent{Type: typ, Payload: strings.TrimRight(strings.TrimPrefix(rest, "data: "), "\n")},
		)
	}
	return events
}

func countSSEEvents(events []sseEvent, typ string) int {
	count := 0
	for _, event := range events {
		if event.Type == typ {
			count++
		}
	}
	return count
}

func toolUseStarts(events []sseEvent) []sseEvent {
	var starts []sseEvent
	for _, event := range events {
		if event.Type == "content_block_start" && gjson.Get(
			event.Payload, "content_block.type",
		).String() == "tool_use" {
			starts = append(starts, event)
		}
	}
	return starts
}

func lastStopReason(events []sseEvent) string {
	for _, event := range slices.Backward(events) {
		if event.Type == "message_delta" {
			return gjson.Get(event.Payload, "delta.stop_reason").String()
		}
	}
	return ""
}

func TestConvertOpenAIResponseToClaudeWaitsForFinalCachedUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  string
		finish string
		stop   string
	}{
		{
			name:   "text",
			first:  `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
			finish: `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":140025,"completion_tokens":741}}`,
			stop:   "end_turn",
		},
		{
			name:   "tool",
			first:  `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
			finish: `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":140025,"completion_tokens":741}}`,
			stop:   "tool_use",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := runOpenAIClaudeStream(t, tc.first, tc.finish,
				`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":140025,"completion_tokens":741,"prompt_tokens_details":{"cached_tokens":139136}}}`)
			if got := countSSEEvents(events, "message_delta"); got != 1 {
				t.Fatalf("message_delta count = %d, want 1", got)
			}
			if got := countSSEEvents(events, "message_stop"); got != 1 {
				t.Fatalf("message_stop count = %d, want 1", got)
			}
			for _, event := range events {
				if event.Type != "message_delta" {
					continue
				}
				if stop := gjson.Get(event.Payload, "delta.stop_reason").String(); stop != tc.stop {
					t.Errorf("stop_reason = %q, want %q", stop, tc.stop)
				}
				if cached := gjson.Get(event.Payload, "usage.cache_read_input_tokens").Int(); cached != 139136 {
					t.Errorf("cache_read_input_tokens = %d, want 139136", cached)
				}
				if input := gjson.Get(event.Payload, "usage.input_tokens").Int(); input != 889 {
					t.Errorf("input_tokens = %d, want 889", input)
				}
			}
		})
	}
}

func TestConvertOpenAIResponseToClaudeStreamIgnoresNullToolNameDelta(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	firstChunks := convertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		&param,
	)
	firstOutput := bytes.Join(firstChunks, nil)
	if !bytes.Contains(firstOutput, []byte(`"name":"read_file"`)) {
		t.Fatalf("expected first chunk to start read_file tool block, got %s", firstOutput)
	}

	secondChunks := convertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{\"path\":\"/tmp/a\"}"}}]},"finish_reason":null}]}`),
		&param,
	)
	secondOutput := bytes.Join(secondChunks, nil)
	if bytes.Contains(secondOutput, []byte(`content_block_start`)) {
		t.Fatalf("did not expect null tool name delta to start a new content block, got %s", secondOutput)
	}
	if bytes.Contains(secondOutput, []byte(`"name":""`)) {
		t.Fatalf("did not expect null tool name delta to emit an empty tool name, got %s", secondOutput)
	}
}

func TestConvertOpenAIResponseToClaudeStreamSuppressesEmptyToolNameStopReason(t *testing.T) {
	events := runOpenAIClaudeStream(
		t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":"","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{\"x\":1}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("tool_use start count = %d, want 1", len(starts))
	}
	if got := gjson.Get(starts[0].Payload, "content_block.name").String(); got != "tool_0" {
		t.Fatalf("synthesized tool name = %q, want tool_0", got)
	}
	if got := countSSEEvents(events, "content_block_delta"); got != 1 {
		t.Fatalf("content_block_delta count = %d, want 1", got)
	}
	if got := lastStopReason(events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}

func TestConvertOpenAIResponseToClaudeStreamStartsToolWhenIDArrivesAfterName(t *testing.T) {
	events := runOpenAIClaudeStream(
		t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_real"}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/tmp/a\"}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	starts := toolUseStarts(events)
	if len(starts) != 1 {
		t.Fatalf("tool_use start count = %d, want 1", len(starts))
	}
	if id := gjson.Get(starts[0].Payload, "content_block.id").String(); id != "call_real" {
		t.Fatalf("tool id = %q, want call_real", id)
	}
	if name := gjson.Get(starts[0].Payload, "content_block.name").String(); name != "read_file" {
		t.Fatalf("tool name = %q, want read_file", name)
	}
	if got := countSSEEvents(events, "content_block_stop"); got != 1 {
		t.Fatalf("content_block_stop count = %d, want 1", got)
	}
	if got := lastStopReason(events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}
