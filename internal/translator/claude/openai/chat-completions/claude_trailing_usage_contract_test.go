package chat_completions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/chat-completions"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeChatSDKTrailingUsageAndCacheWriteAlias(t *testing.T) {
	for _, stop := range []struct{ source, target string }{
		{"end_turn", "stop"}, {"tool_use", "tool_calls"}, {"max_tokens", "length"},
	} {
		t.Run(
			stop.source, func(t *testing.T) {
				var state any
				var frames [][]byte
				for _, event := range []string{
					`data: {"type":"message_start","message":{"id":"usage-fixture","usage":{"input_tokens":` +
						`10,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}}}`,
					`data: {"type":"message_delta","delta":{"stop_reason":"` +
						stop.source +
						`"},"usage":{"output_tokens":3}}`,
					`data: {"type":"message_stop"}`,
					`data: {"type":"message_stop"}`,
				} {
					frames = append(
						frames, sdk.Default().TranslateStream(
							context.Background(),
							sdk.FormatClaude, sdk.FormatOpenAI, "claude-fixture", nil, nil,
							[]byte(event), &state,
						)...,
					)
				}
				trailing, finishes := 0, 0
				for _, frame := range frames {
					if gjson.GetBytes(frame, "choices.0.finish_reason").String() == stop.target {
						finishes++
					}
					if !gjson.GetBytes(frame, "usage").Exists() {
						continue
					}
					if gjson.GetBytes(
						frame, "usage.prompt_tokens_details.cache_write_tokens",
					).Int() != 5 || gjson.GetBytes(
						frame, "usage.prompt_tokens_details.cached_creation_tokens",
					).Int() != 5 {
						t.Fatalf("cache alias missing: %s", frame)
					}
					if gjson.GetBytes(frame, "choices.#").Int() == 0 {
						trailing++
						for path, want := range map[string]int64{
							"usage.prompt_tokens": 19, "usage.completion_tokens": 3, "usage.total_tokens": 22,
						} {
							if gjson.GetBytes(frame, path).Int() != want {
								t.Fatalf("%s differs: %s", path, frame)
							}
						}
						if gjson.GetBytes(frame, "id").String() != "usage-fixture" || gjson.GetBytes(
							frame, "model",
						).String() != "claude-fixture" {
							t.Fatalf("trailing identity changed: %s", frame)
						}
					}
				}
				if trailing != 1 || finishes != 1 || len(frames) != 3 {
					t.Fatalf("trailing=%d finishes=%d frames=%q", trailing, finishes, frames)
				}
			},
		)
	}
}

func TestClaudeChatSDKTrailingUsagePresence(t *testing.T) {
	for _, hasUsage := range []bool{false, true} {
		t.Run(
			fmt.Sprint(hasUsage), func(t *testing.T) {
				var state any
				start := `data: {"type":"message_start","message":{"id":"zero-usage"}}`
				if hasUsage {
					start = `data: {"type":"message_start","message":{"id":"zero-usage","usage":` +
						`{"input_tokens":0,"output_tokens":` +
						`0}}}`
				}
				sdk.Default().TranslateStream(
					context.Background(),
					sdk.FormatClaude, sdk.FormatOpenAI, "fixture", nil, nil, []byte(start), &state,
				)
				frames := sdk.Default().TranslateStream(
					context.Background(), sdk.FormatClaude, sdk.FormatOpenAI, "fixture", nil, nil,
					[]byte(`data: {"type":"message_stop"}`), &state,
				)
				if hasUsage {
					if len(frames) != 1 || !gjson.GetBytes(
						frames[0], "usage.prompt_tokens_details.cache_write_tokens",
					).Exists() || gjson.GetBytes(frames[0], "usage.total_tokens").Int() != 0 {
						t.Fatalf("explicit zero usage lost: %q", frames)
					}
				} else if len(frames) != 0 {
					t.Fatalf("usage invented without source accounting: %q", frames)
				}
			},
		)
	}
}

func TestClaudeChatSDKAggregateCacheWriteAlias(t *testing.T) {
	usage := `{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}`
	for _, body := range []string{
		`{"type":"message","id":"fixture","model":"fixture","content":[{"type":` +
			`"text","text":"response"}],"stop_reason":"end_turn","usage":` + usage + `}`,
		strings.Join(
			[]string{
				`data: {"type":"message_start","message":{"id":"fixture","usage":` + usage + `}}`,
				`data: {"type":"message_stop"}`,
			}, "\n",
		),
	} {
		var state any
		out := sdk.TranslateNonStream(
			context.Background(), sdk.FormatClaude, sdk.FormatOpenAI, "fixture", nil, nil, []byte(body), &state,
		)
		if gjson.GetBytes(out, "usage.prompt_tokens_details.cache_write_tokens").Int() != 5 || gjson.GetBytes(
			out, "usage.prompt_tokens_details.cached_creation_tokens",
		).Int() != 5 || gjson.GetBytes(out, "usage.total_tokens").Int() != 22 {
			t.Fatalf("aggregate cache alias changed: %s", out)
		}
	}
}
