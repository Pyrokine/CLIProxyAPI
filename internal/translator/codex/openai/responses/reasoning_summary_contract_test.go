package responses_test

import (
	"context"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexResponsesSDKAggregatedReasoningSummary(t *testing.T) {
	response := `{"id":"fixture","model":"grok-test","status":"completed","output":[{"i` +
		`d":"reasoning-a","type":"reasoning","encrypted_content":"opaque","cont` +
		`ent":[{"type":"reasoning_text","text":"first"},{"type":"reasoning_text` +
		`","text":"second"}]}],"usage":{"input_tokens":9007199254740993}}`
	for _, body := range []string{response, `{"type":"response.completed","response":` + response + `}`} {
		var state any
		out := sdk.TranslateNonStream(
			context.Background(),
			sdk.FormatCodex,
			sdk.FormatOpenAIResponse,
			"grok-test",
			nil,
			nil,
			[]byte(body),
			&state,
		)
		if gjson.GetBytes(out, "output.0.summary.0.type").String() != "summary_text" || gjson.GetBytes(
			out, "output.0.summary.0.text",
		).String() != "first" || gjson.GetBytes(
			out, "output.0.summary.1.text",
		).String() != "second" || gjson.GetBytes(
			out, "output.0.encrypted_content",
		).String() != "opaque" || gjson.GetBytes(
			out, "usage.input_tokens",
		).Raw != "9007199254740993" || gjson.GetBytes(out, "status").String() != "completed" {
			t.Fatalf("aggregate reasoning contract changed: %s", out)
		}
	}
}
