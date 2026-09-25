package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type affinityUnsupportedPluginScheduler struct{}

func (*affinityUnsupportedPluginScheduler) PickAuth(
	context.Context,
	pluginapi.SchedulerPickRequest,
) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{}, false, nil
}

func TestLookupSessionAffinityIsReadOnlyAndReturnsDetachedAuth(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "affinity-read-auth",
		Provider: "claude",
		Metadata: map[string]any{"marker": "original"},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	options := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": {"session-read"}},
		Metadata: map[string]any{cliproxyexecutor.SessionAffinityModelMetadataKey: "claude-model"},
	}
	selector.Remember("claude", options, registered)
	key := "claude::claude:session-read::claude-model"
	selector.cache.mu.RLock()
	entryBefore, exists := selector.cache.entries[key]
	countBefore := len(selector.cache.entries)
	selector.cache.mu.RUnlock()
	if !exists {
		t.Fatalf("binding %q was not created", key)
	}

	for iteration := 0; iteration < 10; iteration++ {
		found, status := manager.LookupSessionAffinity("claude", "claude-model", "session-read")
		if status != "bound" || found == nil || found.ID != registered.ID {
			t.Fatalf("lookup %d = (%#v, %q)", iteration, found, status)
		}
		if found.Index == "" {
			t.Fatal("lookup did not return a stable auth index")
		}
		found.Metadata["marker"] = "mutated clone"
	}
	selector.cache.mu.RLock()
	entryAfter := selector.cache.entries[key]
	countAfter := len(selector.cache.entries)
	selector.cache.mu.RUnlock()
	if !entryAfter.expiresAt.Equal(entryBefore.expiresAt) {
		t.Fatalf("lookup refreshed TTL: before=%v after=%v", entryBefore.expiresAt, entryAfter.expiresAt)
	}
	if countAfter != countBefore {
		t.Fatalf("lookup changed binding count: before=%d after=%d", countBefore, countAfter)
	}
	current, _ := manager.GetByID(registered.ID)
	if current.Metadata["marker"] != "original" {
		t.Fatalf("mutating lookup result changed manager auth: %#v", current.Metadata)
	}

	selector.cache.mu.RLock()
	entriesBeforeMiss := len(selector.cache.entries)
	selector.cache.mu.RUnlock()
	if found, status := manager.LookupSessionAffinity("claude", "claude-model", "missing-session"); status != "unbound" || found != nil {
		t.Fatalf("missing lookup = (%#v, %q)", found, status)
	}
	selector.cache.mu.RLock()
	entriesAfterMiss := len(selector.cache.entries)
	selector.cache.mu.RUnlock()
	if entriesAfterMiss != entriesBeforeMiss {
		t.Fatalf("missing lookup created a binding: before=%d after=%d", entriesBeforeMiss, entriesAfterMiss)
	}
}

func TestLookupSessionAffinityBoundUnboundAmbiguousUnsupportedAndProviderFilter(t *testing.T) {
	selector := NewSessionAffinitySelector(nil)
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	for _, auth := range []*Auth{
		{ID: "affinity-openai", Provider: "openai"},
		{ID: "affinity-claude", Provider: "claude"},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	selector.cache.Set("openai::header:bound-session::model", "affinity-openai")
	if found, status := manager.LookupSessionAffinity("openai", "model", "bound-session"); status != "bound" || found == nil || found.ID != "affinity-openai" {
		t.Fatalf("bound lookup = (%#v, %q)", found, status)
	}
	if found, status := manager.LookupSessionAffinity("openai", "model", "unbound-session"); status != "unbound" || found != nil {
		t.Fatalf("unbound lookup = (%#v, %q)", found, status)
	}

	selector.cache.Set("mixed::header:bound-session::model", "affinity-claude")
	selector.cache.Set("mixed::session:bound-session::model", "affinity-openai")
	if found, status := manager.LookupSessionAffinity("openai", "model", "bound-session"); status != "bound" || found == nil || found.ID != "affinity-openai" {
		t.Fatalf("provider filter produced false ambiguity: (%#v, %q)", found, status)
	}
	if authID, status := selector.LookupAffinity("mixed", "model", "bound-session"); status != "ambiguous" || authID != "" {
		t.Fatalf("mixed ambiguous lookup = (%q, %q)", authID, status)
	}

	roundRobinManager := NewManager(nil, &RoundRobinSelector{}, nil)
	if found, status := roundRobinManager.LookupSessionAffinity("openai", "model", "bound-session"); status != "unsupported" || found != nil {
		t.Fatalf("selector without lookup = (%#v, %q)", found, status)
	}
	manager.SetPluginScheduler(&affinityUnsupportedPluginScheduler{})
	if found, status := manager.LookupSessionAffinity("openai", "model", "bound-session"); status != "unsupported" || found != nil {
		t.Fatalf("plugin scheduler lookup = (%#v, %q)", found, status)
	}
}

func TestLookupSessionAffinityLCPDoesNotExtendTTL(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      80 * time.Millisecond,
	})
	defer selector.Stop()
	auth := &Auth{ID: "affinity-lcp", Provider: "openai"}
	options := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"stable prompt"}]}`),
		SourceFormat:    "openai",
		Metadata: map[string]any{
			cliproxyexecutor.CallerScopeMetadataKey: "affinity-lcp-caller",
		},
	}
	picked, errPick := selector.Pick(context.Background(), "openai", "model", options, []*Auth{auth})
	if errPick != nil || picked == nil {
		t.Fatalf("Pick() = (%#v, %v)", picked, errPick)
	}
	sessionID, _ := options.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string)
	if sessionID == "" {
		t.Fatal("LCP session ID was not published")
	}
	for iteration := 0; iteration < 3; iteration++ {
		time.Sleep(20 * time.Millisecond)
		if authID, status := selector.LookupAffinity("openai", "model", sessionID); iteration < 2 &&
			(status != "bound" || authID != auth.ID) {
			t.Fatalf("early LCP lookup %d = (%q, %q)", iteration, authID, status)
		}
	}
	time.Sleep(40 * time.Millisecond)
	if authID, status := selector.LookupAffinity("openai", "model", sessionID); status != "unbound" || authID != "" {
		t.Fatalf("LCP lookup extended TTL: (%q, %q)", authID, status)
	}
}

func TestAffinityResultOnlyMutatesCredentialAttributedFailures(t *testing.T) {
	selector := NewSessionAffinitySelector(nil)
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: "affinity-result-auth", Provider: "openai", Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	model := "affinity-model"
	bind := func(session string) cliproxyexecutor.Options {
		options := cliproxyexecutor.Options{
			Headers: http.Header{"X-Session-ID": {session}},
			Metadata: map[string]any{
				cliproxyexecutor.SessionAffinityModelMetadataKey:    model,
				cliproxyexecutor.SessionAffinityProviderMetadataKey: "openai",
			},
		}
		selector.Remember("openai", options, registered)
		return options
	}
	assertBound := func(session string) {
		t.Helper()
		found, status := manager.LookupSessionAffinity("openai", model, session)
		if status != "bound" || found == nil || found.ID != registered.ID {
			t.Fatalf("session %q binding = (%#v, %q)", session, found, status)
		}
	}

	for _, test := range []struct {
		name string
		err  *Error
	}{
		{name: "request scoped", err: NewRequestScopedError("caller request failed", http.StatusBadRequest)},
		{name: "caller attributed", err: &Error{Code: requestScopedErrorCode, HTTPStatus: http.StatusUnprocessableEntity, Message: "caller input"}},
		{name: "connection lifecycle", err: &Error{Code: connectionLifecycleErrorCode, Message: "unexpected EOF"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := "preserve-" + test.name
			options := bind(session)
			selector.cache.mu.RLock()
			key := "openai::header:" + session + "::" + model
			before := selector.cache.entries[key]
			selector.cache.mu.RUnlock()
			selector.OnResult(Result{
				AuthID: registered.ID, Provider: "openai", Model: model, Options: options, Error: test.err,
			})
			assertBound(session)
			selector.cache.mu.RLock()
			after := selector.cache.entries[key]
			selector.cache.mu.RUnlock()
			if !after.expiresAt.Equal(before.expiresAt) {
				t.Fatalf("availability-neutral failure touched TTL: before=%v after=%v", before.expiresAt, after.expiresAt)
			}
		})
	}

	requestOptions := bind("manager-request-scoped")
	manager.MarkResult(context.Background(), Result{
		AuthID: registered.ID, Provider: "openai", Model: model, Options: requestOptions,
		Error: NewRequestScopedError("bad caller request", http.StatusBadRequest),
	})
	assertBound("manager-request-scoped")

	credentialOptions := bind("manager-credential")
	manager.MarkResult(context.Background(), Result{
		AuthID: registered.ID, Provider: "openai", Model: model, Options: credentialOptions,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})
	if found, status := manager.LookupSessionAffinity("openai", model, "manager-credential"); status != "unbound" || found != nil {
		t.Fatalf("credential failure kept affinity: (%#v, %q)", found, status)
	}
}

type affinityResultPropagationExecutor struct {
	provider string
	calls    atomic.Int32
}

func (e *affinityResultPropagationExecutor) Identifier() string { return e.provider }

func (e *affinityResultPropagationExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e.calls.Add(1) == 1 {
		return cliproxyexecutor.Response{}, nil
	}
	return cliproxyexecutor.Response{}, &Error{
		HTTPStatus: http.StatusPaymentRequired,
		Message:    "payment required",
	}
}

func (*affinityResultPropagationExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{Message: "unexpected ExecuteStream call"}
}

func (*affinityResultPropagationExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, &Error{Message: "unexpected Refresh call"}
}

func (*affinityResultPropagationExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Message: "unexpected CountTokens call"}
}

func (*affinityResultPropagationExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, &Error{Message: "unexpected HttpRequest call"}
}

func TestRouteTracePreservesLCPAffinityIdentityForResultMutation(t *testing.T) {
	const (
		provider = "affinity-route-trace"
		model    = "affinity-route-trace-model"
		authID   = "affinity-route-trace-auth"
	)
	selector := NewSessionAffinitySelector(nil)
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	executor := &affinityResultPropagationExecutor{provider: provider}
	manager.RegisterExecutor(executor)
	registerSchedulerModels(t, provider, model, authID)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	payload := []byte(`{"messages":[{"role":"user","content":"stable affinity prompt"}]}`)
	options := cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    "openai",
		Metadata: map[string]any{
			cliproxyexecutor.CallerScopeMetadataKey:        "affinity-route-trace-caller",
			cliproxyexecutor.RouteTraceCallbackMetadataKey: func(cliproxyexecutor.RouteTraceEvent) {},
		},
	}
	if _, errExecute := manager.Execute(
		context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, options,
	); errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}

	turns := cliproxysession.ExtractCanonicalTurns(options.SourceFormat, payload)
	namespace := lcpAffinityNamespace("mixed", model, options.Metadata)
	match, ok := selector.matcher.Match(namespace, turns)
	if !ok || match.SessionID == "" || match.AuthID != authID {
		t.Fatalf("LCP binding after success = (%+v, %t)", match, ok)
	}
	if found, status := manager.LookupSessionAffinity(provider, model, match.SessionID); status != "bound" ||
		found == nil || found.ID != authID {
		t.Fatalf("LCP lookup before failure = (%#v, %q)", found, status)
	}

	if _, errExecute := manager.Execute(
		context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, options,
	); errExecute == nil {
		t.Fatal("credential-attributed Execute() error = nil")
	}
	if found, status := manager.LookupSessionAffinity(provider, model, match.SessionID); status != "unbound" || found != nil {
		t.Fatalf("credential failure kept route-traced LCP affinity: (%#v, %q)", found, status)
	}
}
