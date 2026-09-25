package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCPA09OpenAIStreamUsesArrayPositionWhenToolIndexMissing(t *testing.T) {
	events := runOpenAIClaudeStream(
		t,
		`{"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[
			{"id":"call_1","function":{"name":"first","arguments":"{\"a\":1}"}},
			{"id":"call_2","function":{"name":"second","arguments":"{\"b\":2}"}}
		]},"finish_reason":"tool_calls"}]}`,
	)
	starts := toolUseStarts(events)
	if len(starts) != 2 {
		t.Fatalf("tool_use start count = %d, want 2", len(starts))
	}
	if got := gjson.Get(starts[0].Payload, "content_block.name").String(); got != "first" {
		t.Fatalf("first tool name = %q", got)
	}
	if got := gjson.Get(starts[1].Payload, "content_block.name").String(); got != "second" {
		t.Fatalf("second tool name = %q", got)
	}
}

func TestCPA09OpenAIStreamKeepsOneActiveContentBlock(t *testing.T) {
	events := runOpenAIClaudeStream(
		t,
		`{"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"first","arguments":"{\"a\":"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"delta":{"content":"between","tool_calls":[{"index":1,"id":"call_2","function":{"name":"second","arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
	)
	active := false
	for _, event := range events {
		switch event.Type {
		case "content_block_start":
			if active {
				t.Fatalf("content block started before previous block stopped: %s", event.Payload)
			}
			active = true
		case "content_block_stop":
			if !active {
				t.Fatalf("content block stopped without an active block: %s", event.Payload)
			}
			active = false
		}
	}
	if active {
		t.Fatal("content block remained open")
	}
	if got := len(toolUseStarts(events)); got != 2 {
		t.Fatalf("tool_use start count = %d, want 2", got)
	}
	foundText := false
	for _, event := range events {
		if event.Type == "content_block_delta" && gjson.Get(event.Payload, "delta.type").String() == "text_delta" &&
			gjson.Get(event.Payload, "delta.text").String() == "between" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatal("interleaved text was not emitted")
	}
}

func TestCPA09OpenAIStreamDoneSynthesizesMissingFinishReason(t *testing.T) {
	events := runOpenAIClaudeStream(
		t,
		`{"id":"c1","model":"m","choices":[{"delta":{"content":"answer"}}]}`,
	)
	if got := countSSEEvents(events, "message_delta"); got != 1 {
		t.Fatalf("message_delta count = %d, want 1", got)
	}
	if got := countSSEEvents(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop count = %d, want 1", got)
	}
	if got := lastStopReason(events); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
}

func TestCPA09OpenAIStreamEmitsDelayedUsageAndTerminalOnce(t *testing.T) {
	var param any
	request := []byte(`{"stream":true}`)
	chunks := [][]byte{
		[]byte(`data: {"id":"c1","model":"m","choices":[{"delta":{"content":"answer"}}]}`),
		[]byte(`data: {"id":"c1","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`data: {"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4,"cache_creation_tokens":2}}}`),
		[]byte(`data: [DONE]`),
		[]byte(`data: [DONE]`),
	}
	var events []sseEvent
	for _, chunk := range chunks {
		for _, raw := range convertOpenAIResponseToClaude(
			context.Background(), "test-model", request, nil, chunk, &param,
		) {
			firstLine, rest, ok := strings.Cut(string(raw), "\n")
			if !ok || !strings.HasPrefix(firstLine, "event: ") || !strings.HasPrefix(rest, "data: ") {
				continue
			}
			events = append(events, sseEvent{
				Type:    strings.TrimPrefix(firstLine, "event: "),
				Payload: strings.TrimSpace(strings.TrimPrefix(rest, "data: ")),
			})
		}
	}
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
		if got := gjson.Get(event.Payload, "usage.input_tokens").Int(); got != 6 {
			t.Fatalf("usage.input_tokens = %d, want 6", got)
		}
		if got := gjson.Get(event.Payload, "usage.cache_read_input_tokens").Int(); got != 4 {
			t.Fatalf("cache_read_input_tokens = %d, want 4", got)
		}
		if got := gjson.Get(event.Payload, "usage.cache_creation_input_tokens").Int(); got != 2 {
			t.Fatalf("cache_creation_input_tokens = %d, want 2", got)
		}
	}
}
