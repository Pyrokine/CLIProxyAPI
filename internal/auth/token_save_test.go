package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveTokenJSONFailurePreservesExistingCredential(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "token.json")
	original := []byte(`{"access_token":"old"}`)
	if errWrite := os.WriteFile(filePath, original, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	storage := struct {
		Unsupported chan int `json:"unsupported"`
	}{Unsupported: make(chan int)}
	if errSave := SaveTokenJSON(filePath, storage, nil); errSave == nil {
		t.Fatal("SaveTokenJSON() succeeded with unsupported value")
	}
	current, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(current) != string(original) {
		t.Fatalf("credential changed after failed save: %q", current)
	}
}

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
