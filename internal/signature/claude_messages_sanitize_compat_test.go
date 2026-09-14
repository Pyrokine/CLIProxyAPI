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
