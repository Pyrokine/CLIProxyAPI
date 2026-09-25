package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveTokenToFilePreservesCustomMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "claude.json")
	storage := &TokenStorage{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		Email:        "user@example.com",
	}
	storage.SetMetadata(map[string]any{
		"disabled":    true,
		"weight":      5,
		"custom_note": "preserved",
	})

	if errSave := storage.SaveTokenToFile(path); errSave != nil {
		t.Fatalf("SaveTokenToFile() error = %v", errSave)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read token file: %v", errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
		t.Fatalf("decode token file: %v", errDecode)
	}
	if metadata["access_token"] != "new-access-token" || metadata["refresh_token"] != "new-refresh-token" {
		t.Fatalf("token metadata = %#v", metadata)
	}
	if metadata["disabled"] != true || metadata["weight"] != float64(5) || metadata["custom_note"] != "preserved" {
		t.Fatalf("custom metadata = %#v", metadata)
	}
}
