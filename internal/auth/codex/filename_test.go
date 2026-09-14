package codex

import "testing"

func TestC08CredentialFileNameIncludesEveryNonemptyAccountHash(t *testing.T) {
	tests := []struct {
		name string
		plan string
		hash string
		want string
	}{
		{name: "plus", plan: "Plus", hash: " account-hash ", want: "codex-account-hash-user@example.com-plus.json"},
		{name: "no plan", hash: "account-hash", want: "codex-account-hash-user@example.com.json"},
		{name: "legacy fallback", plan: "Plus", want: "codex-user@example.com-plus.json"},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := CredentialFileName(" user@example.com ", tt.plan, tt.hash, true); got != tt.want {
					t.Fatalf("CredentialFileName() = %q, want %q", got, tt.want)
				}
			},
		)
	}
}
