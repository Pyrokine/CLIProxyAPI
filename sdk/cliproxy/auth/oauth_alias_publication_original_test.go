package auth

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalAliasRegistryEpochAndBatchInputs(t *testing.T) {
	for _, test := range []struct{ id, model string }{
		{"client-projection-gen-test", "gemini-3.7-proj-test"},
		{"client-unreg-gen-test", "model-unreg-test"},
		{"client-rereg-gen-test", "model-rereg-test"},
		{"auth-epoch-gen-test", "gemini-epoch-test-model"},
		{"client-proj-strict-test", "model-proj-strict-1"},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(test.id, "gemini", []*registry.ModelInfo{{ID: test.model}})
				t.Cleanup(func() { reg.UnregisterClient(test.id) })
				epoch := reg.ClientRegistrationEpoch(test.id)
				for _, invalid := range []struct {
					epoch uint64
					batch []registry.ClientModelProjection
				}{
					{epoch + 10, []registry.ClientModelProjection{{ModelID: test.model, Suspended: true}}},
					{epoch, nil},
					{epoch, []registry.ClientModelProjection{{ModelID: "model-proj-strict-unowned", Suspended: true}}},
				} {
					if reg.ApplyClientModelProjections(test.id, invalid.epoch, 10, invalid.batch) {
						t.Fatal("invalid epoch or batch was accepted")
					}
				}
				if !reg.ApplyClientModelProjections(
					test.id, epoch, 10, []registry.ClientModelProjection{{ModelID: test.model}},
				) {
					t.Fatal("invalid batch advanced publication generation")
				}
				if reg.ApplyClientModelProjections(
					test.id, epoch, 5,
					[]registry.ClientModelProjection{{ModelID: test.model, Suspended: true, QuotaExceeded: true}},
				) ||
					reg.IsModelSuspendedForClient(test.id, test.model) {
					t.Fatal("old generation replaced the current clean projection")
				}
				if !reg.ApplyClientModelProjections(
					test.id, epoch, 11,
					[]registry.ClientModelProjection{{ModelID: test.model, Suspended: true, QuotaExceeded: true}},
				) ||
					!reg.IsModelSuspendedForClient(test.id, test.model) {
					t.Fatal("new generation was not published")
				}
				reg.UnregisterClient(test.id)
				removedEpoch := reg.ClientRegistrationEpoch(test.id)
				if removedEpoch <= epoch || reg.ApplyClientModelProjections(
					test.id, removedEpoch, 15, []registry.ClientModelProjection{{ModelID: test.model}},
				) {
					t.Fatal("unregistered client accepted a newer projection")
				}
				reg.RegisterClient(test.id, "gemini", []*registry.ModelInfo{{ID: test.model}})
				currentEpoch := reg.ClientRegistrationEpoch(test.id)
				if currentEpoch <= removedEpoch || reg.ApplyClientModelProjections(
					test.id, epoch, 99999, []registry.ClientModelProjection{{ModelID: test.model, Suspended: true}},
				) ||
					!reg.ApplyClientModelProjections(
						test.id, currentEpoch, 1, []registry.ClientModelProjection{{ModelID: test.model}},
					) {
					t.Fatal("old epoch overrode the current registration")
				}
			},
		)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		"client-owner-multi-test", "gemini",
		[]*registry.ModelInfo{{ID: "model-shared-multi-test"}, {ID: "model-owner-only-multi-test"}},
	)
	reg.RegisterClient("client-other-multi-test", "gemini", []*registry.ModelInfo{{ID: "model-shared-multi-test"}})
	t.Cleanup(
		func() {
			reg.UnregisterClient("client-owner-multi-test")
			reg.UnregisterClient("client-other-multi-test")
		},
	)
	if reg.ApplyClientModelProjections(
		"client-other-multi-test",
		reg.ClientRegistrationEpoch("client-other-multi-test"),
		1,
		[]registry.ClientModelProjection{
			{ModelID: "model-owner-only-multi-test", Suspended: true, QuotaExceeded: true},
		},
	) ||
		reg.IsModelSuspendedForClient("client-owner-multi-test", "model-owner-only-multi-test") {
		t.Fatal("another client changed an unowned model")
	}
}

func TestOriginalAliasSchedulerStaleDisabledAndRemovalInputs(t *testing.T) {
	for _, test := range []struct{ id, model string }{
		{"auth-gen-race-test", "gemini-3.7-flash-gen-test"},
		{"auth-tombstone-test", "gemini-3.7-tombstone-test"},
		{"auth-remove-tombstone-test", "gemini-3.7-flash-remove-tombstone"},
		{"auth-rereg-tombstone-test", "gemini-3.7-flash-rereg-test"},
		{"auth-remove-tombstone-epoch-test", "gemini-remove-tombstone-epoch-model"},
		{"auth-old-tombstone-rejection-test", "gemini-old-tombstone-rejection-model"},
		{"auth-update-stale-epoch-test", "gemini-update-stale-epoch-model"},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				manager, first, _ := originalOAuthAliasManager(
					t, &Auth{ID: test.id, Provider: "gemini", Generation: 5}, nil, []string{test.model}, nil,
				)
				disabled := first.Clone()
				disabled.Disabled = true
				disabled.Status = StatusDisabled
				disabled.UpdatedAt = first.UpdatedAt.Add(-10 * time.Minute)
				active := first.Clone()
				active.Label = "current active"
				active, err := manager.Update(t.Context(), active)
				if err != nil {
					t.Fatal(err)
				}
				manager.scheduler.upsertAuth(disabled)
				if picked, err := manager.scheduler.pickSingle(
					t.Context(), active.Provider, test.model, core.Options{}, nil,
				); err != nil || picked.ID != active.ID {
					t.Fatalf("stale disabled snapshot removed active entry: picked=%+v error=%v", picked, err)
				}
				currentDisabled := active.Clone()
				currentDisabled.Disabled = true
				currentDisabled.Status = StatusDisabled
				currentDisabled, err = manager.Update(t.Context(), currentDisabled)
				if err != nil {
					t.Fatal(err)
				}
				manager.scheduler.upsertAuth(active)
				if _, err := manager.scheduler.pickSingle(
					t.Context(), active.Provider, test.model, core.Options{}, nil,
				); err == nil {
					t.Fatal("stale active snapshot reactivated a disabled credential")
				}
				if err := manager.Remove(WithRuntimeOnlyRemove(WithSkipPersist(t.Context())), active.ID); err != nil {
					t.Fatal(err)
				}
				removed := schedulerAuthVersionForTest(t, manager.scheduler, active.ID)
				manager.scheduler.upsertAuth(active)
				manager.scheduler.rebuild([]*Auth{first, active, currentDisabled})
				if !removed.tombstone || removed.generation <= currentDisabled.Generation ||
					schedulerAuthSnapshotForTest(
						manager.scheduler, active.Provider, active.ID,
					) != nil {
					t.Fatal("removed credential was restored from a stale snapshot or rebuild")
				}
				fresh, err := manager.Register(
					WithAllowAuthRecreate(t.Context()), &Auth{ID: active.ID, Provider: active.Provider, Generation: 1},
				)
				if err != nil || fresh.Generation <= removed.generation {
					t.Fatalf("explicit re-registration did not advance generation: auth=%+v error=%v", fresh, err)
				}
				if manager.scheduler.recordRemovalTombstone(active.ID, removed.generation, time.Now().Add(time.Hour)) {
					t.Fatal("delayed removal deleted a newer registration")
				}
				if _, err := manager.Update(t.Context(), active); err == nil {
					t.Fatal("old registration snapshot replaced the newly registered credential")
				}
				if picked, err := manager.scheduler.pickSingle(
					t.Context(), active.Provider, test.model, core.Options{}, nil,
				); err != nil || picked.Generation != fresh.Generation {
					t.Fatalf("fresh registration was not selectable: picked=%+v error=%v", picked, err)
				}
			},
		)
	}
}

func TestOriginalAliasLoadUsesMonotonicPublicationAndRemoval(t *testing.T) {
	const model = "gemini-load-epoch-test-model"
	store := &countingStore{
		list: []*Auth{
			{ID: "auth-load-epoch-1", Provider: "gemini", Status: StatusActive, Generation: 10},
			{ID: "auth-load-epoch-2", Provider: "gemini", Status: StatusActive, Generation: 20},
		},
	}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	for _, id := range []string{"auth-load-epoch-1", "auth-load-epoch-2"} {
		registerProjectionTestClient(t, id, "gemini", &registry.ModelInfo{ID: model})
	}
	if err := manager.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, _ := manager.GetByID("auth-load-epoch-1")
	removed, _ := manager.GetByID("auth-load-epoch-2")
	if first == nil || removed == nil || first.Generation == 0 || removed.Generation == 0 ||
		first.Generation == removed.Generation {
		t.Fatal("Load did not assign distinct manager-owned generations")
	}
	store.list = store.list[:1]
	if err := manager.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := manager.GetByID(first.ID)
	version := schedulerAuthVersionForTest(t, manager.scheduler, removed.ID)
	if reloaded.Generation <= first.Generation || !version.tombstone || version.generation <= removed.Generation {
		t.Fatal("reload or omitted credential removal did not advance publication")
	}
	manager.scheduler.upsertAuth(removed)
	for range 3 {
		picked, err := manager.scheduler.pickSingle(t.Context(), "gemini", model, core.Options{}, nil)
		if err != nil || picked.ID != first.ID {
			t.Fatalf("removed store entry was scheduled: picked=%+v error=%v", picked, err)
		}
	}
}

func TestOriginalAliasRegistryConcurrentHighLoad(t *testing.T) {
	const id, provider = "auth-high-load-stability", "gemini"
	manager, auth, _ := originalOAuthAliasManager(
		t, &Auth{ID: id, Provider: provider}, nil, []string{"model-stable-1", "model-stable-2"}, nil,
	)
	reg := registry.GetGlobalRegistry()
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Go(
			func() {
				model := "model-stable-1"
				if worker%2 == 1 {
					model = "model-stable-2"
				}
				for iteration := range 50 {
					if worker < 4 {
						manager.RefreshSchedulerEntry(id)
					} else if iteration%2 == 0 {
						manager.MarkResult(
							t.Context(), originalOAuthAliasQuotaResult(manager, id, model, 10*time.Minute),
						)
					} else {
						current, _ := manager.GetByID(id)
						manager.MarkResult(
							t.Context(),
							executionResult(current, provider, model, nil, core.DispatchPhaseResponseStarted),
						)
					}
				}
			},
		)
	}
	workers.Go(
		func() {
			for iteration := range 50 {
				models := []*registry.ModelInfo{{ID: "model-stable-1"}, {ID: "model-stable-2"}}
				if iteration%2 == 1 {
					models = []*registry.ModelInfo{{ID: "model-stable-2"}, {ID: "model-stable-3"}}
				}
				reg.RegisterClient(id, provider, models)
			}
		},
	)
	workers.Go(
		func() {
			for range 50 {
				_, _ = manager.scheduler.pickSingle(t.Context(), provider, "model-stable-2", core.Options{}, nil)
			}
		},
	)
	workers.Wait()
	before, _ := manager.GetByID(id)
	reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: "model-stable-final"}})
	if _, _, err := manager.ResetQuota(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	manager.RefreshSchedulerEntry(id)
	if reg.IsModelSuspendedForClient(id, "model-stable-final") {
		t.Fatal("clean final model inherited an old target restriction")
	}
	current, _ := manager.GetByID(id)
	if current.Generation < auth.Generation || len(before.ModelStates) == 0 {
		t.Fatal("concurrent results lost the credential or target state")
	}
}

func TestOriginalAliasRegistrationEpochChangeKeepsAuthority(t *testing.T) {
	for _, test := range []struct{ id, first, retained, next string }{
		{"auth-reconcile-epoch-change-test", "gemini-route-model-a", "gemini-route-model-b", "gemini-route-model-c"},
		{
			"auth-reconcile-exhaust-test", "gemini-reconcile-exhaust-model-a", "gemini-reconcile-exhaust-model-b",
			"gemini-reconcile-exhaust-model-a",
		},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				manager, auth, _ := originalOAuthAliasManager(
					t, &Auth{ID: test.id, Provider: "gemini"}, nil, []string{test.first, test.retained}, nil,
				)
				manager.MarkResult(
					t.Context(), originalOAuthAliasQuotaResult(manager, auth.ID, test.retained, 30*time.Minute),
				)
				before, _ := manager.GetByID(auth.ID)
				reg := registry.GetGlobalRegistry()
				oldEpoch := reg.ClientRegistrationEpoch(auth.ID)
				stop, done := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(done)
					for {
						select {
						case <-stop:
							return
						default:
							reg.RegisterClient(
								auth.ID, auth.Provider, []*registry.ModelInfo{{ID: test.retained}, {ID: test.next}},
							)
						}
					}
				}()
				manager.RefreshSchedulerEntry(auth.ID)
				close(stop)
				<-done
				current, _ := manager.GetByID(auth.ID)
				if !reflect.DeepEqual(before, current) {
					t.Fatal("registration publication changed credential authority")
				}
				reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: test.retained}, {ID: test.next}})
				if reg.ApplyClientModelProjections(
					auth.ID, oldEpoch, 99999, []registry.ClientModelProjection{{ModelID: test.retained}},
				) {
					t.Fatal("old epoch cleared retained target authority")
				}
				manager.RefreshSchedulerEntry(auth.ID)
				if !reg.IsModelSuspendedForClient(auth.ID, test.retained) || reg.IsModelSuspendedForClient(
					auth.ID, test.next,
				) {
					t.Fatal("current registration differs from retained and clean target states")
				}
			},
		)
	}
}
