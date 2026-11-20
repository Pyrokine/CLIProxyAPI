package claude_test

import (
	"context"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/claude"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexClaudeSDKThinkingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name            string
		frames          []string
		starts, stops   int
		text, signature string
	}{
		{
			"unsigned four frames", []string{
				`{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.reasoning_summary_part.done"}`, `{"type":"response.reasoning_summary_part.added"}`,
			}, 2, 1, "first", "",
		},
		{
			"direct text boundary", []string{
				`{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.output_text.delta","delta":"answer"}`,
			}, 2, 1, "first", "",
		},
		{
			"content part boundary", []string{
				`{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.content_part.added","part":{"type":"output_text"}}`,
			}, 2, 1, "first", "",
		},
		{
			"new reasoning item boundary", []string{
				`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.output_item.added","item":{"type":"reasoning",` +
					`"encrypted_content":"second-signature"}}`,
			}, 1, 1, "first", "",
		},
		{
			"unsigned thinking before tool", []string{
				`{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.output_item.done","item":{"type":"function_call","id` +
					`":"item-a","call_id":"call-a","name":"lookup","arguments":"{}"}}`,
			}, 2, 2, "first", "",
		},
		{
			"late signature", []string{
				`{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"late-signature"}}`,
			}, 1, 1, "first", "late-signature",
		},
		{
			"signed same item summary", []string{
				`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"early"}}`,
				`{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"first"}`,
				`{"type":"response.reasoning_summary_part.done"}`, `{"type":"response.reasoning_summary_part.added"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"second"}`,
				`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"final"}}`,
			}, 1, 1, "first\n\nsecond", "final",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				var state any
				starts, stops := 0, 0
				var text, signature strings.Builder
				open := map[int]bool{}
				consume := func(frame string) {
					for _, out := range sdk.Default().TranslateStream(
						context.Background(), sdk.FormatCodex, sdk.FormatClaude, "codex-test", nil, nil,
						[]byte("data: "+frame), &state,
					) {
						for line := range strings.SplitSeq(string(out), "\n") {
							payload, ok := strings.CutPrefix(line, "data: ")
							if !ok {
								continue
							}
							event := gjson.Parse(payload)
							index := int(event.Get("index").Int())
							switch event.Get("type").String() {
							case "content_block_start":
								if open[index] {
									t.Fatalf("duplicate open: %s", out)
								}
								open[index] = true
								starts++
							case "content_block_stop":
								if !open[index] {
									t.Fatalf("orphan stop: %s", out)
								}
								delete(open, index)
								stops++
							case "content_block_delta":
								if !open[index] {
									t.Fatalf("delta outside block: %s", out)
								}
								text.WriteString(event.Get("delta.thinking").String())
								signature.WriteString(event.Get("delta.signature").String())
							}
						}
					}
				}
				for _, frame := range tc.frames {
					consume(frame)
				}
				if starts != tc.starts || stops != tc.stops || text.String() != tc.text ||
					signature.String() != tc.signature {
					t.Fatalf(
						"starts=%d stops=%d text=%q signature=%q",
						starts,
						stops,
						text.String(),
						signature.String(),
					)
				}
				consume(
					`{"type":"response.completed","response":{"output":[],"usage":` +
						`{"input_tokens":1,"output_tokens":` +
						`2}}}`,
				)
				if len(open) != 0 {
					t.Fatalf("terminal left open blocks: %v", open)
				}
			},
		)
	}
}

func TestCodexClaudeSDKNonTextPartsAndFreshRequestState(t *testing.T) {
	for request := range 2 {
		var state any
		var output strings.Builder
		for _, frame := range []string{
			`{"type":"response.content_part.added","part":{"type":"refusal"},"output_index":0,"content_index":0}`,
			`{"type":"response.content_part.done","part":{"type":"refusal"},"output_index":0,"content_index":0}`,
			`{"type":"response.completed","response":{"output":[]}}`,
		} {
			for _, out := range sdk.Default().TranslateStream(
				context.Background(), sdk.FormatCodex, sdk.FormatClaude, "codex-test", nil, nil, []byte("data: "+frame),
				&state,
			) {
				output.Write(out)
			}
		}
		if strings.Contains(output.String(), "content_block_start") || strings.Count(
			output.String(), "event: message_start",
		) != 1 || strings.Count(output.String(), "event: message_stop") != 1 {
			t.Fatalf("request %d unexpected lifecycle: %s", request, output.String())
		}
	}
	var state any
	out := sdk.TranslateNonStream(
		context.Background(), sdk.FormatCodex, sdk.FormatClaude, "codex-test", nil, nil,
		[]byte(`{"type":"response.completed","response":{"output":[{"type":"reasoning"`+
			`,"summary":[{"type":"summary_text","text":"unsigned body"}]}]}}`),
		&state,
	)
	if gjson.GetBytes(out, "content.0.thinking").String() != "unsigned body" || gjson.GetBytes(
		out, "content.0.signature",
	).Exists() {
		t.Fatalf("unsigned nonstream body lost or signature fabricated: %s", out)
	}
}
