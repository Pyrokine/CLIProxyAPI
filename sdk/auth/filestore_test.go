package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
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

func TestFileTokenStoreRequiresCreationIntentForMissingDisabledCredential(t *testing.T) {
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
	if errSave != nil || path != "" {
		t.Fatalf("runtime Save() path=%q error=%v, want skipped", path, errSave)
	}
	if _, errStat := os.Stat(filepath.Join(dir, auth.FileName)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("runtime Save() recreated disabled credential: %v", errStat)
	}

	path, errSave = store.Save(cliproxyauth.WithAuthCreationIntent(context.Background()), auth)
	if errSave != nil {
		t.Fatalf("creation Save() error = %v", errSave)
	}
	if path == "" {
		t.Fatal("creation Save() returned an empty path for disabled credential")
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

func TestFileTokenStoreSaveExistingMetadataRefreshesAuthState(t *testing.T) {
	tests := []struct {
		name          string
		existingToken string
		savedToken    string
	}{
		{name: "unchanged content", existingToken: "token", savedToken: "token"},
		{name: "overwritten content", existingToken: "old-token", savedToken: "new-token"},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				baseDir := t.TempDir()
				fileName := "antigravity-user.json"
				path := filepath.Join(baseDir, fileName)
				existing := []byte(`{"type":"antigravity","access_token":"` + testCase.existingToken + `","disabled":false}`)
				if errWrite := os.WriteFile(path, existing, 0o644); errWrite != nil {
					t.Fatalf("write existing auth file: %v", errWrite)
				}

				store := NewFileTokenStore()
				store.SetBaseDir(baseDir)
				auth := &cliproxyauth.Auth{
					ID: fileName,
					Metadata: map[string]any{
						"type":         "antigravity",
						"access_token": testCase.savedToken,
					},
				}

				savedPath, errSave := store.Save(context.Background(), auth)
				if errSave != nil {
					t.Fatalf("Save() error = %v", errSave)
				}
				if savedPath != path {
					t.Fatalf("Save() path = %q, want %q", savedPath, path)
				}
				if auth.FileName != fileName {
					t.Errorf("FileName = %q, want %q", auth.FileName, fileName)
				}
				for attribute, want := range map[string]string{
					cliproxyauth.AttributePath:          path,
					cliproxyauth.AttributeSource:        path,
					cliproxyauth.AttributeSourceBackend: cliproxyauth.AuthSourceFile,
				} {
					if got := auth.Attributes[attribute]; got != want {
						t.Errorf("attribute %s = %q, want %q", attribute, got, want)
					}
				}
				info, errStat := os.Stat(path)
				if errStat != nil {
					t.Fatalf("stat saved auth file: %v", errStat)
				}
				if runtime.GOOS != "windows" {
					if got := info.Mode().Perm(); got != 0o600 {
						t.Errorf("saved auth mode = %04o, want 0600", got)
					}
				}
				persisted, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatalf("read saved auth file: %v", errRead)
				}
				expected := []byte(`{"type":"antigravity","access_token":"` + testCase.savedToken + `","disabled":false}`)
				if !util.JSONEqual(persisted, expected) {
					t.Errorf("saved auth file = %s, want JSON equal to %s", persisted, expected)
				}
			},
		)
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

func (s *cancellingFileTokenStorage) SetMetadata(_ map[string]any) {
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

type fileStoreMultiAuthParserFunc func(
	context.Context,
	pluginapi.AuthParseRequest,
) ([]*cliproxyauth.Auth, bool, error)

func (f fileStoreMultiAuthParserFunc) ParseAuth(
	context.Context,
	pluginapi.AuthParseRequest,
) (*cliproxyauth.Auth, bool, error) {
	return nil, false, nil
}

func (f fileStoreMultiAuthParserFunc) ParseAuths(
	ctx context.Context,
	req pluginapi.AuthParseRequest,
) ([]*cliproxyauth.Auth, bool, error) {
	return f(ctx, req)
}

func TestParsePluginAuthFilesRejectsInvalidSourceWeightBeforeParser(t *testing.T) {
	called := false
	parser := fileStoreMultiAuthParserFunc(func(
		context.Context,
		pluginapi.AuthParseRequest,
	) ([]*cliproxyauth.Auth, bool, error) {
		called = true
		return []*cliproxyauth.Auth{{ID: "plugin-a", Provider: "plugin"}}, true, nil
	})
	ctx := WithPluginAuthParser(context.Background(), parser)

	_, handled, errParse := ParsePluginAuthFiles(
		ctx,
		"plugin",
		"plugin.json",
		"plugin.json",
		[]byte(`{"type":"plugin","weight":1.5}`),
	)
	if errParse == nil || !handled {
		t.Fatalf("ParsePluginAuthFiles() handled=%t err=%v, want handled weight error", handled, errParse)
	}
	if called {
		t.Fatal("plugin parser called for invalid source weight")
	}
}

func TestParsePluginAuthFilesFiltersInvalidChildWeight(t *testing.T) {
	parser := fileStoreMultiAuthParserFunc(func(
		context.Context,
		pluginapi.AuthParseRequest,
	) ([]*cliproxyauth.Auth, bool, error) {
		return []*cliproxyauth.Auth{
			{ID: "invalid", Provider: "plugin", Metadata: map[string]any{"weight": 1.5}},
			{ID: "valid", Provider: "plugin"},
		}, true, nil
	})
	ctx := WithPluginAuthParser(context.Background(), parser)

	auths, handled, errParse := ParsePluginAuthFiles(
		ctx,
		"plugin",
		"plugin.json",
		"plugin.json",
		[]byte(`{"type":"plugin","weight":4}`),
	)
	if errParse != nil || !handled || len(auths) != 1 {
		t.Fatalf("ParsePluginAuthFiles() handled=%t len=%d err=%v", handled, len(auths), errParse)
	}
	if auths[0].ID != "valid" || auths[0].Attributes[cliproxyauth.AttributeWeight] != "4" {
		t.Fatalf("filtered auth = %#v, want valid child with source weight", auths[0])
	}
}

func TestFileTokenStoreListExpandsPluginMultiAuths(t *testing.T) {
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "plugin.json")
	raw := []byte(`{"type":"plugin","disabled":true,"weight":3,"proxy-url":"http://proxy.test","headers":{"X-Test":"value"}}`)
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	parser := fileStoreMultiAuthParserFunc(func(
		_ context.Context,
		req pluginapi.AuthParseRequest,
	) ([]*cliproxyauth.Auth, bool, error) {
		if req.Provider != "plugin" || req.Path != path || req.FileName != "plugin.json" {
			t.Fatalf("ParseAuths request = %#v", req)
		}
		return []*cliproxyauth.Auth{
			{
				ID:       "plugin-a",
				Provider: "plugin",
				FileName: req.FileName,
				Metadata: map[string]any{
					"type": "plugin", "account": "a", "request-retry": 2,
					"headers": map[string]any{"X-Test": "value"},
				},
			},
			nil,
			{
				ID:       "plugin-b",
				Provider: "plugin",
				FileName: req.FileName,
				Metadata: map[string]any{"type": "plugin", "account": "b", "headers": map[string]any{"X-Test": "value"}},
			},
		}, true, nil
	})
	ctx := WithPluginAuthParser(context.Background(), parser)
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	first, errList := store.List(ctx)
	if errList != nil {
		t.Fatal(errList)
	}
	second, errList := store.List(ctx)
	if errList != nil {
		t.Fatal(errList)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("List() lengths = %d/%d, want 2/2", len(first), len(second))
	}
	for index := range first {
		if !cliproxyauth.IsPluginVirtualAuth(first[index]) {
			t.Fatalf("auth %s missing plugin virtual marker", first[index].ID)
		}
		if first[index].Attributes[cliproxyauth.AttributeVirtualSource] != path ||
			first[index].Attributes[cliproxyauth.AttributePath] != path ||
			first[index].Attributes[cliproxyauth.AttributeSource] != path {
			t.Fatalf("auth %s source attributes = %#v", first[index].ID, first[index].Attributes)
		}
		if !first[index].Disabled || first[index].Status != cliproxyauth.StatusDisabled {
			t.Fatalf("auth %s disabled/status = %t/%s", first[index].ID, first[index].Disabled, first[index].Status)
		}
		if first[index].Attributes[cliproxyauth.AttributeWeight] != "3" ||
			first[index].Attributes["header:X-Test"] != "value" ||
			first[index].ProxyURL != "http://proxy.test" {
			t.Fatalf(
				"auth %s inherited values: attributes=%#v proxy=%q",
				first[index].ID,
				first[index].Attributes,
				first[index].ProxyURL,
			)
		}
		if first[index].EnsureIndex() == "" || first[index].EnsureIndex() != second[index].EnsureIndex() {
			t.Fatalf(
				"auth %s indexes = %q/%q, want stable non-empty index",
				first[index].ID,
				first[index].EnsureIndex(),
				second[index].EnsureIndex(),
			)
		}
	}
	if first[0].EnsureIndex() == first[1].EnsureIndex() {
		t.Fatalf("plugin child indexes collide: %q", first[0].EnsureIndex())
	}
	if first[0].Metadata["request_retry"] != 2 {
		t.Fatalf("canonical child metadata = %#v", first[0].Metadata)
	}
	if _, legacy := first[0].Metadata["request-retry"]; legacy {
		t.Fatalf("legacy child metadata was retained: %#v", first[0].Metadata)
	}
}

func TestPluginVirtualAuthPersistenceDoesNotOverwriteSource(t *testing.T) {
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "plugin.json")
	original := []byte(`{"type":"plugin","accounts":["a","b"],"secret":"source-secret"}`)
	if errWrite := os.WriteFile(path, original, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	parser := fileStoreMultiAuthParserFunc(func(
		_ context.Context,
		req pluginapi.AuthParseRequest,
	) ([]*cliproxyauth.Auth, bool, error) {
		return []*cliproxyauth.Auth{
			{ID: "plugin-a", Provider: "plugin", FileName: req.FileName, Metadata: map[string]any{"type": "plugin", "account": "a"}},
			{ID: "plugin-b", Provider: "plugin", FileName: req.FileName, Metadata: map[string]any{"type": "plugin", "account": "b"}},
		}, true, nil
	})
	ctx := WithPluginAuthParser(context.Background(), parser)
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auths, errList := store.List(ctx)
	if errList != nil || len(auths) != 2 {
		t.Fatalf("List() len=%d err=%v", len(auths), errList)
	}

	auths[0].Metadata["secret"] = "child-overwrite"
	if savedPath, errSave := store.Save(context.Background(), auths[0]); errSave != nil || savedPath != path {
		t.Fatalf("Save(virtual) path=%q err=%v", savedPath, errSave)
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil || string(current) != string(original) {
		t.Fatalf("source changed after direct virtual Save: %s err=%v", current, errRead)
	}

	manager := cliproxyauth.NewManager(store, nil, nil)
	for _, auth := range auths {
		if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	updated := auths[1].Clone()
	updated.Metadata["secret"] = "manager-overwrite"
	if result, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil || result == nil {
		t.Fatalf("Manager.Update(virtual) result=%#v err=%v", result, errUpdate)
	}
	current, errRead = os.ReadFile(path)
	if errRead != nil || string(current) != string(original) {
		t.Fatalf("source changed after manager virtual update: %s err=%v", current, errRead)
	}
}
