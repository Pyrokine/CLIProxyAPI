package auth

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestHomeOriginalSessionCacheInputsKeepIndependentOwners(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	first := &replaceAwareExecutor{id: "test"}
	second := &replaceAwareExecutor{id: "other-provider"}
	manager.RegisterExecutor(first)
	manager.RegisterExecutor(second)
	t.Cleanup(func() { manager.CloseExecutionSession(CloseAllExecutionSessionsID) })
	owners := make(map[string]*HomeDispatchSelection)
	for _, tc := range []struct{ session, upstream string }{
		{"session-1", "upstream-model-a"},
		{"session-2", "upstream-model-b"},
	} {
		auth := &Auth{
			ID: "home-auth-1", Provider: "test", Status: StatusActive,
			Attributes: map[string]string{"websockets": "true", homeUpstreamModelAttributeKey: tc.upstream},
			Metadata:   map[string]any{"email": "home@example.com"},
		}
		auth.EnsureIndex()
		manager.rememberHomeRuntimeAuth(tc.session, auth)
		owners[tc.session] = auth.homeRuntimeOwner
		snapshot, ok := manager.GetExecutionSessionAuthByID(tc.session, auth.ID)
		if !ok || !authWebsocketsEnabled(snapshot) ||
			snapshot.Attributes[homeUpstreamModelAttributeKey] != tc.upstream {
			t.Fatalf("session=%s snapshot=%+v found=%t", tc.session, snapshot, ok)
		}
		ctx := executor.WithDownstreamWebsocket(t.Context())
		opts := executor.Options{
			Metadata: map[string]any{
				executor.ExecutionSessionMetadataKey: tc.session,
				executor.PinnedAuthMetadataKey:       "home-auth-1",
			}, Headers: http.Header{"Authorization": {"Bearer client-key"}},
		}
		selected, provider, err := manager.PickNext(ctx, "test", "gpt-5.4", opts, nil)
		if err != nil || provider != first || selected == nil || selected.ID != auth.ID ||
			selected.Attributes[homeUpstreamModelAttributeKey] != tc.upstream {
			t.Fatalf("session=%s selected=%+v executor=%T error=%v", tc.session, selected, provider, err)
		}
	}
	if _, ok := manager.GetByID("home-auth-1"); ok {
		t.Fatal("unscoped SDK getter exposed a Home session credential")
	}
	manager.CloseExecutionSession("session-1")
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth-1"); ok || owners["session-1"].Active() {
		t.Fatal("closed session retained its original owner or credential")
	}
	if _, ok := manager.GetExecutionSessionAuthByID("session-2", "home-auth-1"); !ok || !owners["session-2"].Active() {
		t.Fatal("closing session-1 cleared the independent session-2 owner")
	}
	owners["session-1"].End("repeated_old_end")
	manager.CloseExecutionSession("session-2")
	if _, ok := manager.GetExecutionSessionAuthByID("session-2", "home-auth-1"); ok || owners["session-2"].Active() {
		t.Fatal("last session retained its original owner or credential")
	}
	for _, provider := range []*replaceAwareExecutor{first, second} {
		if got := provider.ClosedSessionIDs(); !slices.Equal(got, []string{"session-1", "session-2"}) {
			t.Fatalf("executor=%s closed sessions=%v", provider.Identifier(), got)
		}
	}
}

func TestHomeOriginalOwnerlessPinnedInputsReturnVisibleErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		websockets bool
		count      int
		tried      bool
		owned      bool
	}{
		{name: "websocket without selection", websockets: true, count: 1},
		{name: "second Home attempt", websockets: true, count: 2},
		{name: "previously tried auth", websockets: true, count: 1, tried: true},
		{name: "previously tried live owner", websockets: true, count: 1, tried: true, owned: true},
		{name: "non websocket without selection", count: 1},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
				t.Cleanup(func() { manager.CloseExecutionSession(CloseAllExecutionSessionsID) })
				auth := &Auth{ID: "home-auth-1", Provider: "test", Status: StatusActive}
				if tc.websockets {
					auth.Attributes = map[string]string{"websockets": "true"}
				}
				if tc.owned {
					manager.rememberHomeRuntimeAuth("session-1", auth)
				} else {
					manager.homeRuntimeAuths["session-1"] = map[string]*Auth{auth.ID: auth}
				}
				opts := withHomeAuthCount(
					executor.Options{
						Metadata: map[string]any{
							executor.ExecutionSessionMetadataKey: "session-1",
							executor.PinnedAuthMetadataKey:       auth.ID,
						},
					}, tc.count,
				)
				var tried map[string]struct{}
				if tc.tried {
					tried = map[string]struct{}{auth.ID: {}}
				}
				ctx := executor.WithDownstreamWebsocket(t.Context())
				selected, provider, name, err := manager.pickNextViaHome(ctx, "gpt-5.4", opts, tried)
				var visible *Error
				if selected != nil || provider != nil || name != "" ||
					!errors.As(err, &visible) || visible.StatusCode() < 400 {
					t.Fatalf("unauthorized pinned cache result=%+v %T %q error=%v", selected, provider, name, err)
				}
				if tc.tried && (visible.Code != homeRequestRetryExceededErrorCode ||
					visible.StatusCode() != http.StatusServiceUnavailable) {
					t.Fatalf("tried credential error=%q status=%d", visible.Code, visible.StatusCode())
				}
				if _, ok := manager.GetExecutionSessionAuthByID("session-1", auth.ID); ok != tc.owned {
					t.Fatalf("session credential readable=%t, live owner=%t", ok, tc.owned)
				}
			},
		)
	}
}

func TestHomeOriginalRetryCountReleasesRetainedSelection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	provider := &replaceAwareExecutor{id: "test"}
	manager.RegisterExecutor(provider)
	auth := &Auth{
		ID: "home-auth-1", Provider: "test", Status: StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	manager.rememberHomeRuntimeAuth("session-1", auth)
	selection := auth.homeRuntimeOwner
	t.Cleanup(func() { selection.End("test_complete") })
	ctx := executor.WithDownstreamWebsocket(t.Context())
	opts := executor.Options{
		Metadata: map[string]any{
			executor.ExecutionSessionMetadataKey: "session-1",
			executor.PinnedAuthMetadataKey:       auth.ID,
		},
	}
	selection.Retain()
	if !manager.retainHomeWebsocketSelection(ctx, opts, "gpt-5.4", selection) {
		t.Fatal("original first-attempt selection was not retained")
	}
	if got, err := manager.pickHomeDispatchSelection(ctx, "gpt-5.4", opts); err != nil || got != selection {
		t.Fatalf("first selection=%p want=%p error=%v", got, selection, err)
	}
	dispatcher := &testHomeDispatcher{heartbeat: true, err: errors.New("synthetic Home redispatch failure")}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	if got, err := manager.pickHomeDispatchSelection(
		ctx, "gpt-5.4", withHomeAuthCount(opts, 2),
	); err == nil || got != nil {
		t.Fatalf("second attempt reused old selection=%p error=%v", got, err)
	}
	if selection.Active() || dispatcher.calls != 1 {
		t.Fatalf("old active=%t fresh Home calls=%d", selection.Active(), dispatcher.calls)
	}
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", auth.ID); ok {
		t.Fatal("second-attempt redispatch left the old session credential readable")
	}
}

func TestHomeOriginalDisableInputEndsItsSessionOwner(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
	auth := &Auth{
		ID: "home-auth-1", Provider: "test", Status: StatusActive,
		Attributes: map[string]string{"websockets": "true", homeUpstreamModelAttributeKey: "upstream-model"},
	}
	manager.rememberHomeRuntimeAuth("session-1", auth)
	owner := auth.homeRuntimeOwner
	t.Cleanup(func() { owner.End("test_complete") })
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", auth.ID); !ok || !owner.Active() {
		t.Fatal("original enabled Home input did not publish an active session credential")
	}
	manager.SetConfig(&config.Config{})
	if _, ok := manager.GetExecutionSessionAuthByID(
		"session-1", auth.ID,
	); ok || owner.Active() || manager.HomeEnabled() {
		t.Fatal("disabling Home retained its active session owner or readable credential")
	}
}
