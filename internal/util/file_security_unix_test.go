//go:build unix

package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenOrCreateDirectoryRootRejectsDifferentOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing directory owner requires root")
	}
	dir := filepath.Join(t.TempDir(), "attacker-owned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir directory: %v", err)
	}
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatalf("chown directory: %v", err)
	}
	if _, err := OpenOrCreateDirectoryRoot(dir, 0o700); err == nil {
		t.Fatal("OpenOrCreateDirectoryRoot accepted a different owner")
	}
}
