package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveTokenJSON_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "auth", "test-token.json")

	type tokenData struct {
		Token string `json:"token"`
	}
	storage := tokenData{Token: "secret-value"}
	metadata := map[string]any{"provider": "test"}

	if err := SaveTokenJSON(filePath, storage, metadata); err != nil {
		t.Fatalf("SaveTokenJSON failed: %v", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("expected file permissions 0600, got %o", perm)
	}
}
