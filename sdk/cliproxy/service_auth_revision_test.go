package cliproxy

import (
	"context"
	"testing"

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
