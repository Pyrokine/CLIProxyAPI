package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type reentrantUpdateHook struct {
	manager *Manager
	entered atomic.Bool
	err     chan error
}

func (*reentrantUpdateHook) OnAuthRegistered(context.Context, *Auth) {}

func (h *reentrantUpdateHook) OnAuthUpdated(ctx context.Context, auth *Auth) {
	if h == nil || h.manager == nil || auth == nil || !h.entered.CompareAndSwap(false, true) {
		return
	}
	updated := auth.Clone()
	updated.Label = "updated-by-hook"
	_, errUpdate := h.manager.Update(ctx, updated)
	h.err <- errUpdate
}

func (*reentrantUpdateHook) OnResult(context.Context, Result) {}

type blockingAuthStore struct {
	mu      sync.Mutex
	last    *Auth
	block   bool
	entered chan struct{}
	release chan struct{}
	deletes atomic.Int32
}

func (s *blockingAuthStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *blockingAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	block := s.block
	entered := s.entered
	release := s.release
	s.mu.Unlock()
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	s.mu.Lock()
	s.last = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *blockingAuthStore) Delete(context.Context, string) error {
	s.deletes.Add(1)
	return nil
}

type versionedDeleteCaptureStore struct {
	deleteID string
}

func (*versionedDeleteCaptureStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (*versionedDeleteCaptureStore) Save(context.Context, *Auth) (string, error) {
	return "", nil
}

func (*versionedDeleteCaptureStore) Delete(context.Context, string) error {
	return nil
}

func (s *versionedDeleteCaptureStore) DeleteIfVersion(
	_ context.Context,
	id string,
	_ PersistedVersion,
) (bool, error) {
	s.deleteID = id
	return true, nil
}

type blockingPluginScheduler struct {
	authID  string
	entered chan struct{}
	release chan struct{}
}

type blockingSelector struct {
	authID  string
	entered chan struct{}
	release chan struct{}
}

func (s *blockingSelector) Pick(
	ctx context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
	}
	for _, auth := range auths {
		if auth != nil && auth.ID == s.authID {
			return auth.Clone(), nil
		}
	}
	return nil, nil
}

func (s *blockingPluginScheduler) PickAuth(
	ctx context.Context,
	_ pluginapi.SchedulerPickRequest,
) (pluginapi.SchedulerPickResponse, bool, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return pluginapi.SchedulerPickResponse{}, true, ctx.Err()
	case <-s.release:
		return pluginapi.SchedulerPickResponse{AuthID: s.authID, Handled: true}, true, nil
	}
}

func TestUpdateReleasesMutationLockBeforeHook(t *testing.T) {
	hook := &reentrantUpdateHook{err: make(chan error, 1)}
	manager := NewManager(nil, nil, hook)
	hook.manager = manager
	auth := &Auth{
		ID:       "hook-reentry",
		Provider: "plugin-provider",
		Metadata: map[string]any{"token": "old"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	updated := auth.Clone()
	updated.Metadata["token"] = "new"
	done := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(context.Background(), updated)
		done <- errUpdate
	}()
	select {
	case errUpdate := <-done:
		if errUpdate != nil {
			t.Fatalf("Update() error = %v", errUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("Update deadlocked when hook re-entered the same auth")
	}
	select {
	case errHook := <-hook.err:
		if errHook != nil {
			t.Fatalf("reentrant hook Update() error = %v", errHook)
		}
	case <-time.After(time.Second):
		t.Fatal("reentrant hook update did not run")
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Label != "updated-by-hook" {
		t.Fatalf("reentrant hook update was not published: %#v", current)
	}
}

func TestMarkResultPublishesCooldownToScheduler(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "scheduler-cooldown",
		Provider: "plugin-provider",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: "plugin-model"}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.scheduler.upsertAuth(auth.Clone())
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), auth.Provider, "plugin-model", cliproxyexecutor.Options{}, nil,
	); errPick != nil || picked == nil || picked.ID != auth.ID {
		t.Fatalf("initial scheduler pick = %#v, error = %v", picked, errPick)
	}

	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:     auth.ID,
			Model:      "plugin-model",
			Error:      &Error{HTTPStatus: 429, Message: "quota exhausted"},
			RetryAfter: &retryAfter,
		},
	)
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), auth.Provider, "plugin-model", cliproxyexecutor.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("scheduler selected cooling auth: picked=%#v error=%v", picked, errPick)
	}
}

func TestMarkResultPublishesCooldownBeforePersistenceCompletes(t *testing.T) {
	store := &blockingAuthStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "scheduler-persist-cooldown",
		Provider: "plugin-provider",
		Status:   StatusActive,
		Metadata: map[string]any{"token": "test"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: "plugin-model"}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	store.mu.Lock()
	store.block = true
	store.mu.Unlock()

	markDone := make(chan struct{})
	retryAfter := time.Hour
	go func() {
		manager.MarkResult(
			context.Background(), Result{
				AuthID:     auth.ID,
				Model:      "plugin-model",
				Error:      &Error{HTTPStatus: 429, Message: "quota exhausted"},
				RetryAfter: &retryAfter,
			},
		)
		close(markDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("MarkResult did not enter persistence")
	}
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), auth.Provider, "plugin-model", cliproxyexecutor.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("scheduler selected cooling auth during persistence: picked=%#v error=%v", picked, errPick)
	}
	close(store.release)
	select {
	case <-markDone:
	case <-time.After(time.Second):
		t.Fatal("MarkResult did not finish")
	}
}

func TestMarkResultRejectsSuccessFromBeforeQuotaBarrier(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "quota-result-order",
		Provider: "plugin-provider",
		Status:   StatusActive,
	}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "plugin-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	manager.scheduler.upsertAuth(registered.Clone())

	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 0,
			Error:           &Error{HTTPStatus: 429, Message: "quota exhausted"},
			RetryAfter:      &retryAfter,
		},
	)
	cooling, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("cooling auth missing")
	}
	state := cooling.ModelStates[model]
	if state == nil || !state.Quota.Exceeded || state.QuotaEpoch != 1 {
		t.Fatalf("quota barrier state = %#v, want exceeded at epoch 1", state)
	}
	backoffLevel := state.Quota.BackoffLevel

	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 0,
			Success:         true,
		},
	)
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 0,
			Error:           &Error{HTTPStatus: 429, Message: "duplicate quota exhausted"},
		},
	)
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 0,
			Error:           &Error{HTTPStatus: 401, Message: "stale unauthorized"},
		},
	)

	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing after stale results")
	}
	state = current.ModelStates[model]
	if state == nil || !state.Quota.Exceeded || state.QuotaEpoch != 1 {
		t.Fatalf("stale result cleared quota barrier: %#v", state)
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("stale 429 backoff level = %d, want %d", state.Quota.BackoffLevel, backoffLevel)
	}
	if state.LastError == nil || state.LastError.HTTPStatus != 401 {
		t.Fatalf("stale non-quota error was not retained: %#v", state.LastError)
	}
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), auth.Provider, model, cliproxyexecutor.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("scheduler selected auth after stale success: picked=%#v error=%v", picked, errPick)
	}
	if available, errAvailable := reg.GetFirstAvailableModel(auth.Provider); errAvailable == nil || available != "" {
		t.Fatalf("registry model resumed after stale success: model=%q error=%v", available, errAvailable)
	}
}

func TestStreamSuccessFromBeforeQuotaBarrierDoesNotResumeModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "quota-stream-order", Provider: "plugin-provider", Status: StatusActive}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "plugin-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	manager.scheduler.upsertAuth(registered.Clone())

	remaining := make(chan cliproxyexecutor.StreamChunk)
	stream := manager.wrapStreamResult(
		context.Background(), registered.Clone(), auth.Provider, model, nil, nil, remaining, OAuthModelAliasResult{},
	)
	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 0,
			Error:           &Error{HTTPStatus: 429, Message: "quota exhausted"},
			RetryAfter:      &retryAfter,
		},
	)
	close(remaining)
	for range stream.Chunks {
	}

	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing after stale stream success")
	}
	state := current.ModelStates[model]
	if state == nil || !state.Quota.Exceeded || state.QuotaEpoch != 1 {
		t.Fatalf("stale stream success cleared quota barrier: %#v", state)
	}
	if available, errAvailable := reg.GetFirstAvailableModel(auth.Provider); errAvailable == nil || available != "" {
		t.Fatalf("registry model resumed after stale stream success: model=%q error=%v", available, errAvailable)
	}
}

func TestCurrentQuotaEpochSuccessRecoversAfterCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "quota-current-epoch", Provider: "plugin-provider", Status: StatusActive}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "plugin-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	manager.scheduler.upsertAuth(registered.Clone())

	retryAfter := time.Millisecond
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          auth.ID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 0,
			Error:           &Error{HTTPStatus: 429, Message: "quota exhausted"},
			RetryAfter:      &retryAfter,
		},
	)
	time.Sleep(10 * time.Millisecond)
	picked, errPick := manager.scheduler.pickSingle(
		context.Background(), auth.Provider, model, cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil {
		t.Fatalf("pickSingle() = (%#v, %v), want recovered auth", picked, errPick)
	}
	if epoch := modelQuotaEpoch(picked, model); epoch != 1 {
		t.Fatalf("picked quota epoch = %d, want 1", epoch)
	}
	manager.MarkResult(
		context.Background(), Result{
			AuthID:          picked.ID,
			AuthGeneration:  picked.Generation,
			Model:           model,
			ModelQuotaEpoch: modelQuotaEpoch(picked, model),
			Success:         true,
		},
	)

	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing after current epoch success")
	}
	state := current.ModelStates[model]
	if state == nil || state.Quota.Exceeded || state.Unavailable || state.QuotaEpoch != 1 {
		t.Fatalf("current epoch success did not recover model: %#v", state)
	}
	if available, errAvailable := reg.GetFirstAvailableModel(auth.Provider); errAvailable != nil || available != model {
		t.Fatalf("registry model after recovery = %q, %v; want %q", available, errAvailable, model)
	}
}

func TestRemovePreventsInFlightMarkResultSideEffects(t *testing.T) {
	store := &blockingAuthStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "remove-during-result",
		Provider: "plugin-provider",
		Status:   StatusActive,
		Metadata: map[string]any{"token": "test"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "plugin-model-remove"
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: model}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	store.mu.Lock()
	store.block = true
	store.mu.Unlock()

	markDone := make(chan struct{})
	go func() {
		manager.MarkResult(
			context.Background(), Result{
				AuthID: auth.ID,
				Model:  model,
				Error:  &Error{HTTPStatus: 404, Message: "model not found"},
			},
		)
		close(markDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("MarkResult did not enter persistence")
	}

	removeDone := make(chan struct{})
	go func() {
		manager.Remove(context.Background(), auth.ID)
		close(removeDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := manager.GetByID(auth.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Remove did not remove auth before waiting for MarkResult")
		}
		time.Sleep(time.Millisecond)
	}
	close(store.release)
	select {
	case <-markDone:
	case <-time.After(time.Second):
		t.Fatal("MarkResult did not finish")
	}
	select {
	case <-removeDone:
	case <-time.After(time.Second):
		t.Fatal("Remove did not finish")
	}

	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), auth.Provider, model, cliproxyexecutor.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("scheduler retained removed auth: picked=%#v error=%v", picked, errPick)
	}
	available := registry.GetGlobalRegistry().GetAvailableModelsByProvider(auth.Provider)
	found := false
	for _, item := range available {
		if item != nil && item.ID == model {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("in-flight MarkResult suspended the registry model after auth removal")
	}
}

func TestPickNextRejectsPluginSchedulerAuthCoolingAfterSnapshot(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerTestExecutor{})
	auth := &Auth{ID: "stale-scheduler-auth", Provider: "test", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "stale-scheduler-model"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	scheduler := &blockingPluginScheduler{
		authID:  auth.ID,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager.SetPluginScheduler(scheduler)

	type pickResult struct {
		auth *Auth
		err  error
	}
	picked := make(chan pickResult, 1)
	go func() {
		selected, _, errPick := manager.PickNext(
			context.Background(), auth.Provider, model, cliproxyexecutor.Options{}, nil,
		)
		picked <- pickResult{auth: selected, err: errPick}
	}()
	select {
	case <-scheduler.entered:
	case <-time.After(time.Second):
		t.Fatal("plugin scheduler did not receive the auth snapshot")
	}
	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:     auth.ID,
			Model:      model,
			Error:      &Error{HTTPStatus: 429, Message: "quota exhausted"},
			RetryAfter: &retryAfter,
		},
	)
	close(scheduler.release)
	select {
	case result := <-picked:
		if result.auth != nil || result.err == nil {
			t.Fatalf("PickNext() returned stale cooling auth=%#v error=%v", result.auth, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("PickNext did not finish")
	}
}

func TestPickNextRejectsCustomSelectorAuthRemovedAfterSnapshot(t *testing.T) {
	selector := &blockingSelector{
		authID:  "stale-custom-auth",
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{})
	auth := &Auth{ID: selector.authID, Provider: "test", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "stale-custom-model"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	type pickResult struct {
		auth *Auth
		err  error
	}
	picked := make(chan pickResult, 1)
	go func() {
		selected, _, errPick := manager.PickNext(
			context.Background(), auth.Provider, model, cliproxyexecutor.Options{}, nil,
		)
		picked <- pickResult{auth: selected, err: errPick}
	}()
	select {
	case <-selector.entered:
	case <-time.After(time.Second):
		t.Fatal("custom selector did not receive the auth snapshot")
	}
	manager.Remove(context.Background(), auth.ID)
	close(selector.release)
	select {
	case result := <-picked:
		if result.auth != nil || result.err == nil {
			t.Fatalf("PickNext() returned removed auth=%#v error=%v", result.auth, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("PickNext did not finish")
	}
}

func TestPickNextMixedRejectsCustomSelectorAuthCoolingAfterSnapshot(t *testing.T) {
	selector := &blockingSelector{
		authID:  "stale-custom-mixed-auth",
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{})
	auth := &Auth{ID: selector.authID, Provider: "test", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const model = "stale-custom-mixed-model"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	type pickResult struct {
		auth *Auth
		err  error
	}
	picked := make(chan pickResult, 1)
	go func() {
		selected, _, _, errPick := manager.pickNextMixed(
			context.Background(), []string{auth.Provider}, model, cliproxyexecutor.Options{}, nil,
		)
		picked <- pickResult{auth: selected, err: errPick}
	}()
	select {
	case <-selector.entered:
	case <-time.After(time.Second):
		t.Fatal("custom mixed selector did not receive the auth snapshot")
	}
	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(), Result{
			AuthID:     auth.ID,
			Model:      model,
			Error:      &Error{HTTPStatus: 429, Message: "quota exhausted"},
			RetryAfter: &retryAfter,
		},
	)
	close(selector.release)
	select {
	case result := <-picked:
		if result.auth != nil || result.err == nil {
			t.Fatalf("pickNextMixed() returned cooling auth=%#v error=%v", result.auth, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("pickNextMixed did not finish")
	}
}

func TestLockAuthMutationStopsWaitingAfterContextCancellation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const authID = "canceled-mutation-lock"
	unlockMutation, errMutation := manager.lockAuthMutation(context.Background(), authID)
	if errMutation != nil {
		t.Fatalf("initial lockAuthMutation() error = %v", errMutation)
	}
	defer unlockMutation()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlockCanceled, errCanceled := manager.lockAuthMutation(ctx, authID)
	if unlockCanceled != nil {
		unlockCanceled()
	}
	if !errors.Is(errCanceled, context.Canceled) {
		t.Fatalf("canceled lockAuthMutation() error = %v, want context.Canceled", errCanceled)
	}
}

func TestRemoveIfGenerationPreservesReplacementPublishedWhileWaitingForMutationLock(t *testing.T) {
	store := &blockingAuthStore{}
	manager := NewManager(store, nil, nil)
	const authID = "conditional-remove-replacement"
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "old"}},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	store.mu.Lock()
	store.block = true
	store.entered = make(chan struct{}, 1)
	store.release = make(chan struct{})
	store.mu.Unlock()

	replacement := registered.Clone()
	replacement.Metadata["token"] = "new"
	updateDone := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(context.Background(), replacement)
		updateDone <- errUpdate
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("replacement update did not enter persistence")
	}

	type removeResult struct {
		removed bool
		err     error
	}
	removeDone := make(chan removeResult, 1)
	go func() {
		removed, errRemove := manager.RemoveIfGeneration(
			context.Background(),
			authID,
			registered.Generation,
		)
		removeDone <- removeResult{removed: removed, err: errRemove}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		manager.authMutationLocksMu.Lock()
		lock := manager.authMutationLocks[authID]
		waiting := lock != nil && lock.refs >= 2
		manager.authMutationLocksMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conditional removal did not wait for the replacement mutation")
		}
		time.Sleep(time.Millisecond)
	}
	close(store.release)

	select {
	case errUpdate := <-updateDone:
		if errUpdate != nil {
			t.Fatalf("replacement Update() error = %v", errUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement update did not finish")
	}
	select {
	case result := <-removeDone:
		if result.err != nil {
			t.Fatalf("RemoveIfGeneration() error = %v", result.err)
		}
		if result.removed {
			t.Fatal("RemoveIfGeneration() removed the replacement generation")
		}
	case <-time.After(time.Second):
		t.Fatal("conditional removal did not finish")
	}
	if got := store.deletes.Load(); got != 0 {
		t.Fatalf("store deletes = %d, want 0", got)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current.Generation == registered.Generation || current.Metadata["token"] != "new" {
		t.Fatalf("replacement auth = %#v, ok = %v", current, ok)
	}
}

func TestRemoveDeletesExplicitRecreatePublishedWhileWaitingForMutationLock(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const authID = "remove-recreate-race"
	if _, errRegister := manager.Register(
		context.Background(), &Auth{ID: authID, Provider: "test"},
	); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	unlockMutation, errMutation := manager.lockAuthMutation(context.Background(), authID)
	if errMutation != nil {
		t.Fatalf("lockAuthMutation() error = %v", errMutation)
	}
	recreateStarted := make(chan struct{})
	recreateDone := make(chan error, 1)
	go func() {
		close(recreateStarted)
		_, errRegister := manager.Register(
			WithAllowAuthRecreate(context.Background()),
			&Auth{ID: authID, Provider: "test", Metadata: map[string]any{"token": "new"}},
		)
		recreateDone <- errRegister
	}()
	<-recreateStarted
	time.Sleep(20 * time.Millisecond)

	removeDone := make(chan struct{})
	go func() {
		manager.Remove(context.Background(), authID)
		close(removeDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		_, removed := manager.removedAuths[authID]
		manager.mu.RUnlock()
		if removed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Remove did not publish its initial tombstone")
		}
		time.Sleep(time.Millisecond)
	}
	unlockMutation()

	select {
	case errRegister := <-recreateDone:
		if errRegister != nil {
			t.Fatalf("explicit recreate error = %v", errRegister)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit recreate did not finish")
	}
	select {
	case <-removeDone:
	case <-time.After(time.Second):
		t.Fatal("Remove did not finish")
	}
	if current, ok := manager.GetByID(authID); ok {
		t.Fatalf("Remove returned with recreated auth still present: %#v", current)
	}
}

func TestMergePersistentUpdateConditionalRejectsBeforePersistence(t *testing.T) {
	store := &blockingAuthStore{}
	manager := NewManager(store, nil, nil)
	original := &Auth{
		ID:       "conditional-precheck",
		Provider: "plugin-provider",
		Metadata: map[string]any{"token": "old"},
	}
	registered, errRegister := manager.Register(context.Background(), original)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	update := registered.Clone()
	update.Metadata["token"] = "stale"
	merged, errMerge := manager.MergePersistentUpdateConditional(
		context.Background(), registered, update,
		func(func() bool) bool { return false },
	)
	if errMerge != nil {
		t.Fatalf("MergePersistentUpdateConditional() error = %v", errMerge)
	}
	if merged != nil {
		t.Fatalf("MergePersistentUpdateConditional() = %#v, want rejected update", merged)
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["token"] != "old" {
		t.Fatalf("rejected update changed persistence: %#v", persisted)
	}
}

func TestMutatePersistentAuthPublishesCompletedMutationAfterCancellation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	updated, errMutate := manager.MutatePersistentAuth(
		ctx,
		"cancelled-after-persist",
		func(context.Context) (*Auth, error) {
			cancel()
			return &Auth{
				ID:       "cancelled-after-persist",
				Provider: "codex",
				Metadata: map[string]any{"token": "published"},
			}, nil
		},
	)
	if errMutate != nil {
		t.Fatalf("MutatePersistentAuth() error = %v", errMutate)
	}
	if updated == nil || updated.Metadata["token"] != "published" {
		t.Fatalf("updated auth = %#v, want published mutation", updated)
	}
	current, ok := manager.GetByID("cancelled-after-persist")
	if !ok || current.Metadata["token"] != "published" {
		t.Fatalf("current auth = %#v, want published mutation", current)
	}
}

func TestDeletePersistedAuthIfMatchUsesCapturedVersionPath(t *testing.T) {
	store := &versionedDeleteCaptureStore{}
	manager := NewManager(store, nil, nil)
	version := &PersistedVersion{Path: "/auth/plugin.json"}
	deleted, errDelete := manager.deletePersistedAuthIfMatch(
		context.Background(),
		"account-123",
		nil,
		version,
	)
	if errDelete != nil || !deleted {
		t.Fatalf("deletePersistedAuthIfMatch() = (%t, %v)", deleted, errDelete)
	}
	if store.deleteID != version.Path {
		t.Fatalf("DeleteIfVersion() id = %q, want %q", store.deleteID, version.Path)
	}
}

func TestMutatePersistentAuthPreservesRuntimeForSameCredentialOwner(t *testing.T) {
	runtimeState := &struct{ account string }{account: "a"}
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "gemini-runtime-same-owner",
			Provider: "gemini-cli",
			Metadata: map[string]any{
				"token": map[string]any{"refresh_token": "account-a-refresh"},
			},
			Runtime: runtimeState,
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	updated, errMutate := manager.MutatePersistentAuth(
		context.Background(),
		registered.ID,
		func(context.Context) (*Auth, error) {
			replacement := registered.Clone()
			replacement.Label = "updated"
			replacement.Runtime = nil
			return replacement, nil
		},
	)
	if errMutate != nil {
		t.Fatal(errMutate)
	}
	if updated == nil || updated.Runtime != runtimeState {
		t.Fatalf("updated auth runtime = %#v", updated)
	}
}

func TestMutatePersistentAuthClearsRuntimeWhenCredentialOwnerChanges(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "gemini-runtime-owner",
			Provider: "gemini-cli",
			Metadata: map[string]any{
				"token": map[string]any{"refresh_token": "account-a-refresh"},
			},
			Runtime: &struct{ account string }{account: "a"},
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	updated, errMutate := manager.MutatePersistentAuth(
		context.Background(),
		registered.ID,
		func(context.Context) (*Auth, error) {
			replacement := registered.Clone()
			replacement.Metadata = map[string]any{
				"token": map[string]any{"refresh_token": "account-b-refresh"},
			}
			replacement.Runtime = nil
			return replacement, nil
		},
	)
	if errMutate != nil {
		t.Fatal(errMutate)
	}
	if updated == nil || updated.Runtime != nil || authRefreshOwnerToken(updated) != "account-b-refresh" {
		t.Fatalf("updated auth = %#v", updated)
	}
}

func TestMutatePersistentAuthSerializesManagementSaveWithPluginUpdate(t *testing.T) {
	store := &blockingAuthStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	original := &Auth{
		ID:       "serialized-management-save",
		Provider: "plugin-provider",
		Metadata: map[string]any{"token": "old"},
	}
	registered, errRegister := manager.Register(context.Background(), original)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store.mu.Lock()
	store.block = true
	store.mu.Unlock()
	pluginUpdate := registered.Clone()
	pluginUpdate.Metadata["token"] = "plugin"
	pluginDone := make(chan error, 1)
	go func() {
		_, errMerge := manager.MergePersistentUpdateConditional(
			context.Background(), registered, pluginUpdate,
			func(reserve func() bool) bool { return reserve() },
		)
		pluginDone <- errMerge
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("plugin update did not enter persistence")
	}

	managementEntered := make(chan struct{}, 1)
	managementDone := make(chan error, 1)
	go func() {
		_, errMutate := manager.MutatePersistentAuth(
			context.Background(), original.ID,
			func(context.Context) (*Auth, error) {
				managementEntered <- struct{}{}
				management := registered.Clone()
				management.Metadata["token"] = "management"
				store.mu.Lock()
				store.last = management.Clone()
				store.mu.Unlock()
				return management, nil
			},
		)
		managementDone <- errMutate
	}()
	select {
	case <-managementEntered:
		t.Fatal("management save bypassed the in-flight plugin mutation")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	select {
	case errMerge := <-pluginDone:
		if errMerge != nil {
			t.Fatalf("plugin update error = %v", errMerge)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin update did not finish")
	}
	select {
	case errMutate := <-managementDone:
		if errMutate != nil {
			t.Fatalf("management mutation error = %v", errMutate)
		}
	case <-time.After(time.Second):
		t.Fatal("management mutation did not finish")
	}
	current, ok := manager.GetByID(original.ID)
	if !ok || current.Metadata["token"] != "management" {
		t.Fatalf("current auth after serialized saves = %#v", current)
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["token"] != "management" {
		t.Fatalf("persisted auth after serialized saves = %#v", persisted)
	}
}

type blockingRequestAuthPreparer struct {
	replaceAwareExecutor
	entered chan *Auth
	release chan struct{}
	calls   atomic.Int32
}

func (p *blockingRequestAuthPreparer) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth != nil && strings.TrimSpace(auth.Attributes["project_id"]) == ""
}

func (p *blockingRequestAuthPreparer) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	call := p.calls.Add(1)
	if call == 1 {
		select {
		case p.entered <- auth.Clone():
		case <-ctx.Done():
			return auth, ctx.Err()
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return auth, ctx.Err()
		}
	}
	updated := auth.Clone()
	if updated.Attributes == nil {
		updated.Attributes = make(map[string]string)
	}
	updated.Attributes["project_id"] = contextStringValue(updated.Metadata["token"])
	return updated, nil
}

func TestMarkResultRejectsStaleAuthGeneration(t *testing.T) {
	store := &blockingAuthStore{}
	manager := NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "stale-result",
			Provider: "plugin-provider",
			Status:   StatusActive,
			Metadata: map[string]any{"token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	updated := registered.Clone()
	updated.Metadata["token"] = "new"
	current, errUpdate := manager.Update(context.Background(), updated)
	if errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	if current == nil || current.Generation == registered.Generation {
		t.Fatalf("credential update did not advance generation: old=%d current=%#v", registered.Generation, current)
	}

	retryAfter := time.Hour
	manager.MarkResult(
		context.Background(),
		Result{
			AuthID:         registered.ID,
			AuthGeneration: registered.Generation,
			Provider:       registered.Provider,
			Model:          "plugin-model",
			Error:          &Error{HTTPStatus: 429, Message: "old request"},
			RetryAfter:     &retryAfter,
		},
	)

	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["token"] != "new" {
		t.Fatalf("stale result changed current credential: %#v", current)
	}
	if current.Failed != 0 || len(current.ModelStates) != 0 {
		t.Fatalf("stale result changed current runtime state: %#v", current)
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["token"] != "new" || len(persisted.ModelStates) != 0 {
		t.Fatalf("stale result changed persisted state: %#v", persisted)
	}

	manager.Remove(context.Background(), registered.ID)
	recreated, errRecreate := manager.Register(
		WithAllowAuthRecreate(context.Background()),
		&Auth{
			ID:       registered.ID,
			Provider: registered.Provider,
			Status:   StatusActive,
			Metadata: map[string]any{"token": "recreated"},
		},
	)
	if errRecreate != nil {
		t.Fatalf("Register(recreate) error = %v", errRecreate)
	}
	manager.MarkResult(
		context.Background(),
		Result{
			AuthID:         registered.ID,
			AuthGeneration: registered.Generation,
			Provider:       registered.Provider,
			Success:        true,
		},
	)
	recreated, ok = manager.GetByID(registered.ID)
	if !ok || recreated.Metadata["token"] != "recreated" || recreated.Success != 0 {
		t.Fatalf("stale result changed recreated auth: %#v", recreated)
	}
}

func TestMarkResultDoesNotMutateManagedAuthForRuntimeCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "shared-id", Provider: "antigravity", Status: StatusActive},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.MarkResult(
		context.Background(),
		Result{
			AuthID:      registered.ID,
			RuntimeOnly: true,
			Provider:    registered.Provider,
			Model:       "runtime-model",
			Error:       &Error{HTTPStatus: 429, Message: "runtime quota"},
		},
	)
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil {
		t.Fatal("managed auth disappeared")
	}
	if current.Failed != 0 || len(current.ModelStates) != 0 {
		t.Fatalf("runtime result changed managed auth: %#v", current)
	}
}

func TestPrepareRequestAuthKeepsRuntimeCredentialIsolated(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	managed, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:         "shared-id",
			Provider:   "antigravity",
			Status:     StatusActive,
			Attributes: map[string]string{"project_id": "managed-project"},
			Metadata:   map[string]any{"token": "managed-token"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	release := make(chan struct{})
	close(release)
	preparer := &blockingRequestAuthPreparer{
		replaceAwareExecutor: replaceAwareExecutor{id: "antigravity"},
		entered:              make(chan *Auth, 1),
		release:              release,
	}
	runtimeAuth := &Auth{
		ID:         managed.ID,
		Provider:   managed.Provider,
		Status:     StatusActive,
		Attributes: map[string]string{},
		Metadata:   map[string]any{"token": "runtime-token"},
	}
	prepared, errPrepare := manager.prepareRequestAuth(context.Background(), preparer, runtimeAuth)
	if errPrepare != nil {
		t.Fatalf("prepareRequestAuth() error = %v", errPrepare)
	}
	if prepared == nil || prepared.Generation != 0 || prepared.Metadata["token"] != "runtime-token" ||
		prepared.Attributes["project_id"] != "runtime-token" {
		t.Fatalf("runtime auth was replaced by managed snapshot: %#v", prepared)
	}
	current, ok := manager.GetByID(managed.ID)
	if !ok || current.Metadata["token"] != "managed-token" || current.Attributes["project_id"] != "managed-project" {
		t.Fatalf("runtime preparation changed managed auth: %#v", current)
	}
}

func TestPrepareRequestAuthRetriesAfterConcurrentCredentialUpdate(t *testing.T) {
	store := &blockingAuthStore{}
	manager := NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:         "prepare-update",
			Provider:   "antigravity",
			Status:     StatusActive,
			Attributes: map[string]string{"account": "old"},
			Metadata:   map[string]any{"token": "old"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	preparer := &blockingRequestAuthPreparer{
		replaceAwareExecutor: replaceAwareExecutor{id: "antigravity"},
		entered:              make(chan *Auth, 1),
		release:              make(chan struct{}),
	}
	preparedCh := make(chan *Auth, 1)
	errCh := make(chan error, 1)
	go func() {
		prepared, errPrepare := manager.prepareRequestAuth(context.Background(), preparer, registered)
		preparedCh <- prepared
		errCh <- errPrepare
	}()

	var first *Auth
	select {
	case first = <-preparer.entered:
	case <-time.After(time.Second):
		t.Fatal("request preparation did not start")
	}
	if first.Metadata["token"] != "old" {
		t.Fatalf("first preparation token = %v, want old", first.Metadata["token"])
	}
	concurrent := registered.Clone()
	concurrent.Metadata["token"] = "new"
	concurrent.Attributes["account"] = "new"
	if _, errUpdate := manager.Update(context.Background(), concurrent); errUpdate != nil {
		t.Fatalf("concurrent Update() error = %v", errUpdate)
	}
	close(preparer.release)

	var prepared *Auth
	select {
	case prepared = <-preparedCh:
	case <-time.After(time.Second):
		t.Fatal("request preparation did not finish")
	}
	if errPrepare := <-errCh; errPrepare != nil {
		t.Fatalf("prepareRequestAuth() error = %v", errPrepare)
	}
	if preparer.calls.Load() != 2 {
		t.Fatalf("PrepareRequestAuth() calls = %d, want 2", preparer.calls.Load())
	}
	if prepared == nil || prepared.Metadata["token"] != "new" || prepared.Attributes["account"] != "new" ||
		prepared.Attributes["project_id"] != "new" {
		t.Fatalf("prepared auth did not use current credential: %#v", prepared)
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["token"] != "new" || persisted.Attributes["account"] != "new" ||
		persisted.Attributes["project_id"] != "new" {
		t.Fatalf("persisted auth did not use current credential: %#v", persisted)
	}
}

func TestPerAuthOperationLocksAreReleasedAfterUse(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	unlockMutation, errLock := manager.lockAuthMutation(context.Background(), "lock-cleanup")
	if errLock != nil {
		t.Fatalf("lockAuthMutation() error = %v", errLock)
	}
	unlockMutation()
	manager.authMutationLocksMu.Lock()
	mutationLocks := len(manager.authMutationLocks)
	manager.authMutationLocksMu.Unlock()
	if mutationLocks != 0 {
		t.Fatalf("auth mutation lock entries = %d, want 0", mutationLocks)
	}

	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:         "operation-lock-cleanup",
			Provider:   "antigravity",
			Status:     StatusActive,
			Attributes: map[string]string{},
			Metadata:   map[string]any{"token": "value"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	release := make(chan struct{})
	close(release)
	preparer := &blockingRequestAuthPreparer{
		replaceAwareExecutor: replaceAwareExecutor{id: "antigravity"},
		entered:              make(chan *Auth, 1),
		release:              release,
	}
	prepared, errPrepare := manager.prepareRequestAuth(context.Background(), preparer, registered)
	if errPrepare != nil || prepared == nil {
		t.Fatalf("prepareRequestAuth() = (%#v, %v)", prepared, errPrepare)
	}
	manager.requestPrepareLocksMu.Lock()
	prepareLocks := len(manager.requestPrepareLocks)
	manager.requestPrepareLocksMu.Unlock()
	if prepareLocks != 0 {
		t.Fatalf("request prepare lock entries = %d, want 0", prepareLocks)
	}

	manager.RegisterExecutor(&replaceAwareExecutor{id: "antigravity"})
	current, ok := manager.GetByID(registered.ID)
	if !ok {
		t.Fatal("prepared auth disappeared")
	}
	if _, errRefresh := manager.refreshAuthForRequest(
		context.Background(),
		current.ID,
		"",
		current.Generation,
	); errRefresh != nil && !strings.Contains(errRefresh.Error(), "unchanged credentials") {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	manager.refreshLocksMu.Lock()
	refreshLocks := len(manager.refreshLocks)
	manager.refreshLocksMu.Unlock()
	if refreshLocks != 0 {
		t.Fatalf("refresh lock entries = %d, want 0", refreshLocks)
	}
}

func TestUpdateDoesNotOverwriteConcurrentResultState(t *testing.T) {
	store := &blockingAuthStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	original := &Auth{
		ID:       "concurrent-result",
		Provider: "plugin-provider",
		Status:   StatusActive,
		Metadata: map[string]any{"token": "old"},
	}
	registered, errRegister := manager.Register(context.Background(), original)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store.mu.Lock()
	store.block = true
	store.mu.Unlock()
	updated := registered.Clone()
	updated.Metadata["token"] = "new"
	updateDone := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(context.Background(), updated)
		updateDone <- errUpdate
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("persistent update did not enter persistence")
	}

	retryAfter := time.Hour
	resultDone := make(chan struct{})
	go func() {
		manager.MarkResult(
			context.Background(), Result{
				AuthID:     original.ID,
				Model:      "plugin-model",
				Error:      &Error{HTTPStatus: 429, Message: "quota exhausted"},
				RetryAfter: &retryAfter,
			},
		)
		close(resultDone)
	}()
	select {
	case <-resultDone:
		t.Fatal("MarkResult bypassed the in-flight persistent update")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	select {
	case errUpdate := <-updateDone:
		if errUpdate != nil {
			t.Fatalf("Update() error = %v", errUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("persistent update did not finish")
	}
	select {
	case <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("MarkResult did not finish")
	}

	current, ok := manager.GetByID(original.ID)
	if !ok || current.Metadata["token"] != "new" {
		t.Fatalf("persistent update was lost: %#v", current)
	}
	state := current.ModelStates["plugin-model"]
	if state == nil || !state.Quota.Exceeded || state.Status != StatusError {
		t.Fatalf("result state was overwritten: %#v", state)
	}
}

func TestMergePersistentUpdateDoesNotOverwriteConcurrentCredentialRefresh(t *testing.T) {
	store := &blockingAuthStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	original := &Auth{
		ID:         "plugin-auth",
		Provider:   "plugin-provider",
		Status:     StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"token": "old"},
	}
	registered, errRegister := manager.Register(context.Background(), original)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	store.mu.Lock()
	store.block = true
	store.mu.Unlock()
	refreshed := registered.Clone()
	refreshed.Metadata["token"] = "refreshed"
	updateDone := make(chan struct{})
	go func() {
		_, _ = manager.Update(context.Background(), refreshed)
		close(updateDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("credential refresh did not enter persistence")
	}

	pluginUpdate := registered.Clone()
	pluginUpdate.Metadata["token"] = "plugin"
	mergeDone := make(chan *Auth, 1)
	go func() {
		merged, _ := manager.MergePersistentUpdate(context.Background(), registered, pluginUpdate)
		mergeDone <- merged
	}()
	select {
	case <-mergeDone:
		t.Fatal("plugin update bypassed the in-flight credential refresh")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	select {
	case <-updateDone:
	case <-time.After(time.Second):
		t.Fatal("credential refresh did not finish")
	}
	select {
	case merged := <-mergeDone:
		if merged != nil {
			t.Fatalf("MergePersistentUpdate() = %#v, want stale update rejection", merged)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin update did not finish")
	}

	current, ok := manager.GetByID(original.ID)
	if !ok || current.Metadata["token"] != "refreshed" {
		t.Fatalf("current token = %v, want refreshed", current.Metadata["token"])
	}
	store.mu.Lock()
	persisted := store.last.Clone()
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["token"] != "refreshed" {
		t.Fatalf("persisted token = %v, want refreshed", persisted.Metadata["token"])
	}
}
