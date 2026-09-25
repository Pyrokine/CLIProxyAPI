package auth

import (
	"sync"
	"testing"
	"time"
)

func schedulerAuthSnapshotForTest(s *authScheduler, provider, authID string) *Auth {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	providerState := s.providers[provider]
	if providerState == nil {
		return nil
	}
	meta := providerState.auths[authID]
	if meta == nil || meta.auth == nil {
		return nil
	}
	return meta.auth.Clone()
}

func TestSchedulerRemovalTombstoneRejectsDelayedSnapshots(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	authID := "scheduler-tombstone-auth"
	registeredAt := time.Unix(100, 0)
	removedAt := time.Unix(200, 0)

	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 7,
		UpdatedAt:  registeredAt,
	})
	if !scheduler.recordRemovalTombstone(authID, 8, removedAt) {
		t.Fatal("recordRemovalTombstone() rejected the authoritative removal")
	}
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got != nil {
		t.Fatalf("removed auth remained scheduled: %#v", got)
	}

	for _, stale := range []*Auth{
		{
			ID:         authID,
			Provider:   "gemini",
			Generation: 8,
			UpdatedAt:  removedAt,
		},
		{
			ID:         authID,
			Provider:   "gemini",
			Generation: 7,
			UpdatedAt:  removedAt.Add(time.Hour),
		},
	} {
		scheduler.upsertAuth(stale)
		if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got != nil {
			t.Fatalf("stale snapshot restored removed auth: %#v", got)
		}
	}

	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 9,
		UpdatedAt:  registeredAt,
	})
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got == nil || got.Generation != 9 {
		t.Fatalf("new registration was not scheduled: %#v", got)
	}

	if scheduler.recordRemovalTombstone(authID, 8, removedAt.Add(2*time.Hour)) {
		t.Fatal("stale removal tombstone deleted a newer registration")
	}
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got == nil || got.Generation != 9 {
		t.Fatalf("stale removal changed newer registration: %#v", got)
	}
}

func TestSchedulerRemovalWinsSameGenerationTimestampSkew(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	authID := "scheduler-removal-clock-skew-auth"
	newerUpdatedAt := time.Unix(400, 0)

	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 5,
		UpdatedAt:  newerUpdatedAt,
	})
	if !scheduler.recordRemovalTombstone(authID, 5, newerUpdatedAt.Add(-time.Hour)) {
		t.Fatal("authoritative removal lost to the same credential generation timestamp")
	}
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got != nil {
		t.Fatalf("same-generation authoritative removal left auth scheduled: %#v", got)
	}
	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 5,
		UpdatedAt:  newerUpdatedAt,
	})
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got != nil {
		t.Fatalf("snapshot tied with tombstone restored auth: %#v", got)
	}
}

func TestSchedulerOrdersSameCredentialGenerationByUpdatedAt(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	authID := "scheduler-updated-at-auth"
	newerAt := time.Unix(300, 0)

	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 5,
		UpdatedAt:  newerAt,
		Attributes: map[string]string{"priority": "10"},
	})
	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "claude",
		Generation: 5,
		UpdatedAt:  newerAt.Add(-time.Second),
		Attributes: map[string]string{"priority": "1"},
	})
	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "claude",
		Generation: 4,
		UpdatedAt:  newerAt.Add(time.Hour),
		Attributes: map[string]string{"priority": "1"},
	})

	got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID)
	if got == nil || got.Provider != "gemini" || got.Attributes["priority"] != "10" {
		t.Fatalf("stale snapshot replaced current scheduler state: %#v", got)
	}
	if staleProvider := schedulerAuthSnapshotForTest(scheduler, "claude", authID); staleProvider != nil {
		t.Fatalf("stale provider snapshot was scheduled: %#v", staleProvider)
	}

	scheduler.upsertAuth(&Auth{
		ID:         authID,
		Provider:   "claude",
		Generation: 5,
		UpdatedAt:  newerAt.Add(time.Second),
		Attributes: map[string]string{"priority": "20"},
	})
	got = schedulerAuthSnapshotForTest(scheduler, "claude", authID)
	if got == nil || got.Attributes["priority"] != "20" {
		t.Fatalf("newer same-generation snapshot was rejected: %#v", got)
	}
	if oldProvider := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); oldProvider != nil {
		t.Fatalf("provider change left the old scheduler entry: %#v", oldProvider)
	}
}

func TestSchedulerRebuildPreservesRemovalTombstone(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	authID := "scheduler-rebuild-auth"
	registeredAt := time.Unix(400, 0)
	removedAt := time.Unix(500, 0)
	stale := &Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 3,
		UpdatedAt:  registeredAt,
	}

	scheduler.upsertAuth(stale)
	if !scheduler.recordRemovalTombstone(authID, 3, removedAt) {
		t.Fatal("recordRemovalTombstone() rejected current generation")
	}
	scheduler.rebuild([]*Auth{stale})
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got != nil {
		t.Fatalf("rebuild restored a tombstoned snapshot: %#v", got)
	}

	scheduler.rebuild([]*Auth{{
		ID:         authID,
		Provider:   "gemini",
		Generation: 4,
		UpdatedAt:  registeredAt,
	}})
	if got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID); got == nil || got.Generation != 4 {
		t.Fatalf("rebuild rejected a newer registration: %#v", got)
	}
}

func TestSchedulerStoresIndependentAuthSnapshot(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	authID := "scheduler-clone-auth"
	modelID := "scheduler-clone-model"
	auth := &Auth{
		ID:         authID,
		Provider:   "gemini",
		Generation: 1,
		UpdatedAt:  time.Unix(600, 0),
		Attributes: map[string]string{"priority": "9"},
		ModelStates: map[string]*ModelState{
			modelID: {Status: StatusActive},
		},
	}
	scheduler.upsertAuth(auth)

	auth.Provider = "claude"
	auth.Attributes["priority"] = "0"
	auth.ModelStates[modelID].Status = StatusDisabled
	auth.Disabled = true

	got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID)
	if got == nil {
		t.Fatal("scheduler did not retain the auth snapshot")
	}
	if got.Provider != "gemini" || got.Attributes["priority"] != "9" || got.Disabled {
		t.Fatalf("caller mutation changed scheduler metadata: %#v", got)
	}
	if got.ModelStates[modelID] == nil || got.ModelStates[modelID].Status != StatusActive {
		t.Fatalf("caller mutation changed scheduler model state: %#v", got.ModelStates[modelID])
	}
}

func TestSchedulerConcurrentSnapshotsKeepHighestVersion(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	authID := "scheduler-concurrent-auth"
	updatedAt := time.Unix(700, 0)

	var wg sync.WaitGroup
	for generation := uint64(1); generation <= 64; generation++ {
		generation := generation
		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduler.upsertAuth(&Auth{
				ID:         authID,
				Provider:   "gemini",
				Generation: generation,
				UpdatedAt:  updatedAt.Add(time.Duration(generation)),
			})
		}()
	}
	wg.Wait()

	got := schedulerAuthSnapshotForTest(scheduler, "gemini", authID)
	if got == nil || got.Generation != 64 {
		t.Fatalf("concurrent scheduler generation = %#v, want 64", got)
	}
}
