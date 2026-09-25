package vertex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveTokenToFileSecuresExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "vertex.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write existing auth file: %v", err)
	}

	storage := &CredentialStorage{
		ServiceAccount: map[string]any{
			"project_id":   "example-project",
			"client_email": "svc@example.test",
		},
	}
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat auth file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("auth file mode = %o, want 600", got)
	}
}

func TestSaveTokenToFilePersistsInjectedMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "vertex.json")
	storage := &CredentialStorage{
		ServiceAccount: map[string]any{
			"project_id":   "example-project",
			"client_email": "svc@example.test",
		},
		ProjectID: "example-project",
		Email:     "svc@example.test",
		Location:  "us-central1",
	}
	storage.SetMetadata(map[string]any{
		"disabled":    true,
		"weight":      7,
		"custom_note": "preserved",
	})

	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile() error = %v", err)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read auth file: %v", errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
		t.Fatalf("decode auth file: %v", errDecode)
	}
	if metadata["type"] != "vertex" {
		t.Fatalf("type = %#v, want vertex", metadata["type"])
	}
	if metadata["project_id"] != "example-project" {
		t.Fatalf("project_id = %#v, want example-project", metadata["project_id"])
	}
	if metadata["disabled"] != true {
		t.Fatalf("disabled = %#v, want true", metadata["disabled"])
	}
	if metadata["weight"] != float64(7) {
		t.Fatalf("weight = %#v, want 7", metadata["weight"])
	}
	if metadata["custom_note"] != "preserved" {
		t.Fatalf("custom_note = %#v, want preserved", metadata["custom_note"])
	}
	if _, nested := metadata["metadata"]; nested {
		t.Fatal("metadata was persisted as a nested object")
	}
}
