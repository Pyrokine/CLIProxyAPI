package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileCooldownStateStoreSecuresDirectoryAndFile(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auth")
	if errMkdir := os.MkdirAll(authDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	now := time.Now().UTC()
	if errSave := store.Save(
		context.Background(), []CooldownStateRecord{
			{
				AuthID:         "auth-1",
				AuthFile:       filepath.Join(authDir, "auth.json"),
				Model:          "gpt-test",
				NextRetryAfter: now.Add(time.Hour),
				UpdatedAt:      now,
			},
		},
	); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}

	dirInfo, errStatDir := os.Stat(authDir)
	if errStatDir != nil {
		t.Fatalf("Stat(authDir) error = %v", errStatDir)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("auth directory mode = %04o, want 0700", got)
	}
	fileInfo, errStatFile := os.Stat(filepath.Join(authDir, "auth.cds"))
	if errStatFile != nil {
		t.Fatalf("Stat(cooldown file) error = %v", errStatFile)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("cooldown file mode = %04o, want 0600", got)
	}
}
