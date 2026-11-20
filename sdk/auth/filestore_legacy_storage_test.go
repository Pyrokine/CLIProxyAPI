package auth

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// The v6 contract permits storage whose private state is owned by closures.
type legacyClosureTokenStorage struct {
	setMetadata func(map[string]any)
	save        func(string) error
}

func (s legacyClosureTokenStorage) SetMetadata(metadata map[string]any) {
	s.setMetadata(metadata)
}

func (s legacyClosureTokenStorage) SaveTokenToFile(path string) error {
	return s.save(path)
}

func newLegacyClosureTokenStorage(saveError error) legacyClosureTokenStorage {
	var mu sync.Mutex
	metadata := map[string]any{"type": "fixture", "marker": "published"}
	return legacyClosureTokenStorage{
		setMetadata: func(next map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			metadata = next
		},
		save: func(path string) error {
			mu.Lock()
			defer mu.Unlock()
			// These provider fields are absent from Auth.Metadata.
			payload := make(map[string]any, len(metadata)+2)
			maps.Copy(payload, metadata)
			payload["access_token"] = "synthetic-private-token"
			payload["provider_projection"] = metadata["note"]
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			if err = os.WriteFile(path, raw, 0o600); err != nil {
				return err
			}
			return saveError
		},
	}
}

func readLegacyCredential(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := coreauth.DecodeCredentialMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestLegacyTokenStorageOriginalPublicContract(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		storage := newLegacyClosureTokenStorage(nil)
		storage.SetMetadata(map[string]any{"type": "fixture", "disabled": disabled, "note": "operator"})
		path := filepath.Join(t.TempDir(), "direct.json")
		if err := storage.SaveTokenToFile(path); err != nil {
			t.Fatal(err)
		}
		got := readLegacyCredential(t, path)
		if got["disabled"] != disabled || got["note"] != "operator" ||
			got["access_token"] != "synthetic-private-token" ||
			got["provider_projection"] != "operator" {
			t.Fatalf("legacy public methods lost provider or manager metadata: %#v", got)
		}
	}
}

func TestFileTokenStoreLegacyStorageCancelledContext(t *testing.T) {
	store := NewFileTokenStore()
	dir := t.TempDir()
	store.SetBaseDir(dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := newLegacyClosureTokenStorage(nil)
	record := &coreauth.Auth{
		ID: "legacy.json", Provider: "fixture", Storage: storage,
		Metadata: map[string]any{"type": "fixture"},
	}
	path, err := store.Save(ctx, record)
	if !errors.Is(err, context.Canceled) || path != "" {
		t.Fatalf("Save() = (%q, %v), want context.Canceled", path, err)
	}
	if _, err = os.Stat(filepath.Join(dir, record.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled save created credential: %v", err)
	}
}

func TestLegacyTokenStorageValueCopiesShareOpaqueState(t *testing.T) {
	storage := newLegacyClosureTokenStorage(nil)
	copyStorage := storage
	copyStorage.SetMetadata(map[string]any{"type": "fixture", "marker": "copy"})
	path := filepath.Join(t.TempDir(), "original.json")
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	if got := readLegacyCredential(t, path)["marker"]; got != "copy" {
		t.Fatalf("closure fixture did not retain shared opaque state: %v", got)
	}
	if _, ok := any(storage).(coreauth.PersistenceTokenStorage); ok {
		t.Fatal("legacy fixture unexpectedly implements the optional clone contract")
	}
	if reflect.TypeOf(storage).NumMethod() != 2 {
		t.Fatal("legacy fixture must expose only the two original methods")
	}
}

type cloneFailingTokenStorage struct {
	metadata map[string]any
	err      error
}

func (s *cloneFailingTokenStorage) CloneForPersistence() coreauth.TokenStorage {
	copyStorage := *s
	return &copyStorage
}

func (s *cloneFailingTokenStorage) SetMetadata(metadata map[string]any) {
	s.metadata = metadata
}

func (s *cloneFailingTokenStorage) SaveTokenToFile(path string) error {
	if err := os.WriteFile(path, []byte(`{"type":"fixture","access_token":"partial"}`), 0o600); err != nil {
		return err
	}
	return s.err
}

func TestManagerCloneStorageFailurePreservesPublishedCredential(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	before := &coreauth.Auth{
		ID: "clone-failure.json", Provider: "fixture", Status: coreauth.StatusActive,
		Metadata: map[string]any{"type": "fixture", "access_token": "old-token", "note": "published"},
	}
	path, err := store.Save(context.Background(), before.Clone())
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	providerError := errors.New("synthetic provider save failure")
	storage := &cloneFailingTokenStorage{metadata: map[string]any{"marker": "published"}, err: providerError}
	before.Storage = storage
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetStore(store)
	next := registered.Clone()
	next.Disabled = true
	next.Metadata["note"] = "attempted"
	if _, err = manager.Update(context.Background(), next); !errors.Is(err, providerError) {
		t.Fatalf("Update() error = %v, want original provider error", err)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Generation != registered.Generation || current.Disabled ||
		current.Metadata["note"] != "published" ||
		current.Storage != storage {
		t.Fatalf("failed save changed published credential: %#v", current)
	}
	if _, changed := current.Metadata["disabled"]; changed {
		t.Fatal("failed save published unsaved disabled metadata")
	}
	if !reflect.DeepEqual(storage.metadata, map[string]any{"marker": "published"}) {
		t.Fatalf("failed save mutated original token storage: %#v", storage.metadata)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatalf("failed save changed original file: %q, %v", after, err)
	}
}

func TestManagerLegacyStorageStaleResultKeepsNewGeneration(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	storage := newLegacyClosureTokenStorage(nil)
	before, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "legacy-generation.json", Provider: "fixture", Storage: storage,
			Metadata: map[string]any{"type": "fixture", "access_token": "old-token"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	replacement := before.Clone()
	replacement.Metadata["access_token"] = "new-token"
	after, err := manager.Register(context.Background(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation == before.Generation {
		t.Fatal("replacement did not advance credential generation")
	}
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	manager.SetStore(store)
	manager.MarkResult(
		context.Background(), coreauth.Result{
			AuthID: before.ID, AuthGeneration: before.Generation,
			Provider: before.Provider, Model: "fixture-model", Success: true,
		},
	)
	current, ok := manager.GetByID(after.ID)
	if !ok || current.Generation != after.Generation || current.Metadata["access_token"] != "new-token" {
		t.Fatalf("stale result changed replacement credential: %#v", current)
	}
	if _, err = os.Stat(filepath.Join(dir, after.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale result attempted persistence: %v", err)
	}
}
