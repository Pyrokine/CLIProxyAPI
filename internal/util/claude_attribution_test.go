package util

import "testing"

func TestIsClaudeCodeAttributionSystemText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "billing header",
			text: "x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;",
			want: true,
		},
		{
			name: "trimmed billing header",
			text: "  x-anthropic-billing-header: cc_version=2.1.63.abc;  ",
			want: true,
		},
		{
			name: "regular system prompt",
			text: "You are a helpful assistant",
			want: false,
		},
		{
			name: "Claude Code identity remains system content",
			text: "You are Claude Code, Anthropic's official CLI for Claude.",
			want: false,
		},
		{
			name: "embedded CCH text is not attribution",
			text: "Keep the literal cch=00000; in this system instruction",
			want: false,
		},
		{
			name: "billing header must start the block",
			text: "Keep this note before x-anthropic-billing-header: cch=00000;",
			want: false,
		},
		{
			name: "mixed billing and caller instructions remain",
			text: "x-anthropic-billing-header: cch=00000;\nKeep this caller instruction",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				if got := IsClaudeCodeAttributionSystemText(tc.text); got != tc.want {
					t.Fatalf("IsClaudeCodeAttributionSystemText() = %v, want %v", got, tc.want)
				}
			},
		)
	}
}
