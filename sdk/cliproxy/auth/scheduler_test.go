package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type schedulerTestExecutor struct{}

func (schedulerTestExecutor) Identifier() string { return "test" }

func (schedulerTestExecutor) Execute(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) ExecuteStream(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (schedulerTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (schedulerTestExecutor) CountTokens(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) HttpRequest(_ context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	return nil, nil
}

type trackingSelector struct {
	calls      int
	lastAuthID []string
}

func (s *trackingSelector) Pick(
	_ context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	s.calls++
	s.lastAuthID = s.lastAuthID[:0]
	for _, auth := range auths {
		s.lastAuthID = append(s.lastAuthID, auth.ID)
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[len(auths)-1], nil
}

func newSchedulerForTest(selector Selector, auths ...*Auth) *authScheduler {
	scheduler := newAuthScheduler(selector)
	scheduler.rebuild(auths)
	return scheduler
}

func registerSchedulerModels(t *testing.T, provider string, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(
		func() {
			for _, authID := range authIDs {
				reg.UnregisterClient(authID)
			}
		},
	)
}

func TestSchedulerPick_RoundRobinHighestPriority(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "high-b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "high-a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
	)

	want := []string{"high-a", "high-b", "high-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_PreservesCursorAcrossIndexRebuild(t *testing.T) {
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
	)

	first, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("first pickSingle() error = %v", errPick)
	}
	if first == nil || first.ID != "a" {
		t.Fatalf("first auth = %#v, want a", first)
	}

	scheduler.upsertAuth(
		&Auth{
			ID: "b", Provider: "gemini", Attributes: map[string]string{"priority": "10", "note": "updated"},
		},
	)

	second, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("second pickSingle() error = %v", errPick)
	}
	if second == nil || second.ID != "b" {
		t.Fatalf("second auth = %#v, want b", second)
	}
}

func TestSchedulerPick_FillFirstSticksToFirstReady(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "b", Provider: "gemini"},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "c", Provider: "gemini"},
	)

	for index := range 3 {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != "a" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, "a")
		}
	}
}

func TestSchedulerPick_PromotesExpiredCooldownBeforePick(t *testing.T) {
	t.Parallel()

	model := "gemini-2.5-pro"
	registerSchedulerModels(t, "gemini", model, "cooldown-expired")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       "cooldown-expired",
			Provider: "gemini",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(-1 * time.Second),
				},
			},
		},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() auth = nil")
	}
	if got.ID != "cooldown-expired" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "cooldown-expired")
	}
}

func TestSchedulerPick_TransientUnavailabilityIsNotQuotaCooldown(t *testing.T) {
	t.Parallel()

	const (
		provider = "gemini"
		model    = "gemini-2.5-pro"
		authID   = "transient-unavailable"
	)
	registerSchedulerModels(t, provider, model, authID)
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       authID,
			Provider: provider,
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(time.Minute),
				},
			},
		},
	)

	picked, errPick := scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if picked != nil || errPick == nil {
		t.Fatalf("pickSingle() = (%#v, %v), want no auth and an error", picked, errPick)
	}
	if _, ok := errors.AsType[*modelCooldownError](errPick); ok {
		t.Fatalf("pickSingle() error = %T, do not report a transient error as quota cooldown", errPick)
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr.Code != "auth_unavailable" ||
		authErr.HTTPStatus != http.StatusServiceUnavailable || !authErr.Retryable {
		t.Fatalf("pickSingle() error = %#v, want retryable 503 auth_unavailable", errPick)
	}
	if SafeResponseHeaders(errPick).Get("Retry-After") == "" {
		t.Fatalf("transient auth_unavailable is missing Retry-After: %v", errPick)
	}
}

func TestUnavailableRetryAfterRemainsGenericWithBlockedCandidates(t *testing.T) {
	const (
		provider = "gemini"
		model    = "test-retry-after-model"
	)
	registerSchedulerModels(t, provider, model, "temporarily-blocked", "disabled-blocked")
	deadline := time.Now().Add(time.Minute)
	candidates := []*Auth{
		{
			ID: "temporarily-blocked", Provider: provider,
			ModelStates: map[string]*ModelState{
				model: {Status: StatusError, Unavailable: true, NextRetryAfter: deadline},
			},
		},
		{ID: "disabled-blocked", Provider: provider, Disabled: true, Status: StatusDisabled},
	}
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, candidates...)
	_, schedulerErr := scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	_, selectorErr := getAvailableAuths(candidates, provider, model, cliproxyexecutor.Options{}, time.Now())
	for _, err := range []error{schedulerErr, selectorErr} {
		if err == nil {
			t.Fatal("blocked selection returned no error")
		}
		if _, isCooldown := errors.AsType[*modelCooldownError](err); isCooldown {
			t.Fatalf("mixed blocked candidates were reported as model_cooldown: %v", err)
		}
		var authErr *Error
		if !errors.As(err, &authErr) || authErr.Code != "auth_unavailable" ||
			authErr.HTTPStatus != http.StatusServiceUnavailable || !authErr.Retryable {
			t.Fatalf("blocked selection error = %v, want retryable 503 auth_unavailable", err)
		}
		if SafeResponseHeaders(err).Get("Retry-After") == "" {
			t.Fatalf("blocked selection omitted local retry deadline: %v", err)
		}
	}
}

func TestAuthUnavailableSafeHeadersUseOnlyLocalDeadline(t *testing.T) {
	now := time.Now()
	cause := errors.New("upstream Retry-After: 999")
	err := newAuthUnavailableErrorWithCause(now.Add(1500*time.Millisecond), now, cause)
	wrapped := errors.Join(errors.New("selection failed"), err)
	if got := SafeResponseHeaders(wrapped).Get("Retry-After"); got != "2" {
		t.Fatalf("safe Retry-After = %q, want 2", got)
	}
	if got := SafeResponseHeaders(wrapped); len(got) != 1 {
		t.Fatalf("safe response headers = %v, want only Retry-After", got)
	}
	if SafeResponseHeaders(newAuthUnavailableErrorWithCause(now, now, cause)) != nil {
		t.Fatal("deadline-free selection manufactured Retry-After")
	}
	var authErr *Error
	if !errors.As(wrapped, &authErr) || authErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("wrapped error lost auth status: %v", wrapped)
	}
}

func TestSchedulerPick_GeminiVirtualParentUsesTwoLevelRotation(t *testing.T) {
	t.Parallel()

	registerSchedulerModels(
		t, "gemini-cli", "gemini-2.5-pro", "cred-a::proj-1", "cred-a::proj-2", "cred-b::proj-1", "cred-b::proj-2",
	)
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID: "cred-a::proj-1", Provider: "gemini-cli",
			Attributes: map[string]string{"gemini_virtual_parent": "cred-a"},
		},
		&Auth{
			ID: "cred-a::proj-2", Provider: "gemini-cli",
			Attributes: map[string]string{"gemini_virtual_parent": "cred-a"},
		},
		&Auth{
			ID: "cred-b::proj-1", Provider: "gemini-cli",
			Attributes: map[string]string{"gemini_virtual_parent": "cred-b"},
		},
		&Auth{
			ID: "cred-b::proj-2", Provider: "gemini-cli",
			Attributes: map[string]string{"gemini_virtual_parent": "cred-b"},
		},
	)

	wantParents := []string{"cred-a", "cred-b", "cred-a", "cred-b"}
	wantIDs := []string{"cred-a::proj-1", "cred-b::proj-1", "cred-a::proj-2", "cred-b::proj-2"}
	for index := range wantIDs {
		got, errPick := scheduler.pickSingle(
			context.Background(), "gemini-cli", "gemini-2.5-pro", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
		if got.Attributes["gemini_virtual_parent"] != wantParents[index] {
			t.Fatalf(
				"pickSingle() #%d parent = %q, want %q", index, got.Attributes["gemini_virtual_parent"],
				wantParents[index],
			)
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex"},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_XAIWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "xai-http", Provider: "xai"},
		&Auth{ID: "xai-ws-a", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "xai-ws-b", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"xai-ws-a", "xai-ws-b", "xai-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "xai", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketAcrossPriorityTiers(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "codex-ws", Provider: "codex", Attributes: map[string]string{"priority": "1", "websockets": "true"}},
	)

	got, errPick := scheduler.pickSingle(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()), "codex", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickSingle() auth = nil")
	}
	if got.ID != "codex-ws" {
		t.Fatalf("pickSingle() auth.ID = %q, want codex-ws", got.ID)
	}
}

func TestSchedulerPick_MixedCodexWebsocketPrefersWebsocketAcrossPriorityTiers(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "codex-ws", Provider: "codex", Attributes: map[string]string{"priority": "1", "websockets": "true"}},
	)

	got, provider, errPick := scheduler.pickMixed(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()), []string{"codex"}, "",
		cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickMixed() auth = nil")
	}
	if provider != "codex" {
		t.Fatalf("pickMixed() provider = %q, want codex", provider)
	}
	if got.ID != "codex-ws" {
		t.Fatalf("pickMixed() auth.ID = %q, want codex-ws", got.ID)
	}
}

func TestSchedulerPick_MixedXAIWebsocketPrefersWebsocketAcrossPriorityTiers(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "xai-http", Provider: "xai", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "xai-ws", Provider: "xai", Attributes: map[string]string{"priority": "1", "websockets": "true"}},
	)

	got, provider, errPick := scheduler.pickMixed(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()), []string{"xai"}, "",
		cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickMixed() auth = nil")
	}
	if provider != "xai" {
		t.Fatalf("pickMixed() provider = %q, want xai", provider)
	}
	if got.ID != "xai-ws" {
		t.Fatalf("pickMixed() auth.ID = %q, want xai-ws", got.ID)
	}
}

func TestSchedulerPick_MixedProvidersUsesProviderRotationOverReadyCandidates(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini"},
		&Auth{ID: "gemini-b", Provider: "gemini"},
		&Auth{ID: "claude-a", Provider: "claude"},
	)

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(
			context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestSchedulerPick_MixedProvidersPrefersHighestPriorityTier(t *testing.T) {
	t.Parallel()

	model := "gpt-default"
	registerSchedulerModels(t, "provider-low", model, "low")
	registerSchedulerModels(t, "provider-high-a", model, "high-a")
	registerSchedulerModels(t, "provider-high-b", model, "high-b")

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "provider-low", Attributes: map[string]string{"priority": "4"}},
		&Auth{ID: "high-a", Provider: "provider-high-a", Attributes: map[string]string{"priority": "7"}},
		&Auth{ID: "high-b", Provider: "provider-high-b", Attributes: map[string]string{"priority": "7"}},
	)

	providers := []string{"provider-low", "provider-high-a", "provider-high-b"}
	wantProviders := []string{"provider-high-a", "provider-high-b", "provider-high-a", "provider-high-b"}
	wantIDs := []string{"high-a", "high-b", "high-a", "high-b"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(
			context.Background(), providers, model, cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_UsesProviderRotationBeforeCredentialRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"},
	); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"},
	); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "claude-a", Provider: "claude"},
	); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	// pickNextMixed builds candidates from m.auths (map, non-deterministic order),
	// then passes them to the selector (RoundRobinSelector). Verify that all registered
	// auth entries are visited and each pick returns a valid provider.
	validIDs := map[string]bool{"gemini-a": true, "gemini-b": true, "claude-a": true}
	validProviders := map[string]bool{"gemini": true, "claude": true}
	seenIDs := make(map[string]int)
	for index := range 4 {
		got, _, provider, errPick := manager.pickNextMixed(
			context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, map[string]struct{}{},
		)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if !validIDs[got.ID] {
			t.Fatalf("pickNextMixed() #%d unexpected auth.ID = %q", index, got.ID)
		}
		if !validProviders[provider] {
			t.Fatalf("pickNextMixed() #%d unexpected provider = %q", index, provider)
		}
		if provider != got.Provider {
			t.Fatalf(
				"pickNextMixed() #%d provider mismatch: returned %q but auth.Provider = %q", index, provider,
				got.Provider,
			)
		}
		seenIDs[got.ID]++
	}
	// Over 4 picks with 3 candidates, all should be visited at least once
	for id := range validIDs {
		if seenIDs[id] == 0 {
			t.Fatalf("expected auth %q to be picked at least once, seen: %v", id, seenIDs)
		}
	}
}

func TestManagerCustomSelector_FallsBackToLegacyPath(t *testing.T) {
	t.Parallel()

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["auth-a"] = &Auth{ID: "auth-a", Provider: "gemini"}
	manager.auths["auth-b"] = &Auth{ID: "auth-b", Provider: "gemini"}

	got, _, _, errPick := manager.pickNextMixed(
		context.Background(), []string{"gemini"}, "", cliproxyexecutor.Options{}, map[string]struct{}{},
	)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if selector.calls != 1 {
		t.Fatalf("selector.calls = %d, want %d", selector.calls, 1)
	}
	if len(selector.lastAuthID) != 2 {
		t.Fatalf("len(selector.lastAuthID) = %d, want %d", len(selector.lastAuthID), 2)
	}
	if got.ID != selector.lastAuthID[len(selector.lastAuthID)-1] {
		t.Fatalf(
			"pickNext() auth.ID = %q, want selector-picked %q", got.ID, selector.lastAuthID[len(selector.lastAuthID)-1],
		)
	}
}

func TestManager_PickNextMixed_UsesSchedulerRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"},
	); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"},
	); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "claude-a", Provider: "claude"},
	); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	// pickNextMixed builds candidates from m.auths map (non-deterministic order)
	// and uses the selector for round-robin. Verify all auth entries are visited
	// and rotation occurs (not always same auth).
	validIDs := map[string]bool{"gemini-a": true, "gemini-b": true, "claude-a": true}
	seenIDs := make(map[string]int)
	for index := range 4 {
		got, _, provider, errPick := manager.pickNextMixed(
			context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if !validIDs[got.ID] {
			t.Fatalf("pickNextMixed() #%d unexpected auth.ID = %q", index, got.ID)
		}
		if provider != got.Provider {
			t.Fatalf(
				"pickNextMixed() #%d provider mismatch: returned %q but auth.Provider = %q", index, provider,
				got.Provider,
			)
		}
		seenIDs[got.ID]++
	}
	// All 3 candidates should be visited at least once across 4 picks
	for id := range validIDs {
		if seenIDs[id] == 0 {
			t.Fatalf("expected auth %q to be picked at least once, seen: %v", id, seenIDs)
		}
	}
}

func TestManager_PickNextMixed_SkipsProvidersWithoutExecutors(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"},
	); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: "claude-a", Provider: "claude"},
	); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	got, _, provider, errPick := manager.pickNextMixed(
		context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "claude-a")
	}
}
