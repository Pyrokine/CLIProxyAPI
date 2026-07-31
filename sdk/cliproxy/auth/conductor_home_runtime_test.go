package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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

type opaqueUnauthorizedRefreshExecutor struct {
	replaceAwareExecutor
	refreshCalls int
	unchanged    bool
	refreshErr   error
	started      chan struct{}
	release      chan struct{}
}

type opaqueRuntimeStorage struct {
	value string
}

type testRefreshCommitGuard struct {
	calls atomic.Int32
}

func (*testRefreshCommitGuard) Current() bool { return true }

func (g *testRefreshCommitGuard) CommitRefresh(commit func() bool) bool {
	g.calls.Add(1)
	return commit()
}

type refreshCommitStore struct {
	mu        sync.Mutex
	saved     *Auth
	saveCalls int
	entered   chan struct{}
	release   chan struct{}
}

type cancelAfterSaveStore struct {
	cancel context.CancelFunc
	saved  *Auth
}

type blockingRefreshRollbackStore struct {
	mu              sync.Mutex
	saved           *Auth
	saveCalls       int
	deleteCalls     int
	deleteFailures  int
	deleteOnce      sync.Once
	rollbackEntered chan struct{}
	rollbackRelease chan struct{}
	deleted         chan struct{}
}

func (*refreshCommitStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *refreshCommitStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.saveCalls++
	call := s.saveCalls
	s.mu.Unlock()
	if call == 1 && s.entered != nil {
		close(s.entered)
	}
	if call == 1 && s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	s.saved = auth.Clone()
	s.mu.Unlock()
	return "", nil
}

func (*refreshCommitStore) Delete(context.Context, string) error { return nil }

func (*cancelAfterSaveStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *cancelAfterSaveStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saved = auth.Clone()
	if s.cancel != nil {
		s.cancel()
	}
	return auth.ID, nil
}

func (*cancelAfterSaveStore) Delete(context.Context, string) error { return nil }

func (s *blockingRefreshRollbackStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		return nil, nil
	}
	return []*Auth{s.saved.Clone()}, nil
}

func (s *blockingRefreshRollbackStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.saveCalls++
	call := s.saveCalls
	s.mu.Unlock()
	if call == 2 {
		close(s.rollbackEntered)
		<-s.rollbackRelease
	}
	s.mu.Lock()
	s.saved = auth.Clone()
	s.mu.Unlock()
	return "", nil
}

func (s *blockingRefreshRollbackStore) Delete(context.Context, string) error {
	s.mu.Lock()
	s.deleteCalls++
	if s.deleteFailures > 0 {
		s.deleteFailures--
		s.mu.Unlock()
		return errors.New("delete failed")
	}
	s.saved = nil
	s.mu.Unlock()
	if s.deleted != nil {
		s.deleteOnce.Do(func() { close(s.deleted) })
	}
	return nil
}

type rejectedPublicationStore struct {
	manager        *Manager
	mu             sync.Mutex
	saved          *Auth
	replacement    *Auth
	trigger        atomic.Bool
	removeDone     chan struct{}
	removeAfterSeq atomic.Uint64
	deleteCalls    int
}

func (*rejectedPublicationStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *rejectedPublicationStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.saved = auth.Clone()
	s.mu.Unlock()
	if auth != nil && s.manager != nil && s.trigger.CompareAndSwap(true, false) {
		go func(id string) {
			_ = s.manager.Remove(WithSkipPersist(context.Background()), id)
			close(s.removeDone)
		}(auth.ID)
		deadline := time.Now().Add(time.Second)
		for {
			s.manager.mu.RLock()
			seq, removed := s.manager.removedAuths[auth.ID]
			s.manager.mu.RUnlock()
			if removed && seq > s.removeAfterSeq.Load() || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		s.mu.Lock()
		s.saved = s.replacement.Clone()
		s.mu.Unlock()
	}
	return auth.ID, nil
}

func (*rejectedPublicationStore) DeleteIfVersion(
	context.Context,
	string,
	PersistedVersion,
) (bool, error) {
	return false, nil
}

func (s *rejectedPublicationStore) Delete(context.Context, string) error {
	s.mu.Lock()
	s.deleteCalls++
	s.saved = nil
	s.mu.Unlock()
	return nil
}

type versionedDeletionStore struct {
	mu          sync.Mutex
	saved       *Auth
	deleteCalls int
}

func (s *versionedDeletionStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		return nil, nil
	}
	return []*Auth{s.saved.Clone()}, nil
}

func (s *versionedDeletionStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.saved = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (*versionedDeletionStore) DeleteIfVersion(
	context.Context,
	string,
	PersistedVersion,
) (bool, error) {
	return false, nil
}

func (s *versionedDeletionStore) Delete(context.Context, string) error {
	s.mu.Lock()
	s.deleteCalls++
	s.saved = nil
	s.mu.Unlock()
	return nil
}

type guardedRefreshExecutor struct {
	replaceAwareExecutor
	guard RefreshCommitGuard
}

type rejectingAfterCommitGuard struct{}

func (rejectingAfterCommitGuard) Current() bool { return true }

func (rejectingAfterCommitGuard) CommitRefresh(commit func() bool) bool {
	_ = commit()
	return false
}

func (e *guardedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	updated := auth.Clone()
	updated.Metadata = map[string]any{"access_token": "new"}
	updated.RefreshCommit = e.guard
	return updated, nil
}

func (*opaqueRuntimeStorage) SaveTokenToFile(string) error { return nil }

func (e *opaqueUnauthorizedRefreshExecutor) ShouldRefreshAfterUnauthorized(*Auth) bool {
	return true
}

func (e *opaqueUnauthorizedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls++
	if e.started != nil {
		close(e.started)
	}
	if e.release != nil {
		<-e.release
	}
	updated := auth.Clone()
	if e.refreshErr != nil {
		return updated, e.refreshErr
	}
	if e.unchanged {
		return updated, nil
	}
	updated.Metadata = map[string]any{"provider_refresh_state": "new"}
	return updated, nil
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

func TestManagerPickNextViaHomeKeepsPinnedHTTPFallbackAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	manager.rememberHomeRuntimeAuth(
		"session-1",
		&Auth{
			ID:         "auth-1",
			Provider:   "codex",
			Attributes: map[string]string{"websockets": "false"},
		},
	)

	dispatcher := &testHomeDispatcher{heartbeat: true}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	auth, gotExecutor, provider, err := manager.pickNextViaHome(
		ctx,
		"gpt-5",
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			cliproxyexecutor.PinnedAuthMetadataKey:       "auth-1",
			homeAuthCountMetadataKey:                     2,
		}},
		nil,
	)
	if err != nil {
		t.Fatalf("pickNextViaHome() error = %v", err)
	}
	if auth == nil || auth.ID != "auth-1" || gotExecutor != executor || provider != "codex" {
		t.Fatalf("pickNextViaHome() = %#v %T %q", auth, gotExecutor, provider)
	}
	if dispatcher.calls != 0 {
		t.Fatalf("RPopAuth calls = %d, want 0", dispatcher.calls)
	}
}

func TestManagerPickNextViaHomeDoesNotRedispatchPinnedHTTPAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "codex"})

	dispatcher := &testHomeDispatcher{heartbeat: true}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })

	_, _, _, err := manager.pickNextViaHome(
		context.Background(),
		"gpt-5",
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: "auth-1",
		}},
		map[string]struct{}{"auth-1": {}},
	)
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != homeRequestRetryExceededErrorCode {
		t.Fatalf("pickNextViaHome() error = %#v, want %s", err, homeRequestRetryExceededErrorCode)
	}
	if dispatcher.calls != 0 {
		t.Fatalf("RPopAuth calls = %d, want 0", dispatcher.calls)
	}
}

func TestManagerPickNextViaHomeDoesNotFailOverPinnedAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "codex"})
	manager.rememberHomeRuntimeAuth(
		"session-1",
		&Auth{
			ID:         "auth-1",
			Provider:   "codex",
			Attributes: map[string]string{"websockets": "true"},
		},
	)

	dispatcher := &testHomeDispatcher{heartbeat: true}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	_, _, _, err := manager.pickNextViaHome(
		ctx,
		"gpt-5",
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			cliproxyexecutor.PinnedAuthMetadataKey:       "auth-1",
			homeAuthCountMetadataKey:                     2,
		}},
		map[string]struct{}{"auth-1": {}},
	)
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != homeRequestRetryExceededErrorCode {
		t.Fatalf("pickNextViaHome() error = %#v, want %s", err, homeRequestRetryExceededErrorCode)
	}
	if dispatcher.calls != 0 {
		t.Fatalf("RPopAuth calls = %d, want 0", dispatcher.calls)
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

type runtimeResultHook struct {
	results chan Result
}

func (*runtimeResultHook) OnAuthRegistered(context.Context, *Auth) {}
func (*runtimeResultHook) OnAuthUpdated(context.Context, *Auth)    {}

func (h *runtimeResultHook) OnResult(_ context.Context, result Result) {
	h.results <- result
}

type runtimeIndexRefreshExecutor struct {
	schedulerTestExecutor
	called bool
}

type unchangedRuntimeRefreshExecutor struct {
	schedulerTestExecutor
	called bool
}

func (*runtimeIndexRefreshExecutor) Identifier() string { return "codex" }

func (*unchangedRuntimeRefreshExecutor) Identifier() string { return "plugin-provider" }

func (e *unchangedRuntimeRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.called = true
	return auth.Clone(), nil
}

type testHomeRuntimeRefreshClient struct {
	heartbeat bool
	payload   []byte
	index     string
}

func (c *testHomeRuntimeRefreshClient) HeartbeatOK() bool { return c.heartbeat }

func (c *testHomeRuntimeRefreshClient) GetRefreshAuth(
	_ context.Context,
	authIndex string,
) ([]byte, error) {
	c.index = authIndex
	return c.payload, nil
}

func (e *runtimeIndexRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.called = true
	updated := auth.Clone()
	updated.Metadata = map[string]any{"access_token": "refreshed"}
	return updated, nil
}

func TestWrapStreamResultMarksRuntimeSuccessAsRuntimeOnly(t *testing.T) {
	hook := &runtimeResultHook{results: make(chan Result, 1)}
	manager := NewManager(nil, nil, hook)
	remaining := make(chan cliproxyexecutor.StreamChunk)
	close(remaining)

	result := manager.wrapStreamResult(
		context.Background(),
		&Auth{ID: "home-auth", Provider: "codex", Generation: 0},
		"codex",
		"gpt-5",
		nil,
		nil,
		remaining,
		OAuthModelAliasResult{},
	)
	for range result.Chunks {
	}

	select {
	case marked := <-hook.results:
		if !marked.Success || !marked.RuntimeOnly || marked.AuthID != "home-auth" {
			t.Fatalf("stream result = %#v, want successful runtime-only result", marked)
		}
	case <-time.After(time.Second):
		t.Fatal("stream success result was not reported")
	}
}

func TestWrapStreamResultStopsAfterTerminalError(t *testing.T) {
	hook := &runtimeResultHook{results: make(chan Result, 1)}
	manager := NewManager(nil, nil, hook)
	remaining := make(chan cliproxyexecutor.StreamChunk, 2)
	remaining <- cliproxyexecutor.StreamChunk{Err: errors.New("terminal boom")}
	remaining <- cliproxyexecutor.StreamChunk{Payload: []byte("late success")}
	close(remaining)

	result := manager.wrapStreamResult(
		context.Background(),
		&Auth{ID: "home-auth", Provider: "codex", Generation: 0},
		"codex",
		"gpt-5",
		nil,
		nil,
		remaining,
		OAuthModelAliasResult{},
	)
	chunks := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for chunk := range result.Chunks {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || chunks[0].Err.Error() != "terminal boom" {
		t.Fatalf("stream chunks = %#v, want one terminal error", chunks)
	}
	select {
	case marked := <-hook.results:
		if marked.Success || marked.Error == nil {
			t.Fatalf("stream result = %#v, want failure", marked)
		}
	case <-time.After(time.Second):
		t.Fatal("stream failure result was not reported")
	}
}

func TestTryRefreshAfterUnauthorizedUsesRuntimeAuthIndex(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &runtimeIndexRefreshExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:         "home-auth",
		Index:      "home-index",
		Provider:   "codex",
		Generation: 0,
		Metadata:   map[string]any{"access_token": "expired"},
	}

	refreshed, ok := manager.tryRefreshAfterUnauthorized(
		context.Background(),
		auth,
		executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		false,
	)
	if !ok || !executor.called {
		t.Fatalf("tryRefreshAfterUnauthorized() = (%#v, %v), refresh called = %v", refreshed, ok, executor.called)
	}
	if refreshed == nil || refreshed.Generation != 0 || authAccessToken(refreshed) != "refreshed" {
		t.Fatalf("refreshed auth = %#v", refreshed)
	}
}

func TestTryRefreshAfterUnauthorizedRefreshesRuntimeAuthThroughHome(t *testing.T) {
	payload, errMarshal := json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "returned-index",
			Auth: Auth{
				ID:       "ignored-id",
				Provider: "plugin-provider",
				Metadata: map[string]any{"access_token": "refreshed"},
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal Home refresh payload: %v", errMarshal)
	}
	client := &testHomeRuntimeRefreshClient{heartbeat: true, payload: payload}
	previous := currentHomeRuntimeRefreshClient
	currentHomeRuntimeRefreshClient = func() homeRuntimeRefreshClient { return client }
	t.Cleanup(func() { currentHomeRuntimeRefreshClient = previous })

	manager := NewManager(nil, nil, nil)
	executor := &unchangedRuntimeRefreshExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:         "home-auth",
		Index:      "home-index",
		Provider:   "plugin-provider",
		Generation: 0,
		Metadata:   map[string]any{"access_token": "expired"},
	}
	refreshed, ok := manager.tryRefreshAfterUnauthorized(
		context.Background(),
		auth,
		executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		false,
	)
	if !ok || refreshed == nil {
		t.Fatalf("tryRefreshAfterUnauthorized() = (%#v, %v)", refreshed, ok)
	}
	if executor.called {
		t.Fatal("executor refresh was called after Home returned a refreshed credential")
	}
	if client.index != "home-index" || refreshed.ID != "home-auth" ||
		refreshed.Index != "returned-index" || authAccessToken(refreshed) != "refreshed" ||
		refreshed.Generation != 0 {
		t.Fatalf("refreshed auth = %#v, requested index = %q", refreshed, client.index)
	}
}

func TestTryRefreshAfterUnauthorizedRejectsUnchangedHomeCredential(t *testing.T) {
	payload, errMarshal := json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "home-index",
			Auth: Auth{
				Provider: "plugin-provider",
				Metadata: map[string]any{"access_token": "expired"},
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal Home refresh payload: %v", errMarshal)
	}
	client := &testHomeRuntimeRefreshClient{heartbeat: true, payload: payload}
	previous := currentHomeRuntimeRefreshClient
	currentHomeRuntimeRefreshClient = func() homeRuntimeRefreshClient { return client }
	t.Cleanup(func() { currentHomeRuntimeRefreshClient = previous })

	manager := NewManager(nil, nil, nil)
	executor := &unchangedRuntimeRefreshExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:         "home-auth",
		Index:      "home-index",
		Provider:   "plugin-provider",
		Generation: 0,
		Metadata:   map[string]any{"access_token": "expired"},
	}
	refreshed, ok := manager.tryRefreshAfterUnauthorized(
		context.Background(),
		auth,
		executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		false,
	)
	if ok || refreshed != auth || !executor.called {
		t.Fatalf("tryRefreshAfterUnauthorized() = (%#v, %v), refresh called = %v", refreshed, ok, executor.called)
	}
}

func TestTryRefreshAfterUnauthorizedRejectsUnchangedRuntimeCredential(t *testing.T) {
	previous := currentHomeRuntimeRefreshClient
	currentHomeRuntimeRefreshClient = func() homeRuntimeRefreshClient { return nil }
	t.Cleanup(func() { currentHomeRuntimeRefreshClient = previous })

	manager := NewManager(nil, nil, nil)
	executor := &unchangedRuntimeRefreshExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:         "home-auth",
		Index:      "home-index",
		Provider:   "plugin-provider",
		Generation: 0,
	}
	refreshed, ok := manager.tryRefreshAfterUnauthorized(
		context.Background(),
		auth,
		executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		false,
	)
	if ok || refreshed != auth || !executor.called {
		t.Fatalf("tryRefreshAfterUnauthorized() = (%#v, %v), refresh called = %v", refreshed, ok, executor.called)
	}
}

func TestCompareAndSwapHomeRuntimeAuthUpdatesExistingSession(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	original := &Auth{
		ID:         "home-auth",
		Index:      "old-index",
		Provider:   "codex",
		Generation: 0,
		Metadata:   map[string]any{"access_token": "old"},
	}
	manager.rememberHomeRuntimeAuth("session-1", original)
	refreshed := original.Clone()
	refreshed.Index = "new-index"
	refreshed.Metadata = map[string]any{"access_token": "new"}
	if !manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, executor) {
		t.Fatal("expected runtime auth cache update")
	}
	cached, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth")
	if !ok || cached.Index != "new-index" || authAccessToken(cached) != "new" || cached.Generation != 0 {
		t.Fatalf("cached runtime auth = %#v", cached)
	}
}

func TestCompareAndSwapHomeRuntimeAuthPreservesOpaqueStorage(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "plugin-provider"}
	manager.RegisterExecutor(executor)
	storage := &opaqueRuntimeStorage{value: "opaque"}
	original := &Auth{
		ID:         "home-auth",
		Index:      "old-index",
		Provider:   "plugin-provider",
		Generation: 0,
		Storage:    storage,
		Metadata:   map[string]any{"access_token": "old"},
	}
	manager.rememberHomeRuntimeAuth("session-1", original)
	refreshed := original.Clone()
	refreshed.Index = "new-index"
	refreshed.Storage = nil
	refreshed.Metadata = map[string]any{"access_token": "new"}
	if !manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, executor) {
		t.Fatal("expected runtime auth cache update")
	}
	cached, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth")
	if !ok || cached.Storage != storage {
		t.Fatalf("cached runtime storage = %#v", cached)
	}
}

func TestRegisterExecutorReplacementClearsHomeRuntimeAuths(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	previous := &replaceAwareExecutor{id: "plugin-provider"}
	replacement := &replaceAwareExecutor{id: "plugin-provider"}
	manager.RegisterExecutor(previous)
	manager.rememberHomeRuntimeAuth(
		"session-1",
		&Auth{ID: "home-auth", Provider: "plugin-provider", Generation: 0},
	)
	manager.RegisterExecutor(replacement)
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth"); ok {
		t.Fatal("executor replacement retained provider-owned runtime auth")
	}
}

func TestCompareAndSwapHomeRuntimeAuthRejectsReplacedExecutor(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	previous := &replaceAwareExecutor{id: "plugin-provider"}
	replacement := &replaceAwareExecutor{id: "plugin-provider"}
	manager.RegisterExecutor(previous)
	original := &Auth{ID: "home-auth", Index: "old-index", Provider: "plugin-provider", Generation: 0}
	manager.rememberHomeRuntimeAuth("session-1", original)
	manager.RegisterExecutor(replacement)
	manager.rememberHomeRuntimeAuth("session-1", original)
	refreshed := original.Clone()
	refreshed.Index = "new-index"
	if manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, previous) {
		t.Fatal("replaced executor published stale runtime auth")
	}
}

func TestHomeCompatibilityExecutorOwnsRuntimeAuthLifecycle(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	compatibility := &replaceAwareExecutor{id: "openai-compatibility"}
	manager.RegisterExecutor(compatibility)
	original := &Auth{
		ID:          "home-auth",
		Index:       "old-index",
		Provider:    "vendor-x",
		Generation:  0,
		Attributes:  map[string]string{"base_url": "https://example.test/v1"},
		executorKey: "openai-compatibility",
	}
	manager.rememberHomeRuntimeAuth("session-1", original)
	refreshed := original.Clone()
	refreshed.Index = "new-index"
	if !manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, compatibility) {
		t.Fatal("compatibility executor could not commit its Home runtime auth refresh")
	}
	manager.RegisterExecutor(&replaceAwareExecutor{id: "openai-compatibility"})
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth"); ok {
		t.Fatal("compatibility executor replacement retained its Home runtime auth")
	}
}

func TestCompareAndSwapHomeRuntimeAuthRejectsChangedCurrentExecutorOwner(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	compatibility := &replaceAwareExecutor{id: "openai-compatibility"}
	vendor := &replaceAwareExecutor{id: "vendor-x"}
	manager.RegisterExecutor(compatibility)
	manager.RegisterExecutor(vendor)
	original := &Auth{
		ID:          "home-auth",
		Index:       "home-index",
		Provider:    "vendor-x",
		Generation:  0,
		executorKey: "openai-compatibility",
		Metadata:    map[string]any{"access_token": "old"},
	}
	manager.rememberHomeRuntimeAuth("session-1", original)
	current := original.Clone()
	current.executorKey = "vendor-x"
	manager.rememberHomeRuntimeAuth("session-1", current)
	refreshed := original.Clone()
	refreshed.Metadata = map[string]any{"access_token": "new"}
	if manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, compatibility) {
		t.Fatal("stale compatibility refresh replaced the current vendor executor owner")
	}
	cached, ok := manager.GetExecutionSessionAuthByID("session-1", original.ID)
	if !ok || cached.executorKey != "vendor-x" || authAccessToken(cached) != "old" {
		t.Fatalf("Home runtime auth owner changed after stale refresh: %#v", cached)
	}
}

func TestCompareAndSwapHomeRuntimeAuthPreservesAssignedIndex(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "plugin-provider"}
	manager.RegisterExecutor(executor)
	original := &Auth{ID: "home-auth", Index: "home-index", Provider: "plugin-provider", Generation: 0}
	original.indexAssigned = true
	manager.rememberHomeRuntimeAuth("session-1", original)
	refreshed := original.Clone()
	refreshed.Index = ""
	refreshed.indexAssigned = false
	if !manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, executor) {
		t.Fatal("Home runtime auth refresh was rejected")
	}
	stored, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth")
	if !ok || stored == nil || stored.Index != "home-index" || !stored.indexAssigned {
		t.Fatalf("Home runtime auth = %#v, want preserved assigned index", stored)
	}
}

func TestCompareAndSwapHomeRuntimeAuthDoesNotRecreateClosedSession(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	original := &Auth{ID: "home-auth", Index: "old-index", Provider: "codex", Generation: 0}
	manager.rememberHomeRuntimeAuth("session-1", original)
	manager.CloseExecutionSession("session-1")
	refreshed := original.Clone()
	refreshed.Index = "new-index"
	if manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, executor) {
		t.Fatal("closed execution session was recreated")
	}
	if _, ok := manager.GetExecutionSessionAuthByID("session-1", "home-auth"); ok {
		t.Fatal("closed execution session contains refreshed auth")
	}
}

func TestRefreshAuthForRequestRollsBackAfterProviderReplacement(t *testing.T) {
	guard := &testRefreshCommitGuard{}
	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                guard,
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &refreshCommitStore{entered: make(chan struct{}), release: make(chan struct{})}
	manager.mu.Lock()
	manager.store = store
	manager.mu.Unlock()
	refreshResult := make(chan struct {
		auth *Auth
		err  error
	}, 1)
	go func() {
		refreshed, errRefresh := manager.refreshAuthForRequest(
			context.Background(), registered.ID, "old", registered.Generation,
		)
		refreshResult <- struct {
			auth *Auth
			err  error
		}{auth: refreshed, err: errRefresh}
	}()
	<-store.entered
	replacementDone := make(chan struct{})
	go func() {
		manager.RegisterExecutor(&replaceAwareExecutor{id: "plugin-provider"})
		close(replacementDone)
	}()
	select {
	case <-replacementDone:
	case <-time.After(time.Second):
		t.Fatal("executor replacement waited for refresh persistence")
	}
	close(store.release)
	result := <-refreshResult
	if result.auth != nil || result.err == nil || !strings.Contains(result.err.Error(), "owner changed after persistence") {
		t.Fatalf("refreshAuthForRequest() = (%#v, %v), want stale owner error", result.auth, result.err)
	}
	store.mu.Lock()
	saved := store.saved.Clone()
	saveCalls := store.saveCalls
	store.mu.Unlock()
	if saved == nil || authAccessToken(saved) != "old" || saveCalls != 2 || guard.calls.Load() != 0 {
		t.Fatalf(
			"rollback state = %#v, saves = %d, guard calls before executor check = %d",
			saved,
			saveCalls,
			guard.calls.Load(),
		)
	}
}

func TestTryRefreshAfterUnauthorizedUsesExecutorCapabilityForOpaqueCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &opaqueUnauthorizedRefreshExecutor{replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"}}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth",
			Provider: "plugin-provider",
			Metadata: map[string]any{"provider_refresh_state": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	refreshed, ok := manager.tryRefreshAfterUnauthorized(
		context.Background(),
		registered,
		executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		false,
	)
	if !ok || refreshed == nil || executor.refreshCalls != 1 ||
		contextStringValue(refreshed.Metadata["provider_refresh_state"]) != "new" {
		t.Fatalf("refresh result = %#v, ok = %v, calls = %d", refreshed, ok, executor.refreshCalls)
	}
}

func TestTryRefreshAfterUnauthorizedRejectsManagedNoOpCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &opaqueUnauthorizedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		unchanged:            true,
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "plugin-auth", Provider: "plugin-provider", Metadata: map[string]any{"opaque": "old"}},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	refreshed, ok := manager.tryRefreshAfterUnauthorized(
		context.Background(),
		registered,
		executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		false,
	)
	if ok || refreshed != registered || executor.refreshCalls != 1 {
		t.Fatalf("refresh result = %#v, ok = %v, calls = %d", refreshed, ok, executor.refreshCalls)
	}
}

func TestRefreshAuthForRequestRollsBackRejectedFinalPublication(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &refreshCommitStore{}
	manager.SetStore(store)
	refreshed, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old", registered.Generation,
	)
	if refreshed != nil || errRefresh == nil || !strings.Contains(errRefresh.Error(), "owner changed after persistence") {
		t.Fatalf("refreshAuthForRequest() = (%#v, %v), want rejected publication", refreshed, errRefresh)
	}
	current, ok := manager.GetByID(registered.ID)
	store.mu.Lock()
	saved := store.saved.Clone()
	saveCalls := store.saveCalls
	store.mu.Unlock()
	if !ok || authAccessToken(current) != "old" || saved == nil || authAccessToken(saved) != "old" || saveCalls != 2 {
		t.Fatalf("rollback state current=%#v saved=%#v saves=%d", current, saved, saveCalls)
	}
}

func TestRefreshRollbackTimeoutReleasesPersistenceLock(t *testing.T) {
	previousTimeout := swapRefreshPersistenceRollbackTimeout(20 * time.Millisecond)
	t.Cleanup(func() { swapRefreshPersistenceRollbackTimeout(previousTimeout) })

	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
	}
	manager.SetStore(store)
	_, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old", registered.Generation,
	)
	if errRefresh == nil || !errors.Is(errRefresh, errAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want durable state uncertainty", errRefresh)
	}
	select {
	case <-store.rollbackEntered:
	default:
		t.Fatal("refresh rollback did not enter the blocking store")
	}

	registerDone := make(chan error, 1)
	go func() {
		_, err := manager.Register(
			context.Background(),
			&Auth{
				ID:       "other-auth",
				Provider: "plugin-provider",
				Metadata: map[string]any{"access_token": "other"},
			},
		)
		registerDone <- err
	}()
	select {
	case err := <-registerDone:
		if err != nil {
			t.Fatalf("Register(other-auth) error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked refresh rollback retained the global persistence lock")
	}

	current, _ := manager.GetByID(registered.ID)
	current.Metadata = map[string]any{"access_token": "later"}
	if _, errUpdate := manager.Update(context.Background(), current); !errors.Is(errUpdate, errAuthDurableStateUncertain) {
		t.Fatalf("Update(plugin-auth) error = %v, want isolated durable state", errUpdate)
	}
	markDone := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{AuthID: registered.ID, Success: true})
		close(markDone)
	}()
	select {
	case <-markDone:
	case <-time.After(time.Second):
		t.Fatal("MarkResult blocked on an unfinished persistence rollback")
	}

	close(store.rollbackRelease)
	deadline := time.Now().Add(time.Second)
	for {
		current, _ = manager.GetByID(registered.ID)
		current.Metadata = map[string]any{"access_token": "later"}
		_, errUpdate := manager.Update(context.Background(), current)
		if errUpdate == nil {
			break
		}
		if !errors.Is(errUpdate, errAuthDurableStateUncertain) || time.Now().After(deadline) {
			t.Fatalf("Update(plugin-auth) error after rollback finished = %v", errUpdate)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRemoveDuringRefreshRollbackDefersDurableDeletion(t *testing.T) {
	previousTimeout := swapRefreshPersistenceRollbackTimeout(20 * time.Millisecond)
	t.Cleanup(func() { swapRefreshPersistenceRollbackTimeout(previousTimeout) })

	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth-remove",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
		deleted:         make(chan struct{}),
	}
	manager.SetStore(store)
	_, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old", registered.Generation,
	)
	if !errors.Is(errRefresh, errAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want durable state uncertainty", errRefresh)
	}
	select {
	case <-store.rollbackEntered:
	default:
		t.Fatal("refresh rollback did not enter the blocking store")
	}

	if errRemove := manager.Remove(context.Background(), registered.ID); !errors.Is(
		errRemove,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("Remove() error = %v, want durable state uncertainty", errRemove)
	}
	if _, ok := manager.GetByID(registered.ID); ok {
		t.Fatal("Remove() kept auth in runtime state")
	}
	if errRemove := manager.Remove(WithSkipPersist(context.Background()), registered.ID); !errors.Is(
		errRemove,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("duplicate Remove() error = %v, want durable state uncertainty", errRemove)
	}
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if _, ok := manager.GetByID(registered.ID); ok {
		t.Fatal("Load() restored auth covered by pending durable deletion")
	}
	store.mu.Lock()
	deleteCalls := store.deleteCalls
	store.mu.Unlock()
	if deleteCalls != 0 {
		t.Fatalf("Delete calls before rollback completion = %d, want 0", deleteCalls)
	}

	close(store.rollbackRelease)
	select {
	case <-store.deleted:
	case <-time.After(time.Second):
		t.Fatal("deferred durable deletion did not run after rollback completion")
	}
	store.mu.Lock()
	deleteCalls = store.deleteCalls
	store.mu.Unlock()
	if deleteCalls != 1 {
		t.Fatalf("Delete calls after rollback completion = %d, want 1", deleteCalls)
	}
}

func TestRecreateDuringRefreshRollbackSupersedesDeferredDeletion(t *testing.T) {
	previousTimeout := swapRefreshPersistenceRollbackTimeout(20 * time.Millisecond)
	t.Cleanup(func() { swapRefreshPersistenceRollbackTimeout(previousTimeout) })

	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth-recreate",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
	}
	manager.SetStore(store)
	_, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old", registered.Generation,
	)
	if !errors.Is(errRefresh, errAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want durable state uncertainty", errRefresh)
	}
	if errRemove := manager.Remove(context.Background(), registered.ID); !errors.Is(
		errRemove,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("Remove() error = %v, want durable state uncertainty", errRemove)
	}

	recreated := &Auth{
		ID:       registered.ID,
		Provider: registered.Provider,
		Metadata: map[string]any{"access_token": "recreated"},
	}
	recreateCtx := WithAllowAuthRecreate(WithSkipPersist(context.Background()))
	if _, errRecreate := manager.Register(recreateCtx, recreated); !errors.Is(
		errRecreate,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("Register(recreated) error = %v, want queued durable state", errRecreate)
	}
	close(store.rollbackRelease)

	deadline := time.Now().Add(time.Second)
	for {
		current, ok := manager.GetByID(registered.ID)
		store.mu.Lock()
		saved := store.saved.Clone()
		store.mu.Unlock()
		if ok && authAccessToken(current) == "recreated" && authAccessToken(saved) == "recreated" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recreated auth not restored: current=%#v saved=%#v", current, saved)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeleteSupersedesQueuedRecreationDuringRollback(t *testing.T) {
	previousTimeout := swapRefreshPersistenceRollbackTimeout(20 * time.Millisecond)
	t.Cleanup(func() { swapRefreshPersistenceRollbackTimeout(previousTimeout) })

	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth-delete-wins",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
		deleted:         make(chan struct{}),
	}
	manager.SetStore(store)
	_, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old", registered.Generation,
	)
	if !errors.Is(errRefresh, errAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want durable state uncertainty", errRefresh)
	}

	recreated := &Auth{
		ID:       registered.ID,
		Provider: registered.Provider,
		Metadata: map[string]any{"access_token": "recreated"},
	}
	recreateCtx := WithAllowAuthRecreate(WithSkipPersist(context.Background()))
	if _, errRecreate := manager.Register(recreateCtx, recreated); !errors.Is(
		errRecreate,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("Register(recreated) error = %v, want queued durable state", errRecreate)
	}
	if errRemove := manager.Remove(WithSkipPersist(context.Background()), registered.ID); !errors.Is(
		errRemove,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("Remove() error = %v, want durable state uncertainty", errRemove)
	}
	close(store.rollbackRelease)
	select {
	case <-store.deleted:
	case <-time.After(time.Second):
		t.Fatal("newer delete did not remove rollback credential")
	}
	if current, ok := manager.GetByID(registered.ID); ok {
		t.Fatalf("newer delete was superseded by queued recreation: %#v", current)
	}
	store.mu.Lock()
	saved := store.saved.Clone()
	store.mu.Unlock()
	if saved != nil {
		t.Fatalf("newer delete left persisted auth: %#v", saved)
	}
}

func TestDeleteInvalidatesDeferredRecreationAfterHandlerStarts(t *testing.T) {
	previousTimeout := swapRefreshPersistenceRollbackTimeout(20 * time.Millisecond)
	t.Cleanup(func() { swapRefreshPersistenceRollbackTimeout(previousTimeout) })

	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth-handler-delete",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
		deleted:         make(chan struct{}),
	}
	manager.SetStore(store)
	handlerStarted := make(chan struct{})
	handlerRelease := make(chan struct{})
	handlerDone := make(chan struct{})
	manager.SetDeferredAuthRecreateHandler(
		func(ctx context.Context, auth *Auth) error {
			close(handlerStarted)
			<-handlerRelease
			_, err := manager.Register(ctx, auth)
			close(handlerDone)
			return err
		},
	)
	_, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old", registered.Generation,
	)
	if !errors.Is(errRefresh, errAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want durable state uncertainty", errRefresh)
	}
	recreated := &Auth{
		ID:       registered.ID,
		Provider: registered.Provider,
		Metadata: map[string]any{"access_token": "recreated"},
	}
	recreateCtx := WithAllowAuthRecreate(WithSkipPersist(context.Background()))
	if _, errRecreate := manager.Register(recreateCtx, recreated); !errors.Is(
		errRecreate,
		errAuthDurableStateUncertain,
	) {
		t.Fatalf("Register(recreated) error = %v, want queued durable state", errRecreate)
	}
	close(store.rollbackRelease)
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("deferred recreation handler did not start")
	}
	if errRemove := manager.Remove(WithSkipPersist(context.Background()), registered.ID); !errors.Is(
		errRemove,
		ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("newer Remove() error = %v, want durable state uncertainty", errRemove)
	}
	close(handlerRelease)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("deferred recreation handler did not finish")
	}
	if current, ok := manager.GetByID(registered.ID); ok {
		t.Fatalf("stale deferred recreation restored auth: %#v", current)
	}
	select {
	case <-store.deleted:
	case <-time.After(time.Second):
		t.Fatal("newer delete did not remove the stale persisted recreation")
	}
}

func TestRemoveDuringActiveRefreshRollbackDeletesRestoredCredential(t *testing.T) {
	previousTimeout := swapRefreshPersistenceRollbackTimeout(time.Second)
	t.Cleanup(func() { swapRefreshPersistenceRollbackTimeout(previousTimeout) })

	manager := NewManager(nil, nil, nil)
	executor := &guardedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		guard:                rejectingAfterCommitGuard{},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth-active-rollback-remove",
			Provider: "plugin-provider",
			Metadata: map[string]any{"access_token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
		deleted:         make(chan struct{}),
	}
	manager.SetStore(store)
	refreshDone := make(chan error, 1)
	go func() {
		_, err := manager.refreshAuthForRequest(
			context.Background(), registered.ID, "old", registered.Generation,
		)
		refreshDone <- err
	}()
	select {
	case <-store.rollbackEntered:
	case <-time.After(time.Second):
		t.Fatal("refresh rollback did not enter the blocking store")
	}
	removeDone := make(chan error, 1)
	go func() {
		removeDone <- manager.Remove(WithSkipPersist(context.Background()), registered.ID)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		_, removed := manager.removedAuths[registered.ID]
		manager.mu.RUnlock()
		if removed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Remove() did not publish its deletion intent")
		}
		time.Sleep(time.Millisecond)
	}
	close(store.rollbackRelease)
	if errRefresh := <-refreshDone; errRefresh == nil {
		t.Fatal("refresh unexpectedly accepted rejected publication")
	}
	if errRemove := <-removeDone; errRemove != nil && !errors.Is(errRemove, errAuthDurableStateUncertain) {
		t.Fatalf("Remove() error = %v", errRemove)
	}
	select {
	case <-store.deleted:
	case <-time.After(time.Second):
		t.Fatal("rollback completion did not delete the restored credential")
	}
}

func TestDeferredAuthDeletionRetriesUntilSuccess(t *testing.T) {
	previousInitialDelay := deferredAuthRecreateRetryInitialDelay
	previousMaxDelay := deferredAuthRecreateRetryMaxDelay
	deferredAuthRecreateRetryInitialDelay = time.Millisecond
	deferredAuthRecreateRetryMaxDelay = 2 * time.Millisecond
	t.Cleanup(func() {
		deferredAuthRecreateRetryInitialDelay = previousInitialDelay
		deferredAuthRecreateRetryMaxDelay = previousMaxDelay
	})

	store := &blockingRefreshRollbackStore{
		rollbackEntered: make(chan struct{}),
		rollbackRelease: make(chan struct{}),
		deleted:         make(chan struct{}),
		deleteFailures:  2,
	}
	manager := NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "retry-delete-auth", Provider: "codex"},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errRemove := manager.Remove(WithSkipPersist(context.Background()), registered.ID); errRemove != nil {
		t.Fatalf("Remove() error = %v", errRemove)
	}
	pending := make(chan struct{})
	close(pending)
	manager.ensureAuthDeletionAfterPersistenceRollback(registered.ID, pending, nil)
	select {
	case <-store.deleted:
	case <-time.After(time.Second):
		t.Fatal("deferred deletion did not retry to success")
	}
	store.mu.Lock()
	deleteCalls := store.deleteCalls
	saved := store.saved
	store.mu.Unlock()
	if deleteCalls != 3 {
		t.Fatalf("Delete() calls = %d, want 3", deleteCalls)
	}
	if saved != nil {
		t.Fatalf("deleted credential remains persisted: %#v", saved)
	}
}

func TestRejectedPublicationDoesNotDeleteNewerAuthoritativeCredential(t *testing.T) {
	store := &rejectedPublicationStore{removeDone: make(chan struct{})}
	manager := NewManager(store, nil, nil)
	store.manager = manager
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "auth-rejected-publication",
			Provider: "codex",
			Metadata: map[string]any{"token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("initial Register() error = %v", errRegister)
	}
	if errRemove := manager.Remove(context.Background(), registered.ID); errRemove != nil {
		t.Fatalf("initial Remove() error = %v", errRemove)
	}
	manager.mu.RLock()
	store.removeAfterSeq.Store(manager.removedAuths[registered.ID])
	manager.mu.RUnlock()
	newer := &Auth{
		ID:       registered.ID,
		Provider: registered.Provider,
		Metadata: map[string]any{"token": "authoritative"},
	}
	store.replacement = newer
	store.trigger.Store(true)
	recreated, errRecreate := manager.Register(
		WithAllowAuthRecreate(context.Background()),
		&Auth{
			ID:       registered.ID,
			Provider: registered.Provider,
			Metadata: map[string]any{"token": "rejected"},
		},
	)
	if recreated != nil || !errors.Is(errRecreate, ErrAuthDurableStateUncertain) {
		t.Fatalf("Register() = (%#v, %v), want durable state uncertainty", recreated, errRecreate)
	}
	select {
	case <-store.removeDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent Remove() did not finish")
	}
	store.mu.Lock()
	saved := store.saved.Clone()
	deleteCalls := store.deleteCalls
	store.mu.Unlock()
	if saved == nil || saved.Metadata["token"] != "authoritative" {
		t.Fatalf("newer authoritative credential was changed: %#v", saved)
	}
	if deleteCalls != 1 {
		t.Fatalf("Delete() calls = %d, want only the initial removal", deleteCalls)
	}
}

func TestRejectedPersistedDeletionPreservesNewerCredential(t *testing.T) {
	store := &versionedDeletionStore{}
	manager := NewManager(store, nil, nil)
	rejected := &Auth{
		ID:       "rejected-versioned-auth",
		Provider: "codex",
		Metadata: map[string]any{"token": "rejected"},
	}
	newer := rejected.Clone()
	newer.Metadata = map[string]any{"token": "authoritative"}
	store.saved = newer.Clone()

	manager.mu.Lock()
	manager.removedAuthSeq++
	manager.removedAuths[rejected.ID] = manager.removedAuthSeq
	manager.mu.Unlock()
	if !errors.Is(
		manager.scheduleRejectedPersistedAuthDeletion(rejected.ID, rejected),
		ErrAuthDurableStateUncertain,
	) {
		t.Fatal("rejected persistence did not report durable state uncertainty")
	}

	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		intent := manager.pendingAuthDeletions[rejected.ID]
		manager.mu.RUnlock()
		if intent != nil && intent.ctx.Err() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conditional deletion worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	store.mu.Lock()
	saved := store.saved.Clone()
	deleteCalls := store.deleteCalls
	store.mu.Unlock()
	if saved == nil || saved.Metadata["token"] != "authoritative" || deleteCalls != 0 {
		t.Fatalf("newer credential was deleted: saved=%#v deletes=%d", saved, deleteCalls)
	}
}

func TestRejectedWatcherRecreationWaitsForDifferentCredential(t *testing.T) {
	store := &versionedDeletionStore{}
	manager := NewManager(store, nil, nil)
	rejected := &Auth{
		ID:       "rejected-watcher-auth",
		Provider: "codex",
		Metadata: map[string]any{"token": "rejected"},
	}
	store.saved = rejected.Clone()
	manager.mu.Lock()
	manager.removedAuthSeq++
	manager.removedAuths[rejected.ID] = manager.removedAuthSeq
	manager.mu.Unlock()
	_ = manager.scheduleRejectedPersistedAuthDeletion(rejected.ID, rejected)

	watcherCtx := WithAllowAuthRecreate(WithSkipPersist(context.Background()))
	if recreated, errRecreate := manager.Register(watcherCtx, rejected.Clone()); recreated != nil || !errors.Is(
		errRecreate,
		ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("rejected watcher recreation = (%#v, %v)", recreated, errRecreate)
	}
	newer := rejected.Clone()
	newer.Metadata = map[string]any{"token": "authoritative"}
	recreated, errRecreate := manager.Register(watcherCtx, newer)
	if errRecreate != nil || recreated == nil || recreated.Metadata["token"] != "authoritative" {
		t.Fatalf("authoritative watcher recreation = (%#v, %v)", recreated, errRecreate)
	}
	store.mu.Lock()
	saved := store.saved.Clone()
	store.mu.Unlock()
	if saved == nil || saved.Metadata["token"] != "authoritative" {
		t.Fatalf("authoritative watcher credential was not restored: %#v", saved)
	}
}

func TestForgetRemovedRuntimeAuthReleasesRemovalState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	for index := 0; index < 100; index++ {
		id := fmt.Sprintf("runtime-only-%d", index)
		if _, errRegister := manager.Register(
			ctx,
			&Auth{
				ID:         id,
				Provider:   "aistudio",
				Status:     StatusActive,
				Attributes: map[string]string{"runtime_only": "true"},
			},
		); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
		if errRemove := manager.Remove(ctx, id); errRemove != nil {
			t.Fatalf("Remove(%s) error = %v", id, errRemove)
		}
		manager.ForgetRemovedRuntimeAuth(id)
	}
	manager.mu.RLock()
	removedAuths := len(manager.removedAuths)
	removedAuthAt := len(manager.removedAuthAt)
	manager.mu.RUnlock()
	if removedAuths != 0 || removedAuthAt != 0 {
		t.Fatalf("removed runtime auth state = (%d, %d), want empty", removedAuths, removedAuthAt)
	}
	var removalEpochs int
	manager.authRemovalEpochs.Range(func(_, _ any) bool {
		removalEpochs++
		return true
	})
	if removalEpochs != 0 {
		t.Fatalf("auth removal epoch entries = %d, want 0", removalEpochs)
	}
}

func TestWatcherMutationReadBeforeRemovalCannotRecreateCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "stale-watcher-auth", Provider: "codex", Metadata: map[string]any{"token": "old"}},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errRemove := manager.Remove(WithSkipPersist(context.Background()), registered.ID); errRemove != nil {
		t.Fatalf("Remove() error = %v", errRemove)
	}
	watcherCtx := WithAllowAuthRecreate(WithSkipPersist(context.Background()))
	stale := registered.Clone()
	stale.Attributes = map[string]string{
		PersistedReadAtAttribute: time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if recreated, errRecreate := manager.Register(watcherCtx, stale); recreated != nil || errRecreate == nil {
		t.Fatalf("stale watcher recreation = (%#v, %v), want rejection", recreated, errRecreate)
	}
	if _, ok := manager.GetByID(registered.ID); ok {
		t.Fatal("stale watcher mutation recreated removed credential")
	}

	fresh := registered.Clone()
	fresh.Metadata = map[string]any{"token": "fresh"}
	fresh.Attributes = map[string]string{
		PersistedReadAtAttribute: time.Now().Add(time.Minute).Format(time.RFC3339Nano),
	}
	recreated, errRecreate := manager.Register(watcherCtx, fresh)
	if errRecreate != nil || recreated == nil || recreated.Metadata["token"] != "fresh" {
		t.Fatalf("fresh watcher recreation = (%#v, %v)", recreated, errRecreate)
	}
}

func TestDeferredAuthRecreationRetriesUntilSuccess(t *testing.T) {
	previousInitialDelay := deferredAuthRecreateRetryInitialDelay
	previousMaxDelay := deferredAuthRecreateRetryMaxDelay
	deferredAuthRecreateRetryInitialDelay = time.Millisecond
	deferredAuthRecreateRetryMaxDelay = 2 * time.Millisecond
	t.Cleanup(func() {
		deferredAuthRecreateRetryInitialDelay = previousInitialDelay
		deferredAuthRecreateRetryMaxDelay = previousMaxDelay
	})

	manager := NewManager(nil, nil, nil)
	var calls atomic.Int32
	completed := make(chan struct{})
	manager.SetDeferredAuthRecreateHandler(
		func(context.Context, *Auth) error {
			if calls.Add(1) < 3 {
				return errors.New("temporary recreate failure")
			}
			close(completed)
			return nil
		},
	)
	pending := make(chan struct{})
	close(pending)
	if !manager.queueAuthRecreation(context.Background(), &Auth{ID: "retry-auth", Provider: "codex"}, pending) {
		t.Fatal("queueAuthRecreation() rejected active manager")
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("deferred recreation did not retry to success")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls = %d, want 3", got)
	}
	if errStop := manager.StopDeferredAuthRecreates(context.Background()); errStop != nil {
		t.Fatalf("StopDeferredAuthRecreates() error = %v", errStop)
	}
}

func TestStopDeferredAuthRecreatesCancelsWorkerAndRejectsNewJobs(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	started := make(chan struct{})
	exited := make(chan struct{})
	manager.SetDeferredAuthRecreateHandler(
		func(ctx context.Context, _ *Auth) error {
			close(started)
			<-ctx.Done()
			close(exited)
			return ctx.Err()
		},
	)
	pending := make(chan struct{})
	close(pending)
	if !manager.queueAuthRecreation(context.Background(), &Auth{ID: "shutdown-auth", Provider: "codex"}, pending) {
		t.Fatal("queueAuthRecreation() rejected active manager")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("deferred recreation handler did not start")
	}
	if errStop := manager.StopDeferredAuthRecreates(context.Background()); !errors.Is(
		errStop,
		ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("StopDeferredAuthRecreates() error = %v, want durable state uncertainty", errStop)
	}
	select {
	case <-exited:
	default:
		t.Fatal("deferred recreation handler did not observe cancellation")
	}
	if manager.queueAuthRecreation(context.Background(), &Auth{ID: "late-auth", Provider: "codex"}, pending) {
		t.Fatal("queueAuthRecreation() accepted a job after shutdown")
	}
	if _, ok := manager.pendingAuthRecreations.Load("shutdown-auth"); ok {
		t.Fatal("shutdown retained the completed recreation intent")
	}
}

func TestStopDeferredAuthRecreatesTimeoutReportsDurableStateUncertainty(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	manager.SetDeferredAuthRecreateHandler(
		func(context.Context, *Auth) error {
			close(started)
			<-release
			return context.Canceled
		},
	)
	pending := make(chan struct{})
	close(pending)
	if !manager.queueAuthRecreation(
		context.Background(),
		&Auth{ID: "shutdown-timeout-auth", Provider: "codex"},
		pending,
	) {
		t.Fatal("queueAuthRecreation() rejected active manager")
	}
	<-started
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelStop()
	errStop := manager.StopDeferredAuthRecreates(stopCtx)
	if !errors.Is(errStop, context.DeadlineExceeded) || !errors.Is(errStop, ErrAuthDurableStateUncertain) {
		t.Fatalf("StopDeferredAuthRecreates() error = %v", errStop)
	}
	close(release)
	if errFinal := manager.StopDeferredAuthRecreates(context.Background()); !errors.Is(
		errFinal,
		ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("final StopDeferredAuthRecreates() error = %v", errFinal)
	}
}

func TestCompareAndSwapHomeRuntimeAuthRollsBackRejectedFinalPublication(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &replaceAwareExecutor{id: "plugin-provider"}
	manager.RegisterExecutor(executor)
	original := &Auth{
		ID:       "home-auth",
		Index:    "home-index",
		Provider: "plugin-provider",
		Metadata: map[string]any{"access_token": "old"},
	}
	manager.rememberHomeRuntimeAuth("session-1", original)
	refreshed := original.Clone()
	refreshed.Metadata = map[string]any{"access_token": "new"}
	refreshed.RefreshCommit = rejectingAfterCommitGuard{}
	if manager.compareAndSwapHomeRuntimeAuth("session-1", original, refreshed, executor) {
		t.Fatal("Home runtime auth accepted rejected final publication")
	}
	current, ok := manager.GetExecutionSessionAuthByID("session-1", original.ID)
	if !ok || authAccessToken(current) != "old" {
		t.Fatalf("Home runtime auth was not restored: %#v", current)
	}
}

func TestRefreshAuthForRequestPreservesQuotaCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &opaqueUnauthorizedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
	}
	manager.RegisterExecutor(executor)
	retryAfter := time.Now().Add(time.Minute)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:             "plugin-auth",
			Provider:       "plugin-provider",
			Metadata:       map[string]any{"provider_refresh_state": "old"},
			Status:         StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			Quota:          QuotaState{Exceeded: true, Reason: "rate limited", NextRecoverAt: retryAfter},
			LastError:      &Error{Code: "rate_limit", Message: "rate limited", HTTPStatus: http.StatusTooManyRequests},
			NextRetryAfter: retryAfter,
			ModelStates: map[string]*ModelState{
				"model-a": {
					Status:         StatusError,
					StatusMessage:  "rate limited",
					Unavailable:    true,
					NextRetryAfter: retryAfter,
					LastError:      &Error{Code: "rate_limit", Message: "rate limited", HTTPStatus: http.StatusTooManyRequests},
					Quota:          QuotaState{Exceeded: true, Reason: "rate limited", NextRecoverAt: retryAfter},
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	refreshed, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "", registered.Generation,
	)
	if errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	modelState := refreshed.ModelStates["model-a"]
	if refreshed == nil || !refreshed.Unavailable || !refreshed.Quota.Exceeded ||
		refreshed.LastError == nil || refreshed.LastError.Code != "rate_limit" ||
		refreshed.NextRetryAfter != retryAfter || modelState == nil || !modelState.Unavailable ||
		modelState.NextRetryAfter != retryAfter || !modelState.Quota.Exceeded {
		t.Fatalf("refresh cleared unrelated quota cooldown: %#v", refreshed)
	}
}

func TestRegisterExecutorIfAuthCurrentRejectsStaleGeneration(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	previous := &replaceAwareExecutor{id: "plugin-provider"}
	candidate := &replaceAwareExecutor{id: "plugin-provider"}
	manager.RegisterExecutor(previous)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "executor-generation-auth", Provider: "plugin-provider"},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	updated, errUpdate := manager.Update(
		context.Background(),
		&Auth{
			ID:         registered.ID,
			Provider:   registered.Provider,
			Generation: registered.Generation,
			Metadata:   map[string]any{"token": "new"},
		},
	)
	if errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	if updated.Generation == registered.Generation {
		t.Fatalf("Update() generation = %d, want newer than %d", updated.Generation, registered.Generation)
	}
	if manager.RegisterExecutorIfAuthCurrent(registered.ID, registered.Generation, candidate) {
		t.Fatal("stale auth generation replaced the shared executor")
	}
	current, ok := manager.Executor(registered.Provider)
	if !ok || current != previous {
		t.Fatalf("Executor() = %#v, want previous executor", current)
	}
}

func TestRefreshAuthForRequestRejectsReplacedExecutorResult(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	previous := &opaqueUnauthorizedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		started:              started,
		release:              release,
	}
	replacement := &opaqueUnauthorizedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
	}
	manager.RegisterExecutor(previous)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth",
			Provider: "plugin-provider",
			Metadata: map[string]any{"provider_refresh_state": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	result := make(chan error, 1)
	go func() {
		_, errRefresh := manager.refreshAuthForRequest(
			context.Background(), registered.ID, "", registered.Generation,
		)
		result <- errRefresh
	}()
	<-started
	manager.RegisterExecutor(replacement)
	close(release)
	if errRefresh := <-result; errRefresh == nil || !strings.Contains(errRefresh.Error(), "stale") {
		t.Fatalf("refresh error = %v, want stale result", errRefresh)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || contextStringValue(current.Metadata["provider_refresh_state"]) != "old" {
		t.Fatalf("current auth = %#v", current)
	}
}

func TestRefreshAuthForRequestRejectsReplacedExecutorError(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	previous := &opaqueUnauthorizedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
		refreshErr:           testStatusError{status: http.StatusUnauthorized, message: "unauthorized"},
		started:              started,
		release:              release,
	}
	replacement := &opaqueUnauthorizedRefreshExecutor{
		replaceAwareExecutor: replaceAwareExecutor{id: "plugin-provider"},
	}
	manager.RegisterExecutor(previous)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "plugin-auth",
			Provider: "plugin-provider",
			Metadata: map[string]any{"provider_refresh_state": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	result := make(chan error, 1)
	go func() {
		_, errRefresh := manager.refreshAuthForRequest(
			context.Background(), registered.ID, "", registered.Generation,
		)
		result <- errRefresh
	}()
	<-started
	manager.RegisterExecutor(replacement)
	close(release)
	if errRefresh := <-result; errRefresh == nil || !strings.Contains(errRefresh.Error(), "stale") {
		t.Fatalf("refresh error = %v, want stale result", errRefresh)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Unavailable || current.LastError != nil || current.Status == StatusError {
		t.Fatalf("replacement auth was polluted by stale refresh error: %#v", current)
	}
}

func TestManagerUpdateRejectsPublicationAfterSuccessfulSaveCancelsContext(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "auth-1", Provider: "codex", Metadata: map[string]any{"token": "old"}},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancelAfterSaveStore{cancel: cancel}
	manager.SetStore(store)
	update := registered.Clone()
	update.Metadata = map[string]any{"token": "new"}
	updated, errUpdate := manager.Update(ctx, update)
	if updated != nil || !errors.Is(errUpdate, ErrAuthDurableStateUncertain) || !errors.Is(errUpdate, context.Canceled) {
		t.Fatalf("Update() = (%#v, %v), want canceled durable state uncertainty", updated, errUpdate)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["token"] != "old" || store.saved == nil ||
		store.saved.Metadata["token"] != "new" {
		t.Fatalf("publication state current=%#v saved=%#v", current, store.saved)
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
