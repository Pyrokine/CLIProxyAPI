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

func TestFileTokenStoreDeleteIfVersionPreservesNewerCredential(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &cliproxyauth.Auth{
		ID:       "versioned.json",
		FileName: "versioned.json",
		Metadata: map[string]any{"type": "codex", "token": "old"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	oldVersion, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	newRaw := []byte(`{"type":"codex","token":"new"}`)
	if errWrite := os.WriteFile(path, newRaw, 0o600); errWrite != nil {
		t.Fatalf("write newer credential: %v", errWrite)
	}
	deleted, errDelete := store.DeleteIfVersion(context.Background(), auth.ID, oldVersion)
	if errDelete != nil || deleted {
		t.Fatalf("DeleteIfVersion(old) = (%t, %v), want preserved", deleted, errDelete)
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil || string(current) != string(newRaw) {
		t.Fatalf("newer credential changed: %q, %v", current, errRead)
	}
	newVersion, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion(new) error = %v", errVersion)
	}
	deleted, errDelete = store.DeleteIfVersion(context.Background(), auth.ID, newVersion)
	if errDelete != nil || !deleted {
		t.Fatalf("DeleteIfVersion(new) = (%t, %v), want deleted", deleted, errDelete)
	}
	if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("matched credential still exists: %v", errStat)
	}
}

func TestFileTokenStoreListUsesRelativeIDWithRelativeBaseDir(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if errMkdir := os.Mkdir("auth", 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(
		filepath.Join("auth", "shared.json"),
		[]byte(`{"type":"codex","refresh_token":"refresh"}`),
		0o600,
	); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir("auth")
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(auths) != 1 || auths[0] == nil || auths[0].ID != "shared.json" {
		t.Fatalf("FileTokenStore.List() = %#v", auths)
	}
}

func TestFileTokenStorePersistsDisabledCredentialWithoutExistingFile(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		FileName: "disabled.json",
		Disabled: true,
		Metadata: map[string]any{"type": "codex"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	if path == "" {
		t.Fatal("Save() returned an empty path for disabled credential")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read disabled credential: %v", errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
		t.Fatalf("decode disabled credential: %v", errDecode)
	}
	if disabled, _ := metadata["disabled"].(bool); !disabled {
		t.Fatalf("persisted disabled value = %#v, want true", metadata["disabled"])
	}
	listed, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if len(listed) != 1 || listed[0] == nil || !listed[0].Disabled || listed[0].Status != cliproxyauth.StatusDisabled {
		t.Fatalf("disabled credential round trip = %#v", listed)
	}
}

func TestFileTokenStoreSaveWithBaseDirUsesScopedDirectory(t *testing.T) {
	defaultDir := t.TempDir()
	scopedDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(defaultDir)

	scopedRecord := &cliproxyauth.Auth{
		ID:       "scoped.json",
		FileName: "scoped.json",
		Metadata: map[string]any{"type": "codex"},
	}
	scopedPath, err := store.SaveWithBaseDir(context.Background(), scopedDir, scopedRecord)
	if err != nil {
		t.Fatalf("SaveWithBaseDir failed: %v", err)
	}
	if want := filepath.Join(scopedDir, "scoped.json"); scopedPath != want {
		t.Fatalf("saved path = %q, want %q", scopedPath, want)
	}
	if _, err = os.Stat(scopedPath); err != nil {
		t.Fatalf("scoped auth file missing: %v", err)
	}

	defaultRecord := &cliproxyauth.Auth{
		ID:       "default.json",
		FileName: "default.json",
		Metadata: map[string]any{"type": "claude"},
	}
	defaultPath, err := store.Save(context.Background(), defaultRecord)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if want := filepath.Join(defaultDir, "default.json"); defaultPath != want {
		t.Fatalf("default saved path = %q, want %q", defaultPath, want)
	}
	if _, err = os.Stat(filepath.Join(defaultDir, "scoped.json")); !os.IsNotExist(err) {
		t.Fatalf("scoped record unexpectedly written to default directory: %v", err)
	}
}

type failingFileTokenStorage struct{}

func (*failingFileTokenStorage) SaveTokenToFile(path string) error {
	if errWrite := os.WriteFile(path, []byte(`{"access_token":"partial"}`), 0o600); errWrite != nil {
		return errWrite
	}
	return errors.New("provider save failed")
}

func TestFileTokenStoreStorageFailurePreservesExistingCredential(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	record := &cliproxyauth.Auth{
		ID:       "existing.json",
		FileName: "existing.json",
		Metadata: map[string]any{"type": "codex", "access_token": "old"},
	}
	path, errSave := store.Save(context.Background(), record)
	if errSave != nil {
		t.Fatal(errSave)
	}
	original, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	record.Storage = &failingFileTokenStorage{}
	if _, errSave = store.Save(context.Background(), record); errSave == nil {
		t.Fatal("Save() succeeded after provider candidate write failed")
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(current) != string(original) {
		t.Fatalf("credential changed after failed provider save: %q", current)
	}
}

type cancellingFileTokenStorage struct {
	cancel context.CancelFunc
	saved  bool
}

func (s *cancellingFileTokenStorage) SetMetadata(map[string]any) {
	s.cancel()
}

func (s *cancellingFileTokenStorage) SaveTokenToFile(path string) error {
	s.saved = true
	return os.WriteFile(path, []byte(`{"access_token":"unexpected"}`), 0o600)
}

func TestFileTokenStoreSaveRejectsCancelledContext(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record := &cliproxyauth.Auth{
		ID:       "cancelled.json",
		FileName: "cancelled.json",
		Metadata: map[string]any{"type": "qwen"},
	}

	path, err := store.Save(ctx, record)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v, want context.Canceled", err)
	}
	if path != "" {
		t.Fatalf("Save path = %q, want empty", path)
	}
	if _, err = os.Stat(filepath.Join(baseDir, record.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled auth file stat error = %v, want os.ErrNotExist", err)
	}
}

func TestFileTokenStoreSaveStopsWhenMetadataSetterCancelsContext(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	ctx, cancel := context.WithCancel(context.Background())
	storage := &cancellingFileTokenStorage{cancel: cancel}
	record := &cliproxyauth.Auth{
		ID:       "cancelled-during-metadata.json",
		FileName: "cancelled-during-metadata.json",
		Metadata: map[string]any{"type": "xai"},
		Storage:  storage,
	}

	path, err := store.Save(ctx, record)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v, want context.Canceled", err)
	}
	if path != "" {
		t.Fatalf("Save path = %q, want empty", path)
	}
	if storage.saved {
		t.Fatal("SaveTokenToFile was called after context cancellation")
	}
	if _, err = os.Stat(filepath.Join(baseDir, record.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled auth file stat error = %v, want os.ErrNotExist", err)
	}
}

func TestExtractAccessToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		expected string
	}{
		{
			"antigravity top-level access_token",
			map[string]any{"access_token": "tok-abc"},
			"tok-abc",
		},
		{
			"gemini nested token.access_token",
			map[string]any{
				"token": map[string]any{"access_token": "tok-nested"},
			},
			"tok-nested",
		},
		{
			"top-level takes precedence over nested",
			map[string]any{
				"access_token": "tok-top",
				"token":        map[string]any{"access_token": "tok-nested"},
			},
			"tok-top",
		},
		{
			"empty metadata",
			map[string]any{},
			"",
		},
		{
			"whitespace-only access_token",
			map[string]any{"access_token": "   "},
			"",
		},
		{
			"wrong type access_token",
			map[string]any{"access_token": 12345},
			"",
		},
		{
			"token is not a map",
			map[string]any{"token": "not-a-map"},
			"",
		},
		{
			"nested whitespace-only",
			map[string]any{
				"token": map[string]any{"access_token": "  "},
			},
			"",
		},
		{
			"fallback to nested when top-level empty",
			map[string]any{
				"access_token": "",
				"token":        map[string]any{"access_token": "tok-fallback"},
			},
			"tok-fallback",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				got := extractAccessToken(tt.metadata)
				if got != tt.expected {
					t.Errorf("extractAccessToken() = %q, want %q", got, tt.expected)
				}
			},
		)
	}
}
