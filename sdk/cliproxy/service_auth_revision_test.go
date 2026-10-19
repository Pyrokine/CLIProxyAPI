package cliproxy

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func authUpdateWithRevision(update AuthUpdate, revision uint64) AuthUpdate {
	update.SetRevision(revision)
	return update
}

func TestAuthUpdateBridgePreservesRevision(t *testing.T) {
	internal := watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionModify,
		ID:     "account",
		Auth:   &coreauth.Auth{ID: "account", Provider: "test"},
	}
	internal.SetRevision(7)
	converted := fromWatcherAuthUpdate(internal)
	if converted.Revision() != 7 {
		t.Fatalf("SDK revision = %d, want 7", converted.Revision())
	}
	roundTrip := toWatcherAuthUpdate(converted)
	if roundTrip.Revision() != 7 {
		t.Fatalf("watcher revision = %d, want 7", roundTrip.Revision())
	}
}

func TestRuntimeAuthSyncHookRestoresModelsAfterRequestCancellation(t *testing.T) {
	const authID = "persisted-sync-auth"
	manager := coreauth.NewManager(nil, nil, nil)
	disabled := &coreauth.Auth{
		ID:         authID,
		Provider:   "qwen",
		Status:     coreauth.StatusDisabled,
		Disabled:   true,
		Attributes: map[string]string{coreauth.AttributePath: "/auth/qwen.json"},
	}
	registered, errRegister := manager.Register(context.Background(), disabled)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
		_ = manager.StopDeferredAuthRecreates(context.Background())
	})

	var watcherRevision uint64
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
		watcher: &WatcherWrapper{
			dispatchPersistedAuthWithRev: func(update *AuthUpdate) (bool, uint64) {
				watcherRevision++
				update.SetRevision(watcherRevision)
				return true, watcherRevision
			},
		},
	}
	enabled := registered.Clone()
	enabled.Disabled = false
	enabled.Status = coreauth.StatusActive
	enabled.StatusMessage = ""
	updated, errUpdate := manager.Update(context.Background(), enabled)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if errHook := service.runtimeAuthSyncHook()(canceledCtx, updated); errHook != nil {
		t.Fatal(errHook)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Disabled || current.Status != coreauth.StatusActive {
		t.Fatalf("runtime auth = %#v, want enabled", current)
	}
	if watcherRevision != 1 || service.authUpdateRevisions[authID] != 1 {
		t.Fatalf("watcher revision=%d processed=%d, want 1", watcherRevision, service.authUpdateRevisions[authID])
	}
	if models := GlobalModelRegistry().GetModelsForClient(authID); len(models) == 0 {
		t.Fatal("enabled persisted auth did not restore models synchronously")
	}
	if _, ok = manager.Executor("qwen"); !ok {
		t.Fatal("enabled persisted auth did not restore its executor")
	}
}

func TestRuntimeAuthSyncHookIgnoresOlderStatusUpdate(t *testing.T) {
	const authID = "status-hook-order"
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(t.Context(), &coreauth.Auth{
		ID: authID, Provider: "qwen", Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributePath: "/auth/qwen.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
		_ = manager.StopDeferredAuthRecreates(context.Background())
	})

	disabled := registered.Clone()
	disabled.Disabled = true
	disabled.Status = coreauth.StatusDisabled
	first, err := manager.Update(t.Context(), disabled)
	if err != nil {
		t.Fatal(err)
	}
	enabled := first.Clone()
	enabled.Disabled = false
	enabled.Status = coreauth.StatusActive
	latest, err := manager.Update(t.Context(), enabled)
	if err != nil {
		t.Fatal(err)
	}

	var watcherRevision uint64
	service := &Service{
		cfg: &config.Config{}, coreManager: manager,
		watcher: &WatcherWrapper{dispatchPersistedAuthWithRev: func(update *AuthUpdate) (bool, uint64) {
			watcherRevision++
			update.SetRevision(watcherRevision)
			return true, watcherRevision
		}},
	}
	hook := service.runtimeAuthSyncHook()
	if err = hook(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	if err = hook(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current.Disabled || current.Status != coreauth.StatusActive {
		t.Fatalf("old status hook replaced a newer state: %#v", current)
	}
	if watcherRevision != 1 {
		t.Fatalf("stale status hook dispatched %d watcher updates, want 1", watcherRevision)
	}
}

func TestHandleAuthUpdateRejectsStaleWatcherRevision(t *testing.T) {
	const authID = "revision-auth"
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
		_ = manager.StopDeferredAuthRecreates(context.Background())
	})

	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(
			AuthUpdate{
				Action:            AuthUpdateActionAdd,
				ID:                authID,
				Auth:              &coreauth.Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "new"}},
				AllowAuthRecreate: true,
			},
			2,
		),
	)
	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(
			AuthUpdate{
				Action:            AuthUpdateActionModify,
				ID:                authID,
				Auth:              &coreauth.Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "stale"}},
				AllowAuthRecreate: true,
			},
			1,
		),
	)
	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(AuthUpdate{Action: AuthUpdateActionDelete, ID: authID}, 1),
	)
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Metadata["token"] != "new" {
		t.Fatalf("stale updates changed auth: %#v", current)
	}

	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(
			AuthUpdate{
				Action: AuthUpdateActionModify,
				ID:     authID,
				Auth:   &coreauth.Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "latest"}},
			},
			3,
		),
	)
	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(AuthUpdate{Action: AuthUpdateActionDelete, ID: authID}, 3),
	)
	current, ok = manager.GetByID(authID)
	if !ok || current == nil || current.Metadata["token"] != "latest" {
		t.Fatalf("duplicate revision changed auth: %#v", current)
	}

	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(AuthUpdate{Action: AuthUpdateActionDelete, ID: authID}, 4),
	)
	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(
			AuthUpdate{
				Action:            AuthUpdateActionAdd,
				ID:                authID,
				Auth:              &coreauth.Auth{ID: authID, Provider: "test"},
				AllowAuthRecreate: true,
			},
			3,
		),
	)
	if _, ok = manager.GetByID(authID); ok {
		t.Fatal("stale add restored a newer deletion")
	}
	if service.authUpdateRevisions[authID] != 4 {
		t.Fatalf("processed revision = %d, want 4", service.authUpdateRevisions[authID])
	}
}

func TestHandleAuthUpdateAcceptsUnversionedCompatibilityUpdate(t *testing.T) {
	const authID = "legacy-auth-update"
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
		_ = manager.StopDeferredAuthRecreates(context.Background())
	})

	service.handleAuthUpdate(
		context.Background(),
		authUpdateWithRevision(
			AuthUpdate{
				Action:            AuthUpdateActionAdd,
				ID:                authID,
				Auth:              &coreauth.Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "versioned"}},
				AllowAuthRecreate: true,
			},
			1,
		),
	)
	service.handleAuthUpdate(
		context.Background(),
		AuthUpdate{
			Action: AuthUpdateActionModify,
			ID:     authID,
			Auth:   &coreauth.Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "legacy"}},
		},
	)
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Metadata["token"] != "legacy" {
		t.Fatalf("unversioned compatibility update = %#v, want legacy token", current)
	}
}

func TestAuthRuntimePublicationKeepsNewerPersistedState(t *testing.T) {
	for _, mode := range []string{"post-persist-hook", "versioned-watcher"} {
		for _, mutation := range []string{"disable", "credentials", "delete"} {
			t.Run(mode+"/"+mutation, func(t *testing.T) {
				const authID = "persisted-publication-generation"
				store := &blockingModelRefreshStore{}
				manager := coreauth.NewManager(store, nil, nil)
				older, err := manager.Register(t.Context(), &coreauth.Auth{
					ID: authID, Provider: "qwen", Status: coreauth.StatusActive,
					Attributes: map[string]string{coreauth.AttributePath: "/synthetic/qwen.json"},
					Metadata:   map[string]any{"access_token": "synthetic-old-token"},
				})
				if err != nil {
					t.Fatal(err)
				}
				service := &Service{cfg: &config.Config{}, coreManager: manager}
				t.Cleanup(func() {
					GlobalModelRegistry().UnregisterClient(authID)
					if err := service.stopModelRefreshWorkers(context.Background()); err != nil {
						t.Error(err)
					}
					if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
						t.Error(err)
					}
				})
				var latest *coreauth.Auth
				persistNewer := func() {
					if mutation == "delete" {
						if err := manager.Remove(t.Context(), authID); err != nil {
							t.Fatal(err)
						}
						return
					}
					next := older.Clone()
					if mutation == "disable" {
						next.Disabled = true
						next.Status = coreauth.StatusDisabled
					} else {
						next.Metadata["access_token"] = "synthetic-new-token"
					}
					var err error
					latest, err = manager.Update(t.Context(), next)
					if err != nil {
						t.Fatal(err)
					}
					if latest == nil || latest.Generation <= older.Generation {
						t.Fatal("new persisted snapshot did not advance generation")
					}
				}
				if mode == "post-persist-hook" {
					entered := make(chan struct{})
					release := make(chan struct{})
					finished := make(chan struct{})
					result := make(chan error, 1)
					var revision uint64
					service.watcher = &WatcherWrapper{
						dispatchPersistedAuthWithRev: func(update *AuthUpdate) (bool, uint64) {
							revision++
							update.SetRevision(revision)
							if revision == 1 {
								close(entered)
								<-release
							}
							return true, revision
						},
					}
					t.Cleanup(func() {
						select {
						case <-release:
						default:
							close(release)
						}
						<-finished
					})
					go func() {
						defer close(finished)
						result <- service.runtimeAuthSyncHook()(context.Background(), older)
					}()
					<-entered
					persistNewer()
					close(release)
					if err := <-result; err != nil {
						t.Fatal(err)
					}
					if latest != nil {
						if err := service.runtimeAuthSyncHook()(t.Context(), latest); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					persistNewer()
					update := AuthUpdate{Action: AuthUpdateActionModify, ID: authID, Auth: older}
					update.SetRevision(99)
					service.handleAuthUpdate(coreauth.WithSkipPersist(t.Context()), update)
				}
				current, exists := manager.GetByID(authID)
				if mutation == "delete" {
					if exists || len(GlobalModelRegistry().GetModelsForClient(authID)) != 0 {
						t.Fatal("older publication restored a newer deletion")
					}
					return
				}
				store.mu.Lock()
				persisted := store.last.Clone()
				store.mu.Unlock()
				if !exists || persisted == nil {
					t.Fatal("new persisted auth is missing")
				}
				if current.Disabled != persisted.Disabled {
					t.Fatalf("runtime disabled = %v, persisted disabled = %v", current.Disabled, persisted.Disabled)
				}
				if current.Metadata["access_token"] != persisted.Metadata["access_token"] {
					t.Fatal("older publication replaced the newer persisted credential")
				}
				if mutation == "disable" && len(GlobalModelRegistry().GetModelsForClient(authID)) != 0 {
					t.Fatal("disabled persisted auth retained published models")
				}
			})
		}
	}
}

func TestAuthRuntimePublicationRejectsSnapshotChangedDuringMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const authID = "persisted-mutation-generation"
		store := &blockingModelRefreshStore{
			entered: make(chan struct{}, 1),
			release: make(chan struct{}),
		}
		manager := coreauth.NewManager(store, nil, nil)
		older, err := manager.Register(t.Context(), &coreauth.Auth{
			ID: authID, Provider: "qwen", Status: coreauth.StatusActive,
			Metadata: map[string]any{"access_token": "synthetic-old-token"},
		})
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{cfg: &config.Config{}, coreManager: manager}
		t.Cleanup(func() {
			GlobalModelRegistry().UnregisterClient(authID)
			if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
				t.Error(err)
			}
		})
		store.mu.Lock()
		store.block = true
		store.mu.Unlock()
		persistDone := make(chan error, 1)
		disabled := older.Clone()
		disabled.Disabled = true
		disabled.Status = coreauth.StatusDisabled
		go func() {
			_, err := manager.Update(t.Context(), disabled)
			persistDone <- err
		}()
		<-store.entered
		applyDone := make(chan error, 1)
		go func() {
			applyDone <- service.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(t.Context()), older)
		}()
		synctest.Wait()
		select {
		case err := <-applyDone:
			t.Fatalf("publication bypassed the in-flight mutation: %v", err)
		default:
		}
		close(store.release)
		if err := <-persistDone; err != nil {
			t.Fatal(err)
		}
		if err := <-applyDone; err != nil {
			t.Fatal(err)
		}
		current, ok := manager.GetByID(authID)
		if !ok || !current.Disabled {
			t.Fatal("older publication replaced the state committed while waiting for the mutation lock")
		}
		if len(GlobalModelRegistry().GetModelsForClient(authID)) != 0 {
			t.Fatal("newer disabled auth retained published models")
		}
	})
}
