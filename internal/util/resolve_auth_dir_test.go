package util

import (
	"path/filepath"
	"testing"
)

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
