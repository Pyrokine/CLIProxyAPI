package auth

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type sharedMetadataStorage struct{}

func (*sharedMetadataStorage) SetMetadata(map[string]any) {}
func (*sharedMetadataStorage) SaveTokenToFile(string) error { return nil }

func TestFileTokenStoreRejectsUncopyableMetadataStorage(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	record := &coreauth.Auth{
		ID: "uncopyable.json", Metadata: map[string]any{"access_token": "fixture-token"},
		Storage: &sharedMetadataStorage{},
	}
	if path, err := store.Save(context.Background(), record); err == nil || path != "" {
		t.Fatalf("Save() = (%q, %v), want a storage snapshot error", path, err)
	}
	if _, errStat := os.Stat(filepath.Join(store.baseDirSnapshot(), record.ID)); !os.IsNotExist(errStat) {
		t.Fatalf("uncopyable storage created a credential file: %v", errStat)
	}
}

func TestResultPersistenceKeepsPublishedStorageOnSaveError(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	manager := coreauth.NewManager(nil, nil, nil)
	storage := &sharedMetadataStorage{}
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "uncopyable-result.json", Provider: "test", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "fixture-token"}, Storage: storage,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetStore(store)
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: registered.ID, AuthGeneration: registered.Generation,
		Provider: registered.Provider, Model: "fixture-model", Success: true,
	})
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil || current.Storage != storage {
		t.Fatalf("failed persistence replaced the published storage: %#v", current)
	}
	if _, changed := current.Metadata["disabled"]; changed {
		t.Fatalf("failed persistence published unsaved metadata: %v", current.Metadata)
	}
	if _, errStat := os.Stat(filepath.Join(store.baseDirSnapshot(), registered.ID)); !os.IsNotExist(errStat) {
		t.Fatalf("failed persistence wrote a credential file: %v", errStat)
	}
}

func TestResultPersistenceDoesNotMutatePublishedStorage(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	manager := coreauth.NewManager(nil, nil, nil)
	storage := &codexauth.TokenStorage{
		AccessToken: "fixture-token",
		Metadata:    map[string]any{"marker": "before"},
	}
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "storage-snapshot.json", Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "fixture-token"}, Storage: storage,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetStore(store)
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: registered.ID, AuthGeneration: registered.Generation,
		Provider: registered.Provider, Model: "fixture-model", Success: true,
	})
	if _, errStat := os.Stat(filepath.Join(store.baseDirSnapshot(), registered.ID)); errStat != nil {
		t.Fatalf("credential was not saved: %v", errStat)
	}
	if got := storage.Metadata["marker"]; got != "before" {
		t.Fatalf("published token storage metadata was changed during save: %v", storage.Metadata)
	}
	if _, changed := storage.Metadata["disabled"]; changed {
		t.Fatalf("save wrote result metadata into the published token storage: %v", storage.Metadata)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil {
		t.Fatal("saved credential is missing from the manager")
	}
	savedStorage, ok := current.Storage.(*codexauth.TokenStorage)
	if !ok || savedStorage == storage || savedStorage.Metadata["disabled"] != false {
		t.Fatalf("manager did not publish the saved storage snapshot: %#v", current.Storage)
	}
}

func TestExternalPersistenceMutationRefreshesVersionForRemoval(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "external-mutation.json", Provider: "fixture", Status: coreauth.StatusActive,
		Metadata: map[string]any{"type": "fixture", "access_token": "before"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, registered.ID)
	raw := []byte(`{"type":"fixture","access_token":"after"}`)
	digest, err := coreauth.DigestPersistedJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := manager.MutatePersistentAuth(context.Background(), registered.ID,
		func(context.Context) (*coreauth.Auth, error) {
			if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
				return nil, errWrite
			}
			next := registered.Clone()
			next.Metadata = map[string]any{"type": "fixture", "access_token": "after"}
			next.Attributes = map[string]string{
				"path": path, coreauth.PersistedSHA256Attribute: hex.EncodeToString(digest[:]),
			}
			return next, nil
		})
	if err != nil || updated == nil {
		t.Fatalf("MutatePersistentAuth() = (%#v, %v)", updated, err)
	}
	removed, err := manager.RemoveIfGeneration(context.Background(), updated.ID, updated.Generation)
	if err != nil || !removed {
		t.Fatalf("RemoveIfGeneration() = (%t, %v)", removed, err)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("removed credential is still on disk: %v", errStat)
	}
}

func TestResultPersistenceKeepsSavedFieldsInRuntime(t *testing.T) {
	for _, test := range []struct {
		name        string
		success     bool
		disposition cliproxyexecutor.ExecutionDisposition
	}{
		{name: "success", success: true},
		{
			name: "request fault", disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseMaybeSent,
				EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(nil, nil, nil)
			registered, err := manager.Register(context.Background(), &coreauth.Auth{
				ID: "result-persist.json", Provider: "codex", Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "fixture-token"},
			})
			if err != nil {
				t.Fatal(err)
			}
			manager.SetStore(store)
			manager.MarkResult(context.Background(), coreauth.Result{
				AuthID: registered.ID, AuthGeneration: registered.Generation,
				Provider: registered.Provider, Model: "fixture-model",
				Success: test.success, Disposition: test.disposition,
			})
			path := filepath.Join(dir, registered.ID)
			if _, errStat := os.Stat(path); errStat != nil {
				t.Fatalf("result was not persisted: %v", errStat)
			}
			current, ok := manager.GetByID(registered.ID)
			if !ok || current == nil {
				t.Fatal("credential disappeared")
			}
			if current.Attributes[coreauth.AttributePath] != path || current.Metadata["disabled"] != false {
				t.Fatalf("saved metadata missing in runtime: path=%q disabled=%v", current.Attributes[coreauth.AttributePath], current.Metadata["disabled"])
			}
		})
	}
}
