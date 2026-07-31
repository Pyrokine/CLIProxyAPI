package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestManager_SetConfigClearsHomeRuntimeAuthsWhenHomeDisabled(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:         "runtime-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}

	m.rememberHomeRuntimeAuth("session-1", auth)
	if _, ok := m.GetExecutionSessionAuthByID("session-1", "runtime-auth"); !ok {
		t.Fatal("expected runtime auth to be cached")
	}

	m.SetConfig(&internalconfig.Config{})
	if _, ok := m.GetExecutionSessionAuthByID("session-1", "runtime-auth"); ok {
		t.Fatal("expected runtime auth cache to be cleared when Home is disabled")
	}
}

func TestManager_Update_PreservesModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			Metadata: map[string]any{"k": "v"},
			ModelStates: map[string]*ModelState{
				model: {
					Quota: QuotaState{BackoffLevel: backoffLevel},
				},
			},
		},
	); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	// Persistent updates must not clear request-time quota state.
	if _, errUpdate := m.Update(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			Metadata: map[string]any{"k": "v2"},
		},
	); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}

	state := updated.ModelStates[model]
	if state == nil || state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("ModelStates were overwritten by persistent update: %+v", updated.ModelStates)
	}

	// Verify metadata was updated
	if v, ok := updated.Metadata["k"]; !ok || v != "v2" {
		t.Fatalf("expected metadata k=v2, got %v", updated.Metadata)
	}
}

func TestManager_SetConfigUpdatesAutoRefreshWorkers(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 3})

	m.mu.RLock()
	workers := cap(m.refreshSemaphore)
	m.mu.RUnlock()

	if workers != 3 {
		t.Fatalf("refresh semaphore capacity = %d, want 3", workers)
	}
}

func TestManager_RefreshAuthUnauthorizedStopsAutoRefreshRetry(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(unauthorizedRefreshExecutor{})
	_, err := m.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "unauthorized-refresh",
			Metadata: map[string]any{"token": "old"},
		},
	)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.refreshAuth(context.Background(), "auth-1")
	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatal("expected auth to be present")
	}
	if updated.LastError == nil || updated.LastError.Code != "unauthorized" {
		t.Fatalf("expected unauthorized refresh error, got %+v", updated.LastError)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected unauthorized refresh to stop retries, got %s", updated.NextRefreshAfter)
	}
	if m.shouldRefresh(updated, updated.UpdatedAt) {
		t.Fatal("expected unauthorized refresh failure to disable auto-refresh retry")
	}
}

type unauthorizedRefreshExecutor struct{ schedulerTestExecutor }

func (unauthorizedRefreshExecutor) Identifier() string { return "unauthorized-refresh" }

func (unauthorizedRefreshExecutor) Refresh(_ context.Context, _ *Auth) (*Auth, error) {
	return nil, &Error{Code: "authentication_error", Message: "401 unauthorized", HTTPStatus: http.StatusUnauthorized}
}

type cancelAfterSuccessRefreshExecutor struct {
	schedulerTestExecutor
	cancel context.CancelFunc
}

func (e *cancelAfterSuccessRefreshExecutor) Identifier() string {
	return "cancel-after-success-refresh"
}

func (e *cancelAfterSuccessRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	updated := auth.Clone()
	updated.Metadata = map[string]any{
		"access_token":  "new-token",
		"refresh_token": "new-refresh",
	}
	e.cancel()
	return updated, nil
}

type contextCheckingRefreshStore struct {
	mu    sync.Mutex
	saved *Auth
}

func (s *contextCheckingRefreshStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *contextCheckingRefreshStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.saved = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *contextCheckingRefreshStore) Delete(context.Context, string) error {
	return nil
}

type blockingRefreshExecutor struct {
	schedulerTestExecutor
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type rotatingRefreshExecutor struct {
	schedulerTestExecutor
	beforeReturn func() error
}

type virtualPersistenceRefreshExecutor struct {
	schedulerTestExecutor
	persistenceRuntime any
}

func (e *virtualPersistenceRefreshExecutor) Identifier() string { return "virtual-persistence-refresh" }

func (e *virtualPersistenceRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	updated := auth.Clone()
	updated.Metadata = map[string]any{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
		"project_id":    "project-a,project-b",
	}
	updated.RefreshPersistence = &Auth{
		ID:         "primary.json",
		Provider:   e.Identifier(),
		FileName:   "primary.json",
		Disabled:   true,
		Status:     StatusDisabled,
		Attributes: map[string]string{"path": auth.Attributes["path"]},
		Metadata:   updated.Metadata,
		Runtime:    e.persistenceRuntime,
	}
	return updated, nil
}

func (e *rotatingRefreshExecutor) Identifier() string { return "rotating-refresh" }

func (e *rotatingRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	updated := auth.Clone()
	updated.Metadata = map[string]any{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
	}
	if e.beforeReturn != nil {
		if err := e.beforeReturn(); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

type retryingRefreshStore struct {
	mu        sync.Mutex
	path      string
	saveCalls int
	failCall  int
	retried   chan struct{}
	once      sync.Once
}

func (*retryingRefreshStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *retryingRefreshStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.saveCalls++
	call := s.saveCalls
	s.mu.Unlock()
	if call == s.failCall {
		return "", errors.New("persist failed")
	}
	PrepareAuthForPersistence(auth)
	raw, errMarshal := json.Marshal(auth.Metadata)
	if errMarshal != nil {
		return "", errMarshal
	}
	if errWrite := os.WriteFile(s.path, raw, 0o600); errWrite != nil {
		return "", errWrite
	}
	if call > s.failCall && s.retried != nil {
		s.once.Do(func() { close(s.retried) })
	}
	return s.path, nil
}

func (s *retryingRefreshStore) Delete(context.Context, string) error {
	return os.Remove(s.path)
}

func (e *blockingRefreshExecutor) Identifier() string { return "blocking-refresh" }

func (e *blockingRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
	}
	updated := auth.Clone()
	updated.Metadata = map[string]any{"access_token": "new"}
	return updated, nil
}

func TestRefreshAuthForRequestPersistsAfterSuccessfulExchangeCancellation(t *testing.T) {
	store := &contextCheckingRefreshStore{}
	manager := NewManager(store, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	executor := &cancelAfterSuccessRefreshExecutor{cancel: cancel}
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "rotated-refresh.json",
			Provider: executor.Identifier(),
			Metadata: map[string]any{
				"access_token":  "old-token",
				"refresh_token": "old-refresh",
			},
		},
	); errRegister != nil {
		t.Fatal(errRegister)
	}

	updated, errRefresh := manager.refreshAuthForRequest(ctx, "rotated-refresh.json", "old-token")
	if errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	if updated == nil || authAccessToken(updated) != "new-token" ||
		authRefreshOwnerToken(updated) != "new-refresh" {
		t.Fatalf("refreshed auth = %#v", updated)
	}
	store.mu.Lock()
	persisted := store.saved
	if persisted != nil {
		persisted = persisted.Clone()
	}
	store.mu.Unlock()
	if persisted == nil || authAccessToken(persisted) != "new-token" ||
		authRefreshOwnerToken(persisted) != "new-refresh" {
		t.Fatalf("persisted auth = %#v", persisted)
	}
}

func TestRefreshAuthForRequestKeepsRotatedCredentialAndRetriesPersistence(t *testing.T) {
	previousInitialDelay := deferredAuthRecreateRetryInitialDelay
	previousMaxDelay := deferredAuthRecreateRetryMaxDelay
	deferredAuthRecreateRetryInitialDelay = time.Millisecond
	deferredAuthRecreateRetryMaxDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		deferredAuthRecreateRetryInitialDelay = previousInitialDelay
		deferredAuthRecreateRetryMaxDelay = previousMaxDelay
	})

	path := filepath.Join(t.TempDir(), "rotating.json")
	store := &retryingRefreshStore{
		path:     path,
		failCall: 2,
		retried:  make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	executor := &rotatingRefreshExecutor{}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:         "rotating.json",
			Provider:   executor.Identifier(),
			Attributes: map[string]string{"path": path},
			Metadata: map[string]any{
				"access_token":  "old-access",
				"refresh_token": "old-refresh",
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	refreshed, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old-access", registered.Generation,
	)
	if !errors.Is(errRefresh, ErrAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want durable state uncertainty", errRefresh)
	}
	if refreshed == nil || authAccessToken(refreshed) != "new-access" ||
		authRefreshOwnerToken(refreshed) != "new-refresh" {
		t.Fatalf("refreshed auth = %#v", refreshed)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || authAccessToken(current) != "new-access" || authRefreshOwnerToken(current) != "new-refresh" {
		t.Fatalf("runtime auth after persistence failure = %#v", current)
	}

	select {
	case <-store.retried:
	case <-time.After(time.Second):
		t.Fatal("refreshed credential persistence was not retried")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	var persisted map[string]any
	if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if persisted["access_token"] != "new-access" || persisted["refresh_token"] != "new-refresh" {
		t.Fatalf("persisted credential = %#v", persisted)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if errStop := manager.StopDeferredAuthRecreates(stopCtx); errStop != nil {
		t.Fatalf("StopDeferredAuthRecreates() error = %v", errStop)
	}
}

func TestRefreshAuthForRequestDoesNotOverwriteExternallyChangedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-change.json")
	store := &retryingRefreshStore{path: path}
	manager := NewManager(store, nil, nil)
	external := map[string]any{
		"access_token":  "external-access",
		"refresh_token": "external-refresh",
		"disabled":      false,
	}
	executor := &rotatingRefreshExecutor{
		beforeReturn: func() error {
			raw, errMarshal := json.Marshal(external)
			if errMarshal != nil {
				return errMarshal
			}
			return os.WriteFile(path, raw, 0o600)
		},
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:         "external-change.json",
			Provider:   executor.Identifier(),
			Attributes: map[string]string{"path": path},
			Metadata: map[string]any{
				"access_token":  "old-access",
				"refresh_token": "old-refresh",
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	refreshed, errRefresh := manager.refreshAuthForRequest(
		context.Background(), registered.ID, "old-access", registered.Generation,
	)
	if !errors.Is(errRefresh, errRefreshPersistedVersionChanged) ||
		!errors.Is(errRefresh, ErrAuthDurableStateUncertain) {
		t.Fatalf("refreshAuthForRequest() error = %v, want persisted version conflict", errRefresh)
	}
	if refreshed == nil || authAccessToken(refreshed) != "new-access" ||
		authRefreshOwnerToken(refreshed) != "new-refresh" {
		t.Fatalf("runtime rotated credential = %#v", refreshed)
	}
	store.mu.Lock()
	saveCalls := store.saveCalls
	store.mu.Unlock()
	if saveCalls != 1 {
		t.Fatalf("Save() calls = %d, want registration only", saveCalls)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	var persisted map[string]any
	if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if persisted["access_token"] != "external-access" || persisted["refresh_token"] != "external-refresh" {
		t.Fatalf("external credential was overwritten: %#v", persisted)
	}
}

func TestRefreshAuthForRequestPersistsVirtualCredentialThroughPrimaryOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "primary.json")
	store := &retryingRefreshStore{path: path}
	manager := NewManager(store, nil, nil)
	sharedOwner := &struct{ Name string }{Name: "shared-owner"}
	replacementOwner := &struct{ Name string }{Name: "replacement-owner"}
	executor := &virtualPersistenceRefreshExecutor{persistenceRuntime: replacementOwner}
	manager.RegisterExecutor(executor)
	primary, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:         "primary.json",
			Provider:   executor.Identifier(),
			FileName:   "primary.json",
			Disabled:   true,
			Status:     StatusDisabled,
			Attributes: map[string]string{"path": path, "gemini_virtual_primary": "true"},
			Metadata: map[string]any{
				"access_token":  "old-access",
				"refresh_token": "old-refresh",
				"project_id":    "project-a,project-b",
			},
			Runtime: sharedOwner,
		},
	)
	if errRegister != nil {
		t.Fatalf("Register(primary) error = %v", errRegister)
	}
	virtualRuntime := &struct{ Parent any }{Parent: sharedOwner}
	virtual, errVirtual := manager.Register(
		context.Background(),
		&Auth{
			ID:       "primary.json::project-a",
			Provider: executor.Identifier(),
			Attributes: map[string]string{
				"runtime_only":          "true",
				"gemini_virtual_parent": primary.ID,
				"path":                  path,
			},
			Metadata: map[string]any{"project_id": "project-a", "virtual": true},
			Runtime:  virtualRuntime,
		},
	)
	if errVirtual != nil {
		t.Fatalf("Register(virtual) error = %v", errVirtual)
	}

	refreshed, errRefresh := manager.refreshAuthForRequest(
		context.Background(), virtual.ID, "", virtual.Generation,
	)
	if errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	if refreshed == nil || refreshed.Runtime != virtualRuntime || refreshed.Metadata["project_id"] != "project-a" {
		t.Fatalf("refreshed virtual auth = %#v", refreshed)
	}
	persistedPrimary, ok := manager.GetByID(primary.ID)
	if !ok || persistedPrimary.Runtime != sharedOwner {
		t.Fatalf("primary runtime owner changed: %#v", persistedPrimary)
	}
	if persistedPrimary.Metadata["access_token"] != "new-access" ||
		persistedPrimary.Metadata["refresh_token"] != "new-refresh" {
		t.Fatalf("primary metadata = %#v", persistedPrimary.Metadata)
	}
	store.mu.Lock()
	saveCalls := store.saveCalls
	store.mu.Unlock()
	if saveCalls != 2 {
		t.Fatalf("Save() calls = %d, want primary registration plus refresh", saveCalls)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	var persisted map[string]any
	if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if persisted["access_token"] != "new-access" || persisted["refresh_token"] != "new-refresh" {
		t.Fatalf("persisted primary credential = %#v", persisted)
	}
}

func TestRefreshAuthForRequestWaitIsCancelable(t *testing.T) {
	executor := &blockingRefreshExecutor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(
		context.Background(),
		&Auth{
			ID:       "auth-refresh-cancel",
			Provider: executor.Identifier(),
			Metadata: map[string]any{"access_token": "old"},
		},
	); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.refreshAuthForRequest(context.Background(), "auth-refresh-cancel", "old")
		firstDone <- err
	}()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := manager.refreshAuthForRequest(waitCtx, "auth-refresh-cancel", "old")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting refresh error = %v, want deadline exceeded", err)
	}
	close(executor.release)
	select {
	case err = <-firstDone:
		if err != nil {
			t.Fatalf("first refresh error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first refresh did not finish")
	}
	manager.refreshLocksMu.Lock()
	remainingLocks := len(manager.refreshLocks)
	manager.refreshLocksMu.Unlock()
	if remainingLocks != 0 {
		t.Fatalf("refresh lock entries = %d, want 0", remainingLocks)
	}
}

func TestManager_Update_WithModelStatesPreservesModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			ModelStates: map[string]*ModelState{
				model: {
					Quota: QuotaState{BackoffLevel: backoffLevel},
				},
			},
		},
	); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	// Update with ModelStates included preserves them
	if _, errUpdate := m.Update(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			Metadata: map[string]any{"k": "v2"},
			ModelStates: map[string]*ModelState{
				model: {
					Quota: QuotaState{BackoffLevel: backoffLevel},
				},
			},
		},
	); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected ModelStates to be preserved")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}
