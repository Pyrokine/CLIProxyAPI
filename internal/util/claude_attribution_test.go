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
