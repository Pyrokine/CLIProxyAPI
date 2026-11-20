package auth

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestFileTokenStoreOwnedLegacyStoragePreservesProviderOutput(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
				dir := t.TempDir()
				store := NewFileTokenStore()
				record := &coreauth.Auth{
					ID: "legacy.json", Provider: "fixture", Disabled: disabled,
					Storage:  newLegacyClosureTokenStorage(nil),
					Metadata: map[string]any{"type": "fixture", "note": "operator"},
				}
				path, err := store.SaveWithBaseDir(coreauth.WithAuthCreationIntent(t.Context()), dir, record)
				if err != nil {
					t.Fatal(err)
				}
				got := readLegacyCredential(t, path)
				if got["disabled"] != disabled || got["note"] != "operator" ||
					got["access_token"] != "synthetic-private-token" || got["provider_projection"] != "operator" {
					t.Fatalf("owned legacy save lost Provider output: %#v", got)
				}
			},
		)
	}
}

func TestFileTokenStoreOwnedLegacyFailurePreservesOriginalFile(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	path := filepath.Join(dir, "legacy.json")
	original := []byte(`{"type":"fixture","access_token":"original"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	providerError := errors.New("synthetic legacy save failure")
	record := &coreauth.Auth{
		ID: "legacy.json", Provider: "fixture", Storage: newLegacyClosureTokenStorage(providerError),
		Metadata: map[string]any{"type": "fixture", "note": "operator"},
	}
	if saved, err := store.Save(t.Context(), record); saved != "" || !errors.Is(err, providerError) {
		t.Fatalf("owned failed save = (%q, %v), want original error", saved, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("failed owned save changed original file: %q, %v", got, err)
	}
}

type ownedProjectionStorage struct {
	metadata     map[string]any
	saveError    error
	mutateNested bool
	copyMode     string
}

func (s *ownedProjectionStorage) CloneForPersistence() coreauth.TokenStorage {
	switch s.copyMode {
	case "typed-nil":
		return (*ownedProjectionStorage)(nil)
	case "shared":
		return s
	}
	copyStorage := *s
	copyStorage.metadata = maps.Clone(s.metadata)
	return &copyStorage
}

func (s *ownedProjectionStorage) SetMetadata(metadata map[string]any) {
	s.metadata = metadata
	if s.mutateNested {
		metadata["nested"].(map[string]any)["marker"] = "setter-update"
	}
}

func (s *ownedProjectionStorage) SaveTokenToFile(path string) error {
	payload := maps.Clone(s.metadata)
	payload["access_token"] = "synthetic-private-token"
	payload["provider_projection"] = s.metadata["note"]
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	return s.saveError
}

func TestManagerProviderProjectionUsesActualSavedVersion(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	storage := &ownedProjectionStorage{metadata: map[string]any{"marker": "published"}}
	registered, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "projection.json", Provider: "fixture", Status: coreauth.StatusActive, Storage: storage,
			Metadata: map[string]any{"type": "fixture", "note": "operator"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, registered.ID)
	if got := readLegacyCredential(t, path); got["provider_projection"] != "operator" {
		t.Fatalf("Provider projection was not saved: %#v", got)
	}
	if storage.metadata["marker"] != "published" || len(storage.metadata) != 1 {
		t.Fatalf("Manager save changed published Storage: %#v", storage.metadata)
	}
	removed, err := manager.RemoveIfGeneration(t.Context(), registered.ID, registered.Generation)
	if err != nil || !removed {
		t.Fatalf("saved Provider content did not authorize versioned removal: %t, %v", removed, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("versioned removal left Provider file: %v", err)
	}
}

func TestManagerStorageSetterFailurePreservesNestedMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested.json")
	original := []byte(`{"type":"fixture","access_token":"original"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	providerError := errors.New("synthetic nested save failure")
	storage := &ownedProjectionStorage{
		metadata: map[string]any{"marker": "published"}, saveError: providerError, mutateNested: true,
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "nested.json", Provider: "fixture", Status: coreauth.StatusActive, Storage: storage,
			Metadata: map[string]any{
				"type": "fixture", "note": "published", "nested": map[string]any{"marker": "before"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	manager.SetStore(store)
	next := registered.Clone()
	next.Metadata["note"] = "attempted"
	if _, err := manager.Update(t.Context(), next); !errors.Is(err, providerError) {
		t.Fatalf("Update error = %v, want original Provider error", err)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["note"] != "published" ||
		current.Metadata["nested"].(map[string]any)["marker"] != "before" || current.Storage != storage {
		t.Fatalf("failed setter/save changed published state: %#v", current)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("failed setter/save changed original file: %q, %v", got, err)
	}
}

func TestFileTokenStoreRejectsInvalidPersistenceCopies(t *testing.T) {
	for _, mode := range []string{"typed-nil", "shared"} {
		t.Run(
			mode, func(t *testing.T) {
				store := NewFileTokenStore()
				store.SetBaseDir(t.TempDir())
				storage := &ownedProjectionStorage{metadata: map[string]any{"marker": "original"}, copyMode: mode}
				record := &coreauth.Auth{
					ID: "invalid.json", Storage: storage, Metadata: map[string]any{"note": "attempt"},
				}
				if _, err := store.Save(t.Context(), record); err == nil || !strings.Contains(
					err.Error(), "persistence copy",
				) {
					t.Fatalf("invalid copy save error = %v", err)
				}
				if storage.metadata["marker"] != "original" || len(storage.metadata) != 1 {
					t.Fatalf("invalid copy reached shared setter: %#v", storage.metadata)
				}
			},
		)
	}
}

type ownedSaverOnlyStorage struct {
	calls *int
}

func (s ownedSaverOnlyStorage) SaveTokenToFile(string) error {
	*s.calls += 1
	return nil
}

func TestManagerRejectsSharedSaverBeforeCallingIt(t *testing.T) {
	calls := 0
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "saver-only.json", Provider: "fixture", Storage: ownedSaverOnlyStorage{calls: &calls},
			Metadata: map[string]any{"type": "fixture"},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "CloneForPersistence") || calls != 0 {
		t.Fatalf("shared saver result = %v, calls=%d", err, calls)
	}
}

func TestOwnedProviderCandidateErrorPreservesPublicationScope(t *testing.T) {
	providerError := errors.New("synthetic nested publication failure")
	storage := legacyClosureTokenStorage{
		setMetadata: func(map[string]any) {},
		save: func(path string) error {
			return &internalauth.TokenFilePublicationError{Path: path, Err: providerError}
		},
	}
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	_, err := store.Save(
		context.Background(), &coreauth.Auth{
			ID: "candidate.json", Storage: storage, Metadata: map[string]any{"type": "fixture"},
		},
	)
	if !errors.Is(err, providerError) || errors.Is(err, coreauth.ErrAuthDurableStateUncertain) {
		t.Fatalf("candidate-only failure changed live publication scope: %v", err)
	}
}

func TestFileTokenStorePropagatesReportedLivePublicationUncertainty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uncertain.json")
	providerError := errors.New("synthetic directory sync failure")
	storage := legacyClosureTokenStorage{
		setMetadata: func(map[string]any) {},
		save: func(string) error {
			return &internalauth.TokenFilePublicationError{Path: path, Err: providerError}
		},
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	_, err := store.Save(
		t.Context(), &coreauth.Auth{
			ID: "uncertain.json", Storage: storage, Metadata: map[string]any{"type": "fixture"},
		},
	)
	if !errors.Is(err, providerError) || !errors.Is(err, coreauth.ErrAuthDurableStateUncertain) {
		t.Fatalf("reported live publication uncertainty lost its original cause: %v", err)
	}
}
