package auth

import (
	"context"
	"testing"
	"time"
)

func schedulerAuthVersionForTest(t *testing.T, scheduler *authScheduler, authID string) scheduledAuthVersion {
	t.Helper()
	if scheduler == nil {
		t.Fatal("scheduler is nil")
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	version, exists := scheduler.authVersions[authID]
	if !exists {
		t.Fatalf("scheduler version for %q is missing", authID)
	}
	return version
}

func TestManagerSyncSchedulerKeepsAuthSnapshotCurrent(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const authID = "scheduler-rebuild-current-auth"
	registered, err := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID: authID, Provider: "gemini",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	manager.scheduler.mu.Lock()
	released := false
	defer func() {
		if !released {
			manager.scheduler.mu.Unlock()
		}
	}()
	syncDone := make(chan struct{})
	go func() {
		manager.syncScheduler()
		close(syncDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if !manager.mu.TryLock() {
			break
		}
		manager.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("scheduler rebuild did not hold the manager read lock")
		}
		time.Sleep(time.Millisecond)
	}

	resultDone := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID: authID, AuthGeneration: registered.Generation, Success: true,
		})
		close(resultDone)
	}()
	manager.scheduler.mu.Unlock()
	released = true
	select {
	case <-syncDone:
	case <-time.After(time.Second):
		t.Fatal("scheduler rebuild did not finish")
	}
	select {
	case <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("result publication did not finish")
	}
	current, ok := manager.GetByID(authID)
	if !ok || current.Success != 1 {
		t.Fatalf("current auth = %#v, want successful result", current)
	}
	got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID)
	if got == nil || got.Generation != current.Generation || got.Success != 1 {
		t.Fatalf("scheduled auth = %#v, current auth = %#v", got, current)
	}
}

func TestManagerRemovePublishesSchedulerTombstoneGeneration(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	authID := "manager-removal-publication-auth"
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "gemini",
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	delayed := registered.Clone()
	delayed.UpdatedAt = time.Now().Add(24 * time.Hour)

	removeCtx := WithRuntimeOnlyRemove(WithSkipPersist(context.Background()))
	if errRemove := manager.Remove(removeCtx, authID); errRemove != nil {
		t.Fatalf("Remove() error = %v", errRemove)
	}
	tombstone := schedulerAuthVersionForTest(t, manager.scheduler, authID)
	if !tombstone.tombstone || tombstone.generation <= registered.Generation {
		t.Fatalf("scheduler tombstone = %#v, want generation above %d", tombstone, registered.Generation)
	}

	manager.scheduler.upsertAuth(delayed)
	if got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID); got != nil {
		t.Fatalf("delayed removed snapshot was scheduled: %#v", got)
	}

	recreated, errRecreate := manager.Register(
		WithAllowAuthRecreate(context.Background()),
		&Auth{ID: authID, Provider: "gemini"},
	)
	if errRecreate != nil {
		t.Fatalf("Register(recreate) error = %v", errRecreate)
	}
	if recreated == nil || recreated.Generation <= tombstone.generation {
		t.Fatalf("recreated auth = %#v, want generation above %d", recreated, tombstone.generation)
	}
	if got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID); got == nil || got.Generation != recreated.Generation {
		t.Fatalf("recreated auth was not scheduled: %#v", got)
	}
}

func TestManagerRemoveIfGenerationPublishesSchedulerTombstone(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	authID := "manager-conditional-removal-publication-auth"
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "gemini",
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	delayed := registered.Clone()
	delayed.UpdatedAt = time.Now().Add(24 * time.Hour)

	removeCtx := WithRuntimeOnlyRemove(WithSkipPersist(context.Background()))
	removed, errRemove := manager.RemoveIfGeneration(removeCtx, authID, registered.Generation)
	if errRemove != nil {
		t.Fatalf("RemoveIfGeneration() error = %v", errRemove)
	}
	if !removed {
		t.Fatal("RemoveIfGeneration() did not remove the current generation")
	}
	tombstone := schedulerAuthVersionForTest(t, manager.scheduler, authID)
	if !tombstone.tombstone || tombstone.generation <= registered.Generation {
		t.Fatalf("scheduler tombstone = %#v, want generation above %d", tombstone, registered.Generation)
	}

	manager.scheduler.upsertAuth(delayed)
	if got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID); got != nil {
		t.Fatalf("delayed conditional-removal snapshot was scheduled: %#v", got)
	}
}

func TestManagerLoadPublishesSchedulerTombstonesForMissingAuths(t *testing.T) {
	store := &countingStore{}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	authID := "manager-load-removal-publication-auth"
	registered, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       authID,
		Provider: "gemini",
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	delayed := registered.Clone()
	delayed.UpdatedAt = time.Now().Add(24 * time.Hour)

	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load(empty) error = %v", errLoad)
	}
	tombstone := schedulerAuthVersionForTest(t, manager.scheduler, authID)
	if !tombstone.tombstone || tombstone.generation <= registered.Generation {
		t.Fatalf("scheduler tombstone = %#v, want generation above %d", tombstone, registered.Generation)
	}

	manager.scheduler.upsertAuth(delayed)
	if got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID); got != nil {
		t.Fatalf("snapshot omitted by Load() was scheduled: %#v", got)
	}

	store.list = []*Auth{{ID: authID, Provider: "gemini"}}
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load(recreated) error = %v", errLoad)
	}
	got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID)
	if got == nil || got.Generation <= tombstone.generation {
		t.Fatalf("reloaded auth = %#v, want generation above %d", got, tombstone.generation)
	}
}

func TestManagerRemovePublishesSchedulerTombstoneBeforeInFlightMutationCompletes(t *testing.T) {
	store := &blockingAuthStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	})
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	authID := "manager-removal-in-flight-publication-auth"
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "gemini",
		Metadata: map[string]any{"token": "test"},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	store.mu.Lock()
	store.block = true
	store.mu.Unlock()
	markDone := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID:         authID,
			AuthGeneration: registered.Generation,
			Success:        true,
		})
		close(markDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("MarkResult() did not enter persistence")
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- manager.Remove(context.Background(), authID)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		manager.scheduler.mu.Lock()
		version := manager.scheduler.authVersions[authID]
		manager.scheduler.mu.Unlock()
		if version.tombstone {
			if version.generation <= registered.Generation {
				t.Fatalf("scheduler tombstone = %#v, want generation above %d", version, registered.Generation)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Remove() did not publish a scheduler tombstone before the in-flight mutation completed")
		}
		time.Sleep(time.Millisecond)
	}
	if got := schedulerAuthSnapshotForTest(manager.scheduler, "gemini", authID); got != nil {
		t.Fatalf("scheduler retained the removed auth while MarkResult() was in flight: %#v", got)
	}

	close(store.release)
	select {
	case <-markDone:
	case <-time.After(time.Second):
		t.Fatal("MarkResult() did not finish")
	}
	select {
	case errRemove := <-removeDone:
		if errRemove != nil {
			t.Fatalf("Remove() error = %v", errRemove)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove() did not finish")
	}
}
