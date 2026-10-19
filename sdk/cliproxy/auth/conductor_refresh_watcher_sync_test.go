package auth

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestOldWatcherEventCannotUndoUnpersistedRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh.json")
	oldRaw := []byte(`{"access_token":"old","refresh_token":"owner"}`)
	if err := os.WriteFile(path, oldRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	oldVersion, err := ReadPersistedVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "refresh.json", Provider: "fixture", Status: StatusActive,
		Attributes: map[string]string{
			"path": path, PersistedSHA256Attribute: hex.EncodeToString(oldVersion.SHA256[:]),
		},
		Metadata: map[string]any{"access_token": "old", "refresh_token": "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rotated := registered.Clone()
	rotated.Metadata["access_token"] = "new"
	current, err := manager.Update(WithSkipPersist(context.Background()), rotated)
	if err != nil || current == nil {
		t.Fatalf("Update() = (%#v, %v)", current, err)
	}
	intentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	intent := &authRefreshPersistenceIntent{
		auth: current.Clone(), expectedVersion: &oldVersion,
		nextVersion: refreshPersistenceNextVersion(&oldVersion, current),
		generation: current.Generation, ctx: intentCtx, cancel: cancel,
	}
	manager.pendingRefreshPersistenceRetries.Store(current.ID, intent)
	defer manager.cancelPendingRefreshPersistence(current.ID)

	watcherCtx := WithAllowAuthRecreate(WithSkipPersist(context.Background()))
	_, err = manager.Update(watcherCtx, registered.Clone())
	if err == nil {
		t.Error("watcher accepted an old file over an unpersisted refresh")
	}
	current, _ = manager.GetByID(registered.ID)
	if authAccessToken(current) != "new" {
		t.Fatalf("watcher restored the stale access token: %#v", current)
	}
	if pending, ok := manager.pendingRefreshPersistenceRetries.Load(current.ID); !ok || pending != intent || intentCtx.Err() != nil {
		t.Fatal("watcher canceled the pending refreshed credential persistence")
	}
	manager.SetStore(&countingStore{list: []*Auth{registered.Clone()}})
	if errLoad := manager.Load(context.Background()); errLoad == nil {
		t.Error("Load replaced an unpersisted refresh with an older file snapshot")
	}
	if current, _ = manager.GetByID(registered.ID); authAccessToken(current) != "new" {
		t.Fatalf("Load restored the stale access token: %#v", current)
	}
}
