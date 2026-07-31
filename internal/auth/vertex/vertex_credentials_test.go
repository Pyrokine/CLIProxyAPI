package vertex

import (
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
