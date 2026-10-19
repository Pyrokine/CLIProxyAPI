package claude

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAIUsageCacheWriteDeduction(t *testing.T) {
	for _, tt := range []struct {
		name  string
		usage string
		want  int64
		write int64
	}{
		{"read and write", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}`, 50, 30},
		{"creation alias", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":-1,"cache_creation_tokens":30}}`, 50, 30},
		{"negative input", `{"prompt_tokens":-10}`, 0, 0},
		{"saturating deductions", fmt.Sprintf(`{"prompt_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}}`, int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64)), 0, math.MaxInt64},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input, _, _, write := extractOpenAIUsage(gjson.Parse(tt.usage))
			if input != tt.want || write != tt.write {
				t.Fatalf("input/write = %d/%d, want %d/%d", input, write, tt.want, tt.write)
			}
			response := []byte(`{"id":"fixture","choices":[{"message":{"content":"answer"},"finish_reason":"stop"}],"usage":` + tt.usage + `}`)
			out := convertOpenAIResponseToClaudeNonStream(context.Background(), "fixture", nil, nil, response, nil)
			if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != tt.want {
				t.Fatalf("nonstream input = %d, want %d; %s", got, tt.want, out)
			}
		})
	}
}

func TestOpenAIReasoningAliasesReachAllClaudeConverters(t *testing.T) {
	for _, alias := range []string{"reasoning_content", "reasoning", "reasoning_text", "reasoning_details"} {
		t.Run(alias, func(t *testing.T) {
			message := fmt.Sprintf(`{"role":"assistant","content":"answer",%q:[{"text":"thinking"}]}`, alias)
			response := []byte(`{"id":"fixture","choices":[{"message":` + message + `,"finish_reason":"stop"}]}`)
			for _, out := range append(convertOpenAINonStreamingToAnthropic(response), convertOpenAIResponseToClaudeNonStream(context.Background(), "fixture", nil, nil, response, nil)) {
				var thinking string
				for _, block := range gjson.GetBytes(out, "content").Array() {
					if block.Get("type").String() == "thinking" {
						thinking += block.Get("thinking").String()
					}
				}
				if thinking != "thinking" {
					t.Fatalf("reasoning alias lost: %s", out)
				}
			}
			var param any
			chunks := convertOpenAIResponseToClaude(context.Background(), "fixture", []byte(`{"stream":true}`), nil, []byte(`data: {"id":"fixture","choices":[{"delta":`+message+`}]}`), &param)
			if !bytes.Contains(bytes.Join(chunks, nil), []byte(`"thinking":"thinking"`)) {
				t.Fatalf("stream reasoning alias lost: %q", chunks)
			}
			var registered any
			chunks = sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "fixture", []byte(`{"stream":true}`), nil, []byte(`data: {"id":"fixture","choices":[{"delta":`+message+`}]}`), &registered)
			if !bytes.Contains(bytes.Join(chunks, nil), []byte(`"thinking":"thinking"`)) {
				t.Fatalf("registered stream reasoning alias lost: %q", chunks)
			}
		})
	}
}

func TestRegisteredOpenAIToClaudeToolTerminalReasons(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
		reason    string
		want      string
	}{
		{"valid tool", `{"path":"a"}`, "tool_calls", "tool_use"},
		{"length overrides tool", `{"path":"a"}`, "length", "max_tokens"},
		{"filter overrides tool", `{"path":"a"}`, "content_filter", "end_turn"},
		{"truncated with stop", `{"path":`, "stop", "max_tokens"},
		{"truncated without reason", `{"path":`, "", "max_tokens"},
		{"whitespace arguments", " \t", "", "max_tokens"},
		{"array arguments", `[]`, "tool_calls", "max_tokens"},
		{"empty arguments", "", "tool_calls", "tool_use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			var frames [][]byte
			raws := []string{fmt.Sprintf(`data: {"id":"fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":%q}}]}}]}`, tc.arguments)}
			if tc.reason != "" {
				raws = append(raws, fmt.Sprintf(`data: {"choices":[{"delta":{},"finish_reason":%q}]}`, tc.reason))
			}
			raws = append(raws, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`, "data: [DONE]")
			for _, raw := range raws {
				frames = append(frames, sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "fixture", []byte(`{"stream":true}`), nil, []byte(raw), &state)...)
			}
			if err := sdktranslator.ResponseTranslationError(&state); err != nil {
				t.Fatal(err)
			}
			deltas, stops := 0, 0
			for _, frame := range frames {
				for _, line := range bytes.Split(frame, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data:")) {
						continue
					}
					event := gjson.ParseBytes(bytes.TrimSpace(line[5:]))
					switch event.Get("type").String() {
					case "message_delta":
						deltas++
						if got := event.Get("delta.stop_reason").String(); got != tc.want {
							t.Errorf("stop reason = %q, want %q", got, tc.want)
						}
						if event.Get("usage.input_tokens").Int() != 3 || event.Get("usage.output_tokens").Int() != 2 {
							t.Errorf("trailing usage lost: %s", event.Raw)
						}
					case "message_stop":
						stops++
					}
				}
			}
			if deltas != 1 || stops != 1 {
				t.Fatalf("terminal counts = %d/%d, want 1/1", deltas, stops)
			}
		})
	}
}
