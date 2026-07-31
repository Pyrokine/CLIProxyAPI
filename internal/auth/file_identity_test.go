package auth

import (
	"path/filepath"
	"testing"
)

func TestFileAuthIDUsesRelativePath(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "nested", "auth.json")
	if got := FileAuthID(path, authDir); got != filepath.Join("nested", "auth.json") {
		t.Fatalf("FileAuthID() = %q", got)
	}
}

func TestFileAuthIDMatchesAbsolutePathWithRelativeAuthDir(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	authDir := "auth"
	path := filepath.Join(root, authDir, "shared.json")
	if got := FileAuthID(path, authDir); got != "shared.json" {
		t.Fatalf("FileAuthID() = %q", got)
	}
}
