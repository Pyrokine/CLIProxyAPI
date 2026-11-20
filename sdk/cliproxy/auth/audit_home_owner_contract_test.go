package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func auditOwnedHomeSelection(
	t *testing.T,
	manager *Manager,
	session string,
	provider ProviderExecutor,
) (*HomeDispatchSelection, *executionregistry.Registry) {
	t.Helper()
	registry := executionregistry.New()
	pending, err := registry.BeginDispatch()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := registry.Install(
		pending,
		executionregistry.ScopeSpec{
			CredentialID: "synthetic-owner-auth",
			Model:        "owner-model",
			Accounted:    true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	auth := &Auth{
		ID: "synthetic-owner-auth", Index: "synthetic-index", Provider: provider.Identifier(),
		Attributes: map[string]string{"websockets": "true"},
		Metadata:   map[string]any{"access_token": "synthetic-owner-token"},
	}
	selection, err := newHomeDispatchSelection(auth, provider, provider.Identifier(), scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { selection.End("test_complete") })
	ctx := executor.WithDownstreamWebsocket(t.Context())
	opts := executor.Options{Metadata: map[string]any{executor.ExecutionSessionMetadataKey: session}}
	if err := manager.bindHomeSelectionRuntimeAuth(ctx, opts, selection); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.GetExecutionSessionAuthByID(session, auth.ID); !ok {
		t.Fatal("live selection did not publish its session credential")
	}
	return selection, registry
}

func TestAuditHomeRuntimeRequiresLiveSelectionOwner(t *testing.T) {
	for _, kind := range []string{"ownerless", "unbound", "ended", "scope_closed"} {
		t.Run(
			kind, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				provider := &replaceAwareExecutor{id: "synthetic-owner-provider"}
				manager.RegisterExecutor(provider)
				const session = "synthetic-session"
				const id = "synthetic-owner-auth"
				if kind == "ownerless" {
					manager.homeRuntimeAuths[session] = map[string]*Auth{
						id: {ID: id, Provider: provider.Identifier()},
					}
				} else if kind == "unbound" {
					manager.rememberHomeSelectionRuntimeAuth(
						session,
						&HomeDispatchSelection{
							Auth:     &Auth{ID: id, Provider: provider.Identifier()},
							Executor: provider,
						},
					)
				} else {
					selection, _ := auditOwnedHomeSelection(t, manager, session, provider)
					if kind == "ended" {
						selection.End("finished")
					} else {
						selection.scope.End("registry_shutdown")
						if selection.Active() || selection.Retained() {
							t.Error("closed scope still authorizes selection reuse")
						}
					}
					manager.rememberHomeSelectionRuntimeAuth(session, selection)
				}
				if _, ok := manager.GetExecutionSessionAuthByID(session, id); ok {
					t.Fatal("credential is readable without a live selection owner")
				}
				if _, _, _, ok := manager.homeRuntimeAuthByID(session, id); ok {
					t.Fatal("pinned execution admitted an unauthorized runtime credential")
				}
			},
		)
	}
}

func TestAuditHomeRuntimeCASRejectsReplacedSelection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	provider := &replaceAwareExecutor{id: "synthetic-owner-provider"}
	manager.RegisterExecutor(provider)
	old, _ := auditOwnedHomeSelection(t, manager, "session-a", provider)
	expected, _ := manager.GetExecutionSessionAuthByID("session-a", old.Auth.ID)
	current, _ := auditOwnedHomeSelection(t, manager, "session-a", provider)
	other, _ := auditOwnedHomeSelection(t, manager, "session-b", provider)
	updated := expected.Clone()
	updated.Metadata["access_token"] = "synthetic-stale-refresh"
	if manager.compareAndSwapHomeRuntimeAuth("session-a", expected, updated, provider) {
		t.Error("old owner published into a new selection with identical credentials and index")
	}
	old.End("late_old_owner")
	for _, session := range []string{"session-a", "session-b"} {
		snapshot, ok := manager.GetExecutionSessionAuthByID(session, current.Auth.ID)
		if !ok || authAccessToken(snapshot) != "synthetic-owner-token" {
			t.Fatalf("session=%s snapshot=%+v found=%t", session, snapshot, ok)
		}
		snapshot.Metadata["access_token"] = "caller-change"
		again, _ := manager.GetExecutionSessionAuthByID(session, current.Auth.ID)
		if authAccessToken(again) != "synthetic-owner-token" {
			t.Fatal("session getter returned an alias")
		}
	}
	if _, ok := manager.GetByID(current.Auth.ID); ok {
		t.Fatal("unscoped getter exposed a Home session credential")
	}
	manager.CloseExecutionSession("session-a")
	if current.Active() || !other.Active() {
		t.Fatal("closing one session did not end exactly its selection owners")
	}
	if _, ok := manager.GetExecutionSessionAuthByID("session-b", other.Auth.ID); !ok {
		t.Fatal("closing session-a removed session-b")
	}
}

// noinspection GoDirectComparisonOfErrors
func TestAuditHomeRuntimeCloseEndsUnretainedOwnerOnce(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "one_session", true: "all_sessions"}[all], func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				provider := &replaceAwareExecutor{id: "synthetic-owner-provider"}
				manager.RegisterExecutor(provider)
				selection, registry := auditOwnedHomeSelection(t, manager, "session", provider)
				var releases, closes atomic.Int32
				registry.SetReleaseSink(func(executionregistry.ReleaseGroup, int64) { releases.Add(1) })
				if err := selection.Bind(func() error { closes.Add(1); return nil }); err != nil {
					t.Fatal(err)
				}
				attempt, release, err := selection.AttemptContext(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				id := "session"
				if all {
					id = CloseAllExecutionSessionsID
				}
				manager.CloseExecutionSession(id)
				if attempt.Err() != context.Canceled || selection.Active() {
					t.Error("session close left its unretained attempt active")
				}
				release()
				release()
				selection.End("again")
				if closes.Load() != 1 || releases.Load() != 1 || selection.attemptCancels.Len() != 0 {
					t.Fatalf(
						"closes=%d releases=%d pending=%d", closes.Load(), releases.Load(),
						selection.attemptCancels.Len(),
					)
				}
			},
		)
	}
}

func TestAuditHomeRuntimeExecutorReplacementEndsOwner(t *testing.T) {
	for _, action := range []string{"register", "replace", "unregister", "unregister_same"} {
		t.Run(
			action, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				provider := &replaceAwareExecutor{id: "synthetic-owner-provider"}
				manager.RegisterExecutor(provider)
				selection, _ := auditOwnedHomeSelection(t, manager, "session", provider)
				replacement := &replaceAwareExecutor{id: provider.Identifier()}
				switch action {
				case "register":
					manager.RegisterExecutor(replacement)
				case "replace":
					if !manager.ReplaceExecutorIfSame(provider, replacement) {
						t.Fatal("executor replacement rejected")
					}
				case "unregister":
					manager.UnregisterExecutor(provider.Identifier())
				case "unregister_same":
					if !manager.UnregisterExecutorIfSame(provider.Identifier(), provider) {
						t.Fatal("executor removal rejected")
					}
				}
				manager.rememberHomeSelectionRuntimeAuth("session", selection)
				if selection.Active() {
					t.Error("replaced executor retained an active selection owner")
				}
				if _, ok := manager.GetExecutionSessionAuthByID("session", selection.Auth.ID); ok {
					t.Fatal("replaced owner republished its runtime credential")
				}
			},
		)
	}
}

func TestAuditHomeRuntimeEndPublicationRace(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	provider := &replaceAwareExecutor{id: "synthetic-owner-provider"}
	manager.RegisterExecutor(provider)
	for range 100 {
		selection, _ := auditOwnedHomeSelection(t, manager, "session", provider)
		var tasks sync.WaitGroup
		tasks.Add(2)
		go func() { defer tasks.Done(); selection.End("concurrent_end") }()
		go func() { defer tasks.Done(); manager.rememberHomeSelectionRuntimeAuth("session", selection) }()
		tasks.Wait()
		if _, ok := manager.GetExecutionSessionAuthByID("session", selection.Auth.ID); ok {
			t.Fatal("End/publication race retained a readable credential")
		}
	}
}

func TestAuditUnavailableWithoutDeadlineRequiresAuthorizedRecovery(t *testing.T) {
	for _, credential := range []bool{false, true} {
		for _, authority := range []StateResetAuthority{
			StateResetAuthoritySuccess, StateResetAuthorityReset, StateResetAuthorityOperator,
		} {
			for _, action := range []StateResetAuthority{
				StateResetAuthoritySuccess, StateResetAuthorityReset, StateResetAuthorityOperator,
			} {
				t.Run(
					string(authority)+"/"+string(action)+map[bool]string{
						false: "/model", true: "/credential",
					}[credential], func(t *testing.T) {
						auth := &Auth{ID: "synthetic-unavailable", Provider: "test", Status: StatusActive}
						state := &ModelState{
							Unavailable: true, Status: StatusError, ResetAuthority: authority, Generation: 3,
						}
						if credential {
							auth.Unavailable = true
							auth.StateEffectScope = executor.StateEffectScopeCredential
							auth.ResetAuthority = authority
						} else {
							auth.ModelStates = map[string]*ModelState{"owner-model": state}
						}
						selector := &FillFirstSelector{}
						if _, err := selector.Pick(
							t.Context(), "test", "owner-model", executor.Options{}, []*Auth{auth},
						); err == nil {
							t.Fatal("zero deadline admitted authoritative unavailable credential")
						}
						want := resetAuthorityRank(action) >= resetAuthorityRank(authority)
						cleared := false
						if credential {
							cleared = clearAuthState(auth, time.Now(), action)
						} else {
							cleared = clearModelState(state, time.Now(), action)
						}
						selected, err := selector.Pick(
							t.Context(), "test", "owner-model", executor.Options{}, []*Auth{auth},
						)
						if cleared != want || (err == nil) != want || want && selected.ID != auth.ID {
							t.Fatalf("cleared=%t allowed=%t want=%t", cleared, err == nil, want)
						}
					},
				)
			}
		}
	}
}
