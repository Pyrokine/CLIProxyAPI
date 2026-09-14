package chat_completions

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func assertCachedCreationTokens(t *testing.T, payload []byte, want int64) {
	t.Helper()

	got := gjson.GetBytes(payload, "usage.prompt_tokens_details.cached_creation_tokens")
	if !got.Exists() || got.Int() != want {
		t.Fatalf("cached_creation_tokens = %s, want present with value %d; output: %s", got.Raw, want, payload)
	}
}

func TestConvertClaudeResponseToOpenAIIncludesCachedCreationTokens(t *testing.T) {
	var param any
	result := convertClaudeResponseToOpenAI(
		context.Background(),
		"claude-test",
		nil,
		nil,
		[]byte(`data: {"type":"message_delta","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}}`),
		&param,
	)
	if len(result) != 1 {
		t.Fatalf("result count = %d, want 1", len(result))
	}
	assertCachedCreationTokens(t, result[0], 5)
}

func TestConvertClaudeResponseToOpenAINonStreamIncludesCachedCreationTokens(t *testing.T) {
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_1"}}`,
			`data: {"type":"message_delta","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	result := convertClaudeResponseToOpenAINonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	assertCachedCreationTokens(t, result, 5)
}

func TestConvertClaudeResponseToOpenAINonStreamFinishReasons(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		want       string
	}{
		{name: "missing", want: "stop"},
		{name: "end_turn", stopReason: "end_turn", want: "stop"},
		{name: "stop_sequence", stopReason: "stop_sequence", want: "stop"},
		{name: "max_tokens", stopReason: "max_tokens", want: "length"},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				raw := []byte(`data: {"type":"message_delta","delta":{"stop_reason":"` + testCase.stopReason + `"}}`)
				output := convertClaudeResponseToOpenAINonStream(
					context.Background(),
					"",
					nil,
					nil,
					raw,
					nil,
				)
				if got := gjson.GetBytes(output, "choices.0.finish_reason").String(); got != testCase.want {
					t.Fatalf("finish_reason = %q, want %q", got, testCase.want)
				}
			},
		)
	}
}

func TestConvertClaudeResponseToOpenAIMergesUsageAcrossEvents(t *testing.T) {
	var param any
	_ = convertClaudeResponseToOpenAI(
		context.Background(),
		"claude-test",
		nil,
		nil,
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}}}`),
		&param,
	)
	result := convertClaudeResponseToOpenAI(
		context.Background(),
		"claude-test",
		nil,
		nil,
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":3}}`),
		&param,
	)
	if len(result) != 1 {
		t.Fatalf("result count = %d, want 1", len(result))
	}
	assertClaudeOpenAIUsage(t, result[0], 19, 3, 22, 4, 5)
}

func TestConvertClaudeResponseToOpenAINonStreamMergesUsageAcrossEvents(t *testing.T) {
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}}}`,
			`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
		}, "\n",
	))

	result := convertClaudeResponseToOpenAINonStream(
		context.Background(),
		"claude-test",
		nil,
		nil,
		raw,
		nil,
	)
	assertClaudeOpenAIUsage(t, result, 19, 3, 22, 4, 5)
}

func assertClaudeOpenAIUsage(
	t *testing.T,
	payload []byte,
	promptTokens, completionTokens, totalTokens, cachedTokens, cachedCreationTokens int64,
) {
	t.Helper()

	usage := gjson.GetBytes(payload, "usage")
	checks := []struct {
		path string
		want int64
	}{
		{path: "prompt_tokens", want: promptTokens},
		{path: "completion_tokens", want: completionTokens},
		{path: "total_tokens", want: totalTokens},
		{path: "prompt_tokens_details.cached_tokens", want: cachedTokens},
		{path: "prompt_tokens_details.cached_creation_tokens", want: cachedCreationTokens},
	}
	for _, check := range checks {
		if got := usage.Get(check.path).Int(); got != check.want {
			t.Fatalf("usage.%s = %d, want %d; output: %s", check.path, got, check.want, payload)
		}
	}
}
