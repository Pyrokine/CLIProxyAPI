package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(
		&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait,
	)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(
		&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait,
	)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(
		&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait,
	)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

type credentialRetryLimitExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *credentialRetryLimitExecutor) Identifier() string {
	return e.id
}

func (e *credentialRetryLimitExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialRetryLimitExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *credentialRetryLimitExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *credentialRetryLimitExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func newCredentialRetryLimitTestManager(t *testing.T, maxRetryCredentials int) (
	*Manager,
	*credentialRetryLimitExecutor,
) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, maxRetryCredentials)

	executor := &credentialRetryLimitExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth1 := &Auth{ID: "auth-1", Provider: "claude"}
	auth2 := &Auth{ID: "auth-2", Provider: "claude"}

	// Register clients in the global ModelRegistry so pickNextMixed can find them.
	globalReg := registry.GetGlobalRegistry()
	testModel := []*registry.ModelInfo{{ID: "test-model"}}
	globalReg.RegisterClient("auth-1", "claude", testModel)
	globalReg.RegisterClient("auth-2", "claude", testModel)
	t.Cleanup(
		func() {
			globalReg.UnregisterClient("auth-1")
			globalReg.UnregisterClient("auth-2")
		},
	)

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	return m, executor
}

func TestManager_MaxRetryCredentials_LimitsCrossCredentialRetries(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	testCases := []struct {
		name   string
		invoke func(*Manager) error
	}{
		{
			name: "execute",
			invoke: func(m *Manager) error {
				_, errExecute := m.Execute(
					context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "execute_count",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteCount(
					context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "execute_stream",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteStream(
					context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
	}

	for _, tc := range testCases {
		t.Run(
			tc.name, func(t *testing.T) {
				limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1)
				if errInvoke := tc.invoke(limitedManager); errInvoke == nil {
					t.Fatalf("expected error for limited retry execution")
				}
				if calls := limitedExecutor.Calls(); calls != 1 {
					t.Fatalf("expected 1 call with max-retry-credentials=1, got %d", calls)
				}

				unlimitedManager, unlimitedExecutor := newCredentialRetryLimitTestManager(t, 0)
				if errInvoke := tc.invoke(unlimitedManager); errInvoke == nil {
					t.Fatalf("expected error for unlimited retry execution")
				}
				if calls := unlimitedExecutor.Calls(); calls != 2 {
					t.Fatalf("expected 2 calls with max-retry-credentials=0, got %d", calls)
				}
			},
		)
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(
		context.Background(), Result{
			AuthID:   "auth-1",
			Provider: "claude",
			Model:    model,
			Success:  false,
			Error:    &Error{HTTPStatus: 500, Message: "boom"},
		},
	)

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_CloudflareChallenge_On403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-cf-403",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-cf-403"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	m.MarkResult(
		context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "claude",
			Model:    model,
			Success:  false,
			Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "cf-mitigated: challenge"},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be non-zero for cloudflare challenge")
	}
	diff := time.Until(state.NextRetryAfter)
	if diff < 5*time.Second || diff > 25*time.Second {
		t.Fatalf("expected NextRetryAfter to be ~10 seconds, got %v", diff)
	}
	if state.StatusMessage != "cloudflare challenge" {
		t.Fatalf("expected StatusMessage to be 'cloudflare challenge', got %s", state.StatusMessage)
	}
	if !state.Quota.Exceeded || state.Quota.Reason != "cloudflare challenge" {
		t.Fatalf("expected cloudflare challenge quota state, got %+v", state.Quota)
	}
	available := false
	for _, info := range reg.GetAvailableModelsByProvider("claude") {
		if info != nil && info.ID == model {
			available = true
			break
		}
	}
	if !available {
		t.Fatalf("expected model to remain available for cloudflare challenge transient cooldown")
	}
}

func TestManager_MarkResult_CloudflareChallenge_AuthLevelFailure(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-cf-auth", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	m.MarkResult(
		context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "claude",
			Success:  false,
			Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "cloudflare challenge"},
		},
	)

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.StatusMessage != "cloudflare challenge" {
		t.Fatalf("expected auth StatusMessage to be cloudflare challenge, got %q", updated.StatusMessage)
	}
	if updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected auth NextRetryAfter to be non-zero")
	}
	if !updated.Quota.Exceeded || updated.Quota.Reason != "cloudflare challenge" {
		t.Fatalf("expected auth cloudflare quota state, got %+v", updated.Quota)
	}
}

func TestIsCloudflareChallengeErrorMessage_NarrowChallengeMatch(t *testing.T) {
	t.Parallel()

	if isCloudflareChallengeErrorMessage("generic challenge failed") {
		t.Fatal("generic challenge message should not be treated as cloudflare challenge")
	}
	if !isCloudflareChallengeErrorMessage("cf-mitigated: challenge") {
		t.Fatal("cf-mitigated message should be treated as cloudflare challenge")
	}
}

func TestManager_PickNext_DisallowsFreeCodexAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(&credentialRetryLimitExecutor{id: "codex"})
	freeAuth := &Auth{ID: "a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}
	paidAuth := &Auth{ID: "b-paid", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}
	if _, errRegister := m.Register(context.Background(), freeAuth); errRegister != nil {
		t.Fatalf("register free auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), paidAuth); errRegister != nil {
		t.Fatalf("register paid auth: %v", errRegister)
	}

	selected, _, errPick := m.PickNext(
		context.Background(), "codex", "", cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true},
		}, nil,
	)
	if errPick != nil {
		t.Fatalf("PickNext() error = %v", errPick)
	}
	if selected == nil || selected.ID != paidAuth.ID {
		t.Fatalf("PickNext() auth = %#v, want %s", selected, paidAuth.ID)
	}
}

func TestManager_PickNext_DisallowsFreeCodexAuthForCustomSelector(t *testing.T) {
	selector := &trackingSelector{}
	m := NewManager(nil, selector, nil)
	m.RegisterExecutor(&credentialRetryLimitExecutor{id: "codex"})
	freeAuth := &Auth{ID: "a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}
	paidAuth := &Auth{ID: "b-paid", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}}
	if _, errRegister := m.Register(context.Background(), freeAuth); errRegister != nil {
		t.Fatalf("register free auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), paidAuth); errRegister != nil {
		t.Fatalf("register paid auth: %v", errRegister)
	}

	selected, _, errPick := m.PickNext(
		context.Background(), "codex", "", cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true},
		}, nil,
	)
	if errPick != nil {
		t.Fatalf("PickNext() error = %v", errPick)
	}
	if selected == nil || selected.ID != paidAuth.ID {
		t.Fatalf("PickNext() auth = %#v, want %s", selected, paidAuth.ID)
	}
	if len(selector.lastAuthID) != 1 || selector.lastAuthID[0] != paidAuth.ID {
		t.Fatalf("selector candidates = %#v, want only %s", selector.lastAuthID, paidAuth.ID)
	}
}

func TestManager_PickNextMixed_DisallowsFreeCodexAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(&credentialRetryLimitExecutor{id: "codex"})
	m.RegisterExecutor(&credentialRetryLimitExecutor{id: "claude"})
	freeAuth := &Auth{ID: "a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}
	paidAuth := &Auth{ID: "b-paid", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), freeAuth); errRegister != nil {
		t.Fatalf("register free auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), paidAuth); errRegister != nil {
		t.Fatalf("register paid auth: %v", errRegister)
	}

	selected, _, provider, errPick := m.pickNextMixed(
		context.Background(), []string{"codex", "claude"}, "", cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true},
		}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want claude", provider)
	}
	if selected == nil || selected.ID != paidAuth.ID {
		t.Fatalf("pickNextMixed() auth = %#v, want %s", selected, paidAuth.ID)
	}
}
