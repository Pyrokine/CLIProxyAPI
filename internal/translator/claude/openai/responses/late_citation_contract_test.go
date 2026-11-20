package responses_test

import (
	"context"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeResponsesSDKLateCitationStaysWithSourceText(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"citation-fixture","model":"fixture"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"reason"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"second"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_del` +
			`ta","citation":{"type":"web_search_result_location","url":"https://exa` +
			`mple.com/first","title":"First source"}}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	}
	var stream strings.Builder
	for _, event := range events {
		stream.WriteString("data: " + event + "\n\n")
	}
	var state any
	out := sdk.TranslateNonStream(
		context.Background(), sdk.FormatClaude, sdk.FormatOpenAIResponse, "fixture", []byte(`{"model":"fixture"}`), nil,
		[]byte(stream.String()), &state,
	)
	if err := sdk.ResponseTranslationError(&state); err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "output.0.content.0.text").String() != "first" || gjson.GetBytes(
		out, "output.2.content.0.text",
	).String() != "second" {
		t.Fatalf("message order or content changed: %s", out)
	}
	if gjson.GetBytes(
		out, "output.0.content.0.annotations.0.url",
	).String() != "https://example.com/first" || gjson.GetBytes(out, "output.2.content.0.annotations.#").Int() != 0 {
		t.Fatalf("late citation changed source message: %s", out)
	}
}
