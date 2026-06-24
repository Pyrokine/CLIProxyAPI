package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type testHomeDispatcher struct {
	heartbeat bool
	payload   []byte
	err       error
	calls     int
}

func (d *testHomeDispatcher) HeartbeatOK() bool { return d.heartbeat }

func (d *testHomeDispatcher) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	d.calls++
	return d.payload, d.err
}

type testStatusError struct {
	status  int
	message string
}

func (e testStatusError) Error() string   { return e.message }
func (e testStatusError) StatusCode() int { return e.status }

type removeDuringPrepareExecutor struct {
	replaceAwareExecutor
	manager *Manager
}

func (e *removeDuringPrepareExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }

func (e *removeDuringPrepareExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	e.manager.Remove(ctx, auth.ID)
	updated := auth.Clone()
	updated.Metadata = map[string]any{"token": "prepared"}
	return updated, nil
}

type removeDuringSaveStore struct {
	manager        *Manager
	removeOnSave   atomic.Bool
	items          map[string]*Auth
	deleteCh       chan string
	removeAfterSeq atomic.Uint64
}

func (s *removeDuringSaveStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *removeDuringSaveStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if s.items == nil {
		s.items = make(map[string]*Auth)
	}
	if auth != nil {
		s.items[auth.ID] = auth.Clone()
	}
	if auth != nil && s.manager != nil && s.removeOnSave.Load() {
		s.removeOnSave.Store(false)
		go func(authID string) {
			s.manager.Remove(ctx, authID)
			if s.deleteCh != nil {
				s.deleteCh <- authID
			}
		}(auth.ID)
		deadline := time.Now().Add(time.Second)
		for {
			s.manager.mu.RLock()
			seq := s.manager.removedAuths[auth.ID]
			s.manager.mu.RUnlock()
			if seq > s.removeAfterSeq.Load() {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	return "", nil
}

func (s *removeDuringSaveStore) Delete(_ context.Context, id string) error {
	if s.items != nil {
		delete(s.items, id)
	}
	return nil
}

func TestManagerPickNextViaHomeRejectsRepeatedAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "codex"})

	payload, err := json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "home-index",
			Auth: Auth{
				ID:       "auth-1",
				Provider: "codex",
			},
		},
	)
	if err != nil {
		t.Fatalf("marshal dispatch response: %v", err)
	}

	dispatcher := &testHomeDispatcher{heartbeat: true, payload: payload}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })

	auth, executor, provider, err := manager.pickNextViaHome(
		context.Background(),
		"gpt-5",
		cliproxyexecutor.Options{},
		map[string]struct{}{"auth-1": {}},
	)
	if auth != nil || executor != nil || provider != "" {
		t.Fatalf("expected no auth/executor/provider, got %+v %T %q", auth, executor, provider)
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("expected auth error, got %T", err)
	}
	if authErr.Code != homeRequestRetryExceededErrorCode {
		t.Fatalf("error code = %q, want %q", authErr.Code, homeRequestRetryExceededErrorCode)
	}
	if dispatcher.calls != 1 {
		t.Fatalf("RPopAuth calls = %d, want 1", dispatcher.calls)
	}
}

func TestApplyAuthFailureStateTreatsCloudflareChallengeAsQuotaCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	now := time.Now()
	auth := &Auth{ID: "auth-1", Provider: "codex"}
	applyAuthFailureState(
		auth,
		&Error{HTTPStatus: http.StatusForbidden, Message: "cf-mitigated cloudflare challenge"},
		nil,
		now,
	)

	if auth.StatusMessage != "cloudflare challenge" {
		t.Fatalf("StatusMessage = %q, want cloudflare challenge", auth.StatusMessage)
	}
	if !auth.Quota.Exceeded || auth.Quota.Reason != "cloudflare challenge" {
		t.Fatalf("quota = %+v, want cloudflare challenge exceeded", auth.Quota)
	}
	if auth.NextRetryAfter.Sub(now) < 10*time.Second {
		t.Fatalf("NextRetryAfter = %v, want at least 10s after %v", auth.NextRetryAfter, now)
	}
	if auth.Quota.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want 1", auth.Quota.BackoffLevel)
	}
}

func TestManagerMarkResultTreatsModelCloudflareChallengeAsQuotaCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-1", Provider: "codex"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	before := time.Now()
	manager.MarkResult(
		context.Background(),
		Result{
			AuthID:   "auth-1",
			Provider: "codex",
			Model:    "gpt-5",
			Success:  false,
			Error: &Error{
				HTTPStatus: http.StatusForbidden,
				Message:    "challenge-platform from cloudflare",
			},
		},
	)

	updated, ok := manager.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	state := updated.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("expected model state")
	}
	if state.StatusMessage != "cloudflare challenge" {
		t.Fatalf("model StatusMessage = %q, want cloudflare challenge", state.StatusMessage)
	}
	if !state.Quota.Exceeded || state.Quota.Reason != "cloudflare challenge" {
		t.Fatalf("model quota = %+v, want cloudflare challenge exceeded", state.Quota)
	}
	if state.NextRetryAfter.Sub(before) < 10*time.Second {
		t.Fatalf("model NextRetryAfter = %v, want at least 10s after %v", state.NextRetryAfter, before)
	}
}

func TestIsRequestInvalidErrorIgnoresCloudflareChallenge(t *testing.T) {
	err := testStatusError{
		status: http.StatusBadRequest, message: "invalid_request_error: cloudflare <html challenge-platform",
	}
	if isRequestInvalidError(err) {
		t.Fatal("expected Cloudflare challenge error not to be treated as request invalid")
	}
}

func TestManagerRemoveDeletesRuntimeAuthAndClosesExecutorSessions(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "codex"}
	manager.RegisterExecutor(executor)

	registered, err := manager.Register(
		context.Background(),
		&Auth{
			ID:         "auth-1",
			Provider:   "codex",
			Attributes: map[string]string{"websockets": "true"},
			Metadata:   map[string]any{"token": "old"},
		},
	)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.rememberHomeRuntimeAuth("session-1", registered)
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "auth-1"); !ok {
		t.Fatal("expected runtime auth before remove")
	}

	manager.Remove(context.Background(), "auth-1")

	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected auth to be removed")
	}
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "auth-1"); ok {
		t.Fatal("expected runtime auth to be removed")
	}
	closed := executor.ClosedSessionIDs()
	if len(closed) != 1 || closed[0] != CloseAllExecutionSessionsID {
		t.Fatalf("closed sessions = %v, want [%s]", closed, CloseAllExecutionSessionsID)
	}

	updated, err := manager.Update(
		context.Background(),
		&Auth{ID: "auth-1", Provider: "codex", Metadata: map[string]any{"token": "new"}},
	)
	if err != nil {
		t.Fatalf("update removed auth: %v", err)
	}
	if updated != nil {
		t.Fatalf("expected update on removed auth to return nil, got %+v", updated)
	}
	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected update not to reinsert removed auth")
	}
}

func TestManagerRegisterDoesNotRecreateRemovedAuthWithoutExplicitRecreate(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-1", Provider: "codex"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.Remove(context.Background(), "auth-1")

	recreated, err := manager.Register(context.Background(), &Auth{ID: "auth-1", Provider: "codex"})
	if err != nil {
		t.Fatalf("register removed auth: %v", err)
	}
	if recreated != nil {
		t.Fatalf("expected stale register to return nil, got %+v", recreated)
	}
	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected stale register not to reinsert removed auth")
	}

	recreated, err = manager.Register(
		WithAllowAuthRecreate(context.Background()), &Auth{ID: "auth-1", Provider: "codex"},
	)
	if err != nil {
		t.Fatalf("explicit recreate auth: %v", err)
	}
	if recreated == nil {
		t.Fatal("expected explicit recreate to register auth")
	}
	if _, ok := manager.GetByID("auth-1"); !ok {
		t.Fatal("expected explicit recreate to reinsert auth")
	}
}

func TestManagerExplicitRecreateDoesNotOverrideNewRemove(t *testing.T) {
	store := &removeDuringSaveStore{deleteCh: make(chan string, 1)}
	manager := NewManager(store, nil, nil)
	store.manager = manager
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-1", Provider: "codex"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.Remove(context.Background(), "auth-1")
	manager.mu.RLock()
	store.removeAfterSeq.Store(manager.removedAuths["auth-1"])
	manager.mu.RUnlock()
	store.removeOnSave.Store(true)

	recreated, err := manager.Register(
		WithAllowAuthRecreate(context.Background()), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Metadata: map[string]any{"token": "new"},
		},
	)
	if err != nil {
		t.Fatalf("explicit recreate auth: %v", err)
	}
	if recreated != nil {
		t.Fatalf("expected recreate to return nil after new remove, got %+v", recreated)
	}
	select {
	case <-store.deleteCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for concurrent remove")
	}
	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected explicit recreate not to override a newer remove")
	}
	if store.items["auth-1"] != nil {
		t.Fatalf("expected store entry to be deleted after newer remove, got %+v", store.items["auth-1"])
	}
}

func TestManagerRemoveDeletesPureHomeRuntimeAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:         "runtime-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}
	manager.rememberHomeRuntimeAuth("session-1", auth)
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "runtime-auth"); !ok {
		t.Fatal("expected runtime auth before remove")
	}

	manager.Remove(context.Background(), "runtime-auth")

	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "runtime-auth"); ok {
		t.Fatal("expected pure runtime auth to be removed")
	}
	closed := executor.ClosedSessionIDs()
	if len(closed) != 1 || closed[0] != CloseAllExecutionSessionsID {
		t.Fatalf("closed sessions = %v, want [%s]", closed, CloseAllExecutionSessionsID)
	}
}

func TestManagerRememberHomeRuntimeAuthSkipsRemovedAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.Remove(context.Background(), "auth-1")
	manager.rememberHomeRuntimeAuth(
		"session-1", &Auth{
			ID:         "auth-1",
			Provider:   "codex",
			Attributes: map[string]string{"websockets": "true"},
		},
	)
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "auth-1"); ok {
		t.Fatal("expected removed auth not to be cached again")
	}
}

func TestManagerUpdateDoesNotRecreateAuthRemovedDuringPersist(t *testing.T) {
	store := &removeDuringSaveStore{deleteCh: make(chan string, 1)}
	manager := NewManager(store, nil, nil)
	store.manager = manager
	if _, err := manager.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Metadata: map[string]any{"token": "old"},
		},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	store.removeOnSave.Store(true)

	updated, err := manager.Update(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Metadata: map[string]any{"token": "new"},
		},
	)
	if err != nil {
		t.Fatalf("update auth: %v", err)
	}
	if updated != nil {
		t.Fatalf("expected update to return nil after remove, got %+v", updated)
	}
	select {
	case <-store.deleteCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for concurrent remove")
	}
	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected update not to reinsert auth removed during persist")
	}
	if store.items["auth-1"] != nil {
		t.Fatalf("expected store entry to be deleted after remove during update, got %+v", store.items["auth-1"])
	}
}

func TestManagerPrepareRequestAuthStopsWhenAuthRemovedDuringPrepare(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &removeDuringPrepareExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "codex"},
		manager:              manager,
	}
	if _, err := manager.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Metadata: map[string]any{"token": "old"},
		},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	auth, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("expected auth before prepare")
	}

	prepared, err := manager.prepareRequestAuth(context.Background(), executor, auth)
	if prepared == nil || prepared.ID != "auth-1" {
		t.Fatalf("prepared auth = %+v, want original auth context", prepared)
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("expected auth error, got %T", err)
	}
	if authErr.Code != "auth_removed" {
		t.Fatalf("error code = %q, want auth_removed", authErr.Code)
	}
	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected auth to remain removed")
	}
}

func TestSessionAffinitySelectorInvalidateAuth(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour},
	)
	defer selector.Stop()

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": {"session-1"}}}
	auths := []*Auth{{ID: "auth-1"}, {ID: "auth-2"}}
	selector.Remember("codex", opts, auths[0])
	if sticky := selector.PickSticky("codex", opts, auths); sticky == nil || sticky.ID != "auth-1" {
		t.Fatalf("sticky auth before invalidation = %+v, want auth-1", sticky)
	}

	selector.InvalidateAuth("auth-1")
	if sticky := selector.PickSticky("codex", opts, auths); sticky != nil {
		t.Fatalf("sticky auth after invalidation = %+v, want nil", sticky)
	}
}

func TestRepeatedHomeAuthErrorStatus(t *testing.T) {
	err := repeatedHomeAuthError()
	if err.Code != homeRequestRetryExceededErrorCode || err.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("repeatedHomeAuthError() = %s", fmt.Sprintf("%+v", err))
	}
}
