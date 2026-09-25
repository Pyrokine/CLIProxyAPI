package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaudeCollapsesAdjacentReasoning(t *testing.T) {
	first := validClaudeThinkingSignature()
	second := validClaudeThinkingSignature()
	input := []byte(`{"model":"claude-test","input":[{"type":"reasoning","encrypted_content":"` + first + `","summary":[{"type":"summary_text","text":"old"}]},{"type":"reasoning","encrypted_content":"` + second + `","summary":[{"type":"summary_text","text":"latest"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)

	output := convertOpenAIResponsesRequestToClaudeDefault("claude-test", input, false)
	root := gjson.ParseBytes(output)
	if got := root.Get("messages.0.content.#").Int(); got != 1 {
		t.Fatalf("assistant content blocks = %d, want 1: %s", got, output)
	}
	if got := root.Get("messages.0.content.0.thinking").String(); got != "latest" {
		t.Fatalf("thinking = %q, want latest: %s", got, output)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user: %s", got, output)
	}
}
