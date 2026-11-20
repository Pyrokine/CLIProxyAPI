package responses_test

import (
	"context"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/responses"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/claude"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeResponsesSDKSplitSignatureRoundTrip(t *testing.T) {
	events := []string{
		`data: {"type":"message_start","message":{"id":"msg","model":"claude-test"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EgQK"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"AggM"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, `data: {"type":"message_stop"}`,
	}
	for _, mode := range []string{"stream", "buffered"} {
		var state any
		var response []byte
		if mode == "buffered" {
			response = sdktranslator.TranslateNonStream(
				context.Background(),
				sdktranslator.FormatClaude,
				sdktranslator.FormatOpenAIResponse,
				"claude-test",
				nil,
				nil,
				[]byte(strings.Join(events, "\n\n")),
				&state,
			)
		} else {
			for _, event := range events {
				for _, frame := range sdktranslator.Default().TranslateStream(
					context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, "claude-test",
					nil, nil, []byte(event), &state,
				) {
					for line := range strings.SplitSeq(string(frame), "\n") {
						if payload, ok := strings.CutPrefix(line, "data: "); ok && gjson.Get(
							payload, "type",
						).String() == "response.completed" {
							response = []byte(gjson.Get(payload, "response").Raw)
						}
					}
				}
			}
		}
		if err := sdktranslator.ResponseTranslationError(&state); err != nil {
			t.Fatal(err)
		}
		item := gjson.GetBytes(response, "output.0")
		if item.Get("encrypted_content").String() != "EgQKAggM" {
			t.Fatalf("%s signature fragments lost: %s", mode, response)
		}
		request := []byte(
			`{"input":[{"type":"message","role":"user","content":"query"},` +
				item.Raw +
				(`,{"type":"message",` +
					`"role":"user",` +
					`"content":` +
					`"continue"}]}`),
		)
		replayed, err := sdktranslator.TranslateRequestChecked(
			context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, "claude-test",
			request, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, msg := range gjson.GetBytes(replayed, "messages").Array() {
			for _, block := range msg.Get("content").Array() {
				if block.Get("type").String() == "thinking" && block.Get("signature").String() == "EgQKAggM" &&
					block.Get("thinking").String() == "reason" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("%s same-provider thinking replay lost: %s", mode, replayed)
		}
	}
}

func TestClaudeResponsesSDKEquivalentUserSeeds(t *testing.T) {
	for _, text := range []string{"alpha", "beta"} {
		var previous string
		for _, input := range []string{
			`"` + text + `"`, `[{"type":"message","content":"` + text + `"}]`,
			`[{"role":"user","content":[{"type":"input_text","text":"` + text + `"}]}]`,
		} {
			out, err := sdktranslator.TranslateRequestChecked(
				context.Background(),
				sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude,
				"claude-test",
				[]byte(`{"input":`+input+`}`),
				false,
			)
			if err != nil {
				t.Fatal(err)
			}
			id := gjson.GetBytes(out, "metadata.user_id").String()
			if id == "" || (previous != "" && id != previous) {
				t.Fatalf("equivalent content identity differs: %s", out)
			}
			previous = id
		}
	}
	ids := make(map[string]bool)
	for _, text := range []string{"alpha", "beta"} {
		out, err := sdktranslator.TranslateRequestChecked(
			context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, "claude-test",
			[]byte(`{"input":"`+text+`"}`), false,
		)
		if err != nil {
			t.Fatal(err)
		}
		ids[gjson.GetBytes(out, "metadata.user_id").String()] = true
	}
	if len(ids) != 2 {
		t.Fatal("different user content shares one identity")
	}
}

func TestClaudeSDKArraySystemFiltersOnlyAttribution(t *testing.T) {
	body := []byte((`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=fixture"},{"` +
		`type":"text","text":"normal directive"}],"messages":[{"role":"user","content":"x-anthropic-billing-` +
		`header: user text"}]}`))
	out, err := sdktranslator.TranslateRequestChecked(
		context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture", body, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "messages.0.content.#").Int() != 1 || gjson.GetBytes(
		out, "messages.0.content.0.text",
	).String() != "normal directive" || gjson.GetBytes(
		out, "messages.1.content",
	).String() != "x-anthropic-billing-header: user text" {
		t.Fatalf("system attribution or user text changed: %s", out)
	}
}
