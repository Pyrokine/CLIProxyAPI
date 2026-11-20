package cliproxy

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestAuthUpdateRefreshTimeInheritanceMatchesLifecycle(t *testing.T) {
	for _, test := range []struct {
		name                             string
		oldDisabled, newDisabled         bool
		oldStatus, newStatus             coreauth.Status
		emptyIncoming, inheritActiveTime bool
	}{
		{
			name: "active to active", oldStatus: coreauth.StatusActive, newStatus: coreauth.StatusActive,
			inheritActiveTime: true,
		},
		{
			name: "active to disabled flag", oldStatus: coreauth.StatusActive, newStatus: coreauth.StatusActive,
			newDisabled: true,
		},
		{
			name: "disabled flag to active", oldStatus: coreauth.StatusActive, newStatus: coreauth.StatusActive,
			oldDisabled: true,
		},
		{name: "active to disabled status", oldStatus: coreauth.StatusActive, newStatus: coreauth.StatusDisabled},
		{name: "disabled status to active", oldStatus: coreauth.StatusDisabled, newStatus: coreauth.StatusActive},
		{
			name: "disabled to disabled", oldStatus: coreauth.StatusDisabled, newStatus: coreauth.StatusDisabled,
			oldDisabled: true, newDisabled: true,
		},
		{
			name: "disable clears incoming times", oldStatus: coreauth.StatusActive, newStatus: coreauth.StatusDisabled,
			newDisabled: true, emptyIncoming: true,
		},
		{
			name: "enable accepts empty incoming times", oldStatus: coreauth.StatusDisabled,
			newStatus: coreauth.StatusActive, oldDisabled: true, emptyIncoming: true,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				const authID = "fixture-auth-update-lifecycle"
				manager := coreauth.NewManager(nil, nil, nil)
				service := &Service{cfg: &config.Config{}, coreManager: manager}
				t.Cleanup(
					func() {
						GlobalModelRegistry().UnregisterClient(authID)
						if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
							t.Error(err)
						}
					},
				)
				now := time.Now().UTC()
				original, err := manager.Register(
					t.Context(), &coreauth.Auth{
						ID: authID, Provider: "test", Disabled: test.oldDisabled, Status: test.oldStatus,
						CreatedAt: now.Add(-24 * time.Hour), LastRefreshedAt: now.Add(-time.Hour),
						NextRefreshAfter: now.Add(time.Hour),
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				incoming := &coreauth.Auth{
					ID: authID, Provider: "test", Disabled: test.newDisabled, Status: test.newStatus,
					CreatedAt: now, LastRefreshedAt: now.Add(-time.Minute), NextRefreshAfter: now.Add(2 * time.Hour),
				}
				if test.emptyIncoming {
					incoming.LastRefreshedAt = time.Time{}
					incoming.NextRefreshAfter = time.Time{}
				}
				wantLast, wantNext := incoming.LastRefreshedAt, incoming.NextRefreshAfter
				if test.inheritActiveTime {
					wantLast, wantNext = original.LastRefreshedAt, original.NextRefreshAfter
				}
				if err := service.applyCoreAuthAddOrUpdate(
					coreauth.WithSkipPersist(t.Context()), incoming,
				); err != nil {
					t.Fatal(err)
				}
				current, ok := manager.GetByID(authID)
				if !ok || current == nil {
					t.Fatal("lifecycle update removed the auth")
				}
				if !current.LastRefreshedAt.Equal(wantLast) || !current.NextRefreshAfter.Equal(wantNext) {
					t.Fatalf(
						"refresh times = %v/%v, want %v/%v", current.LastRefreshedAt, current.NextRefreshAfter,
						wantLast, wantNext,
					)
				}
				if !current.CreatedAt.Equal(original.CreatedAt) {
					t.Fatal("lifecycle update replaced the original creation time")
				}
				if incoming.CreatedAt != now {
					t.Fatal("lifecycle update mutated the caller's auth")
				}
			},
		)
	}
}

func TestAuthUpdateSameRevisionRegistersIndependentAuths(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	for _, id := range []string{"fixture-auth-revision-first", "fixture-auth-revision-second"} {
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(id) })
		update := AuthUpdate{Action: AuthUpdateActionAdd, ID: id, Auth: &coreauth.Auth{ID: id, Provider: "test"}}
		update.SetRevision(7)
		if err := service.handleAuthUpdate(coreauth.WithSkipPersist(t.Context()), update); err != nil {
			t.Error(err)
		}
		current, ok := manager.GetByID(id)
		if !ok || current.ID != id || service.authUpdateRevisions[id] != 7 {
			t.Fatalf("independent auth %s was rejected at a shared watcher revision", id)
		}
		update.Action = AuthUpdateActionDelete
		update.SetRevision(6)
		if err := service.handleAuthUpdate(coreauth.WithSkipPersist(t.Context()), update); err != nil {
			t.Error(err)
		}
		if _, ok := manager.GetByID(id); !ok {
			t.Fatalf("stale deletion removed independent auth %s", id)
		}
	}
	if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPersistedAuthUpdateDelegatesRevisionDispatch(t *testing.T) {
	var absent *WatcherWrapper
	if absent.DispatchPersistedAuthUpdate(AuthUpdate{}) ||
		(&WatcherWrapper{}).DispatchPersistedAuthUpdate(AuthUpdate{}) {
		t.Fatal("missing watcher dispatch was reported as queued")
	}
	for _, queued := range []bool{false, true} {
		update := AuthUpdate{
			Action: AuthUpdateActionModify, ID: "fixture-persisted-compat",
			Auth: &coreauth.Auth{ID: "fixture-persisted-compat"}, AllowAuthRecreate: true,
		}
		var calls int
		wrapper := &WatcherWrapper{
			dispatchPersistedAuthWithRev: func(actual *AuthUpdate) (bool, uint64) {
				calls++
				if actual.Action != update.Action || actual.ID != update.ID || actual.Auth != update.Auth ||
					!actual.AllowAuthRecreate {
					t.Fatal("legacy persisted dispatch changed its payload")
				}
				actual.SetRevision(17)
				return queued, 17
			},
		}
		if got := wrapper.DispatchPersistedAuthUpdate(update); got != queued || calls != 1 {
			t.Fatalf("legacy dispatch queued=%t calls=%d, want %t/1", got, calls, queued)
		}
		if update.Revision() != 0 {
			t.Fatal("by-value legacy dispatch mutated the caller's revision")
		}
	}
}
