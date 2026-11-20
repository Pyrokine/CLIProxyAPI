package util

import (
	"path/filepath"
	"testing"
)

func TestResolveAuthDirContract(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Chdir(root)
	//goland:noinspection SpellCheckingInspection
	for _, input := range []string{
		"~/tokens", `~\tokens`, "tokens", "./tokens/../tokens", filepath.Join(root, "tokens"),
	} {
		t.Run(
			input, func(t *testing.T) {
				got, err := ResolveAuthDir(input)
				want := filepath.Join(root, "tokens")
				if err != nil || got != want || !filepath.IsAbs(got) {
					t.Fatalf("ResolveAuthDir(%q) = %q, %v; want %q", input, got, err, want)
				}
			},
		)
	}
	t.Setenv("HOME", "")
	if _, err := ResolveAuthDir("~/tokens"); err == nil {
		t.Fatal("unresolvable home directory accepted")
	}
	if got, err := ResolveAuthDir(root); err != nil || got != root {
		t.Fatalf("absolute directory gained a home-directory prerequisite: %q, %v", got, err)
	}
}

func TestResolveAuthDirUsesDefaultWhenEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := ResolveAuthDir("")
	if err != nil {
		t.Fatalf("ResolveAuthDir error = %v", err)
	}
	want := filepath.Join(home, ".cli-proxy-api")
	if got != want {
		t.Fatalf("ResolveAuthDir empty = %q, want %q", got, want)
	}
}
