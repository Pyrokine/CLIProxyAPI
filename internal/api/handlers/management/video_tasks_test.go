package management

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

func TestVideoTaskOwnershipPersistsAndCannotBeReassigned(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	store, err := openManagementStateStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{stateStore: store}
	ctx := context.Background()
	task := handlers.VideoTask{
		ID: "video.id:1", Provider: "xai", OwnerIdentity: "creator-hash", AuthID: "upstream-auth",
		Model: "grok-imagine-video",
	}
	if err := h.SaveVideoTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := h.SaveVideoTask(ctx, task); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	other := task
	other.OwnerIdentity = "another-key-hash"
	if err := h.SaveVideoTask(ctx, other); err == nil {
		t.Fatal("ownership was reassigned")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openManagementStateStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h = &Handler{stateStore: store}
	got, found, err := h.LoadVideoTask(ctx, task.Provider, task.ID)
	if err != nil || !found || got != task {
		t.Fatalf("reopened task=%+v found=%v err=%v", got, found, err)
	}
	if _, found, err := h.LoadVideoTask(ctx, "another-provider", task.ID); err != nil || found {
		t.Fatalf("provider isolation: found=%v err=%v", found, err)
	}
	if _, found, err := h.LoadVideoTask(ctx, task.Provider, "legacy-task"); err != nil || found {
		t.Fatalf("unknown task: found=%v err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.SaveVideoTask(ctx, task); err == nil {
		t.Fatal("closed storage accepted a task")
	}
}

func TestVideoTaskConcurrentCreatorsPreserveOneOwner(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	store, err := openManagementStateStore(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h := &Handler{stateStore: store}
	ctx := context.Background()
	var saved atomic.Int32
	var wg sync.WaitGroup
	for _, owner := range []string{"creator-a", "creator-b"} {
		wg.Go(
			func() {
				task := handlers.VideoTask{
					ID: "same-id", Provider: "xai", OwnerIdentity: owner, AuthID: "upstream-auth",
					Model: "grok-imagine-video",
				}
				if h.SaveVideoTask(ctx, task) == nil {
					saved.Add(1)
				}
			},
		)
	}
	wg.Wait()
	if got := saved.Load(); got != 1 {
		t.Fatalf("successful creators=%d, want 1", got)
	}
	task, found, err := h.LoadVideoTask(ctx, "xai", "same-id")
	if err != nil || !found || (task.OwnerIdentity != "creator-a" && task.OwnerIdentity != "creator-b") {
		t.Fatalf("task=%+v found=%v err=%v", task, found, err)
	}
}
