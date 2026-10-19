package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiClaudeRetainsSplitAndLateUsage(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		frames                []string
		input, output, cached int64
	}{
		{
			name: "split before finish",
			frames: []string{
				`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4}}`,
				`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":2,"thoughtsTokenCount":3}}`,
			},
			input: 6, output: 5, cached: 4,
		},
		{
			name: "usage after finish",
			frames: []string{
				`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`,
				`{"usageMetadata":{"cachedContentTokenCount":4,"thoughtsTokenCount":3}}`,
			},
			input: 6, output: 5, cached: 4,
		},
		{
			name: "explicit zero overrides earlier value",
			frames: []string{
				`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"cachedContentTokenCount":4,"thoughtsTokenCount":3}}`,
				`{"usageMetadata":{"cachedContentTokenCount":0,"thoughtsTokenCount":0}}`,
			},
			input: 10, output: 2, cached: 0,
		},
		{
			name: "invalid late counters do not erase known usage",
			frames: []string{
				`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"cachedContentTokenCount":4,"thoughtsTokenCount":3}}`,
				`{"usageMetadata":{"promptTokenCount":null,"candidatesTokenCount":"0","cachedContentTokenCount":-1,"thoughtsTokenCount":1.5}}`,
			},
			input: 6, output: 5, cached: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			var lastUsage gjson.Result
			finals, stops := 0, 0
			for _, frame := range append(tc.frames, "[DONE]") {
				for _, chunk := range convertGeminiResponseToClaude(context.Background(), "gemini-fixture", nil, nil, []byte(frame), &state) {
					for _, line := range strings.Split(string(chunk), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						event := gjson.Parse(strings.TrimPrefix(line, "data: "))
						if event.Get("type").String() == "message_delta" {
							lastUsage = event.Get("usage")
							if event.Get("delta.stop_reason").Exists() {
								finals++
							}
						}
						if event.Get("type").String() == "message_stop" {
							stops++
						}
					}
				}
			}
			if finals != 1 || stops != 1 || lastUsage.Get("input_tokens").Int() != tc.input || lastUsage.Get("output_tokens").Int() != tc.output || !lastUsage.Get("cache_read_input_tokens").Exists() || lastUsage.Get("cache_read_input_tokens").Int() != tc.cached {
				t.Fatalf("finals=%d stops=%d usage=%s, want input=%d output=%d cached=%d", finals, stops, lastUsage.Raw, tc.input, tc.output, tc.cached)
			}
			for _, extra := range []string{`{"usageMetadata":{"promptTokenCount":999}}`, `[DONE]`} {
				if out := convertGeminiResponseToClaude(context.Background(), "gemini-fixture", nil, nil, []byte(extra), &state); len(out) != 0 {
					t.Fatalf("events after message_stop: %q", out)
				}
			}
		})
	}
}
