package store

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestNormalizeGitStoreBranch(t *testing.T) {
	tests := map[string]string{
		" main ":          "main",
		"refs/heads/main": "main",
		"/release/":       "release",
		"":                "",
	}
	for input, want := range tests {
		if got := normalizeGitStoreBranch(input); got != want {
			t.Fatalf("normalizeGitStoreBranch(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGitTokenStoreBranchRef(t *testing.T) {
	store := NewGitTokenStore("https://example.com/repo.git", "", "", "refs/heads/main")
	if got := store.branchRef(); got != plumbing.NewBranchReferenceName("main") {
		t.Fatalf("branchRef() = %q, want refs/heads/main", got)
	}
}
