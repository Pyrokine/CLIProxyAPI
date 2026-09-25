package signature

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeClaudeMessagesForClaudeUpstreamPreservesThinkingInCompatMode(t *testing.T) {
	tests := []struct {
		name      string
		input     []byte
		thinking  string
		signature string
	}{
		{
			name:      "empty",
			input:     []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":""}]}]}`),
			signature: "",
		},
		{
			name:      "opaque",
			input:     []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"opaque-deepseek-id"}]}]}`),
			thinking:  "reason",
			signature: "opaque-deepseek-id",
		},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				withCompat, _ := SanitizeClaudeMessagesForClaudeUpstream(
					testCase.input,
					"deepseek-v4",
					true,
				)
				part := gjson.GetBytes(withCompat, "messages.0.content.0")
				if part.Get("type").String() != "thinking" ||
					part.Get("thinking").String() != testCase.thinking ||
					part.Get("signature").String() != testCase.signature {
					t.Fatalf("compat sanitizer changed thinking block: %s", withCompat)
				}
			},
		)
	}
}

func TestCollapseConsecutiveClaudeThinkingBlocks(t *testing.T) {
	input := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"old","signature":"old-sig"},` +
		`{"type":"thinking","thinking":"latest","signature":"latest-sig"},` +
		`{"type":"tool_use","id":"tool-1","name":"lookup","input":{}},` +
		`{"type":"thinking","thinking":"after tool","signature":"tool-sig"},` +
		`{"type":"thinking","thinking":"after tool latest","signature":"tool-latest-sig"}` +
		`]}]}`)

	output := CollapseConsecutiveClaudeThinkingBlocks(input)
	content := gjson.GetBytes(output, "messages.0.content").Array()
	if len(content) != 3 {
		t.Fatalf("content length = %d, want 3; output=%s", len(content), output)
	}
	if got := content[0].Get("thinking").String(); got != "latest" {
		t.Fatalf("first thinking = %q, want latest", got)
	}
	if got := content[1].Get("type").String(); got != "tool_use" {
		t.Fatalf("separator type = %q, want tool_use", got)
	}
	if got := content[2].Get("thinking").String(); got != "after tool latest" {
		t.Fatalf("thinking after tool = %q, want latest block", got)
	}
}

func TestStripTrailingClaudeThinkingBlocks(t *testing.T) {
	input := []byte(`{"messages":[` +
		`{"role":"user","content":"question"},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"answer"},` +
		`{"type":"thinking","thinking":"tail","signature":"sig"},` +
		`{"type":"redacted_thinking","data":"opaque"}` +
		`]}` +
		`]}`)

	output := StripTrailingClaudeThinkingBlocks(input)
	content := gjson.GetBytes(output, "messages.1.content").Array()
	if len(content) != 1 || content[0].Get("text").String() != "answer" {
		t.Fatalf("trailing thinking was not removed: %s", output)
	}

	onlyThinking := []byte(`{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"tail","signature":"sig"}]}]}`)
	stripped := StripTrailingClaudeThinkingBlocks(onlyThinking)
	if got := gjson.GetBytes(stripped, "messages.#").Int(); got != 1 {
		t.Fatalf("message count = %d, want 1; output=%s", got, stripped)
	}
}

func TestStripTrailingClaudeAssistantPrefillIsCapabilityNeutral(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":"prefill"}]}`)
	output := StripTrailingClaudeAssistantPrefill(input)
	if got := gjson.GetBytes(output, "messages.#").Int(); got != 1 {
		t.Fatalf("message count = %d, want 1; output=%s", got, output)
	}

	unchanged := []byte(`{"messages":[{"role":"user","content":"question"}]}`)
	reused := StripTrailingClaudeAssistantPrefill(unchanged)
	if &reused[0] != &unchanged[0] {
		t.Fatal("payload without assistant prefill was copied")
	}
}
