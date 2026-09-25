package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type disabledTestTokenStorage struct {
	metadata map[string]any
	saves    int
}

func (s *disabledTestTokenStorage) SetMetadata(metadata map[string]any) {
	s.metadata = metadata
}

func (s *disabledTestTokenStorage) SaveTokenToFile(path string) error {
	s.saves++
	raw, errMarshal := json.Marshal(s.metadata)
	if errMarshal != nil {
		return errMarshal
	}
	return os.WriteFile(path, raw, 0o600)
}

func TestFileTokenStoreDisabledStoragePersistsFlag(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "disabled.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"test","disabled":true}`), 0o600); errWrite != nil {
		t.Fatalf("seed credential: %v", errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	storage := &disabledTestTokenStorage{}
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	if _, errSave := store.Save(context.Background(), auth); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read credential: %v", errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
		t.Fatalf("decode credential: %v", errDecode)
	}
	if disabled, _ := metadata["disabled"].(bool); !disabled {
		t.Fatalf("disabled = %#v, want true", metadata["disabled"])
	}
}

func TestFileTokenStoreDisabledStorageRequiresCreationIntentForMissingFile(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "disabled.json")
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	storage := &disabledTestTokenStorage{}
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	savedPath, errSave := store.Save(context.Background(), auth)
	if errSave != nil || savedPath != "" {
		t.Fatalf("runtime Save() path/error = %q/%v, want skipped", savedPath, errSave)
	}
	if storage.saves != 0 {
		t.Fatalf("runtime Save() storage calls = %d, want 0", storage.saves)
	}
	if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("runtime Save() credential stat error = %v, want os.ErrNotExist", errStat)
	}

	ctx := cliproxyauth.WithAuthCreationIntent(context.Background())
	savedPath, errSave = store.Save(ctx, auth)
	if errSave != nil || savedPath != path {
		t.Fatalf("creation-intent Save() path/error = %q/%v, want %q", savedPath, errSave, path)
	}
	if storage.saves != 1 {
		t.Fatalf("creation-intent Save() storage calls = %d, want 1", storage.saves)
	}
}
