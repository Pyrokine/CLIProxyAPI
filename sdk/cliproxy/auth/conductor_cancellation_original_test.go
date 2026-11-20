package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

type cancellationStore struct {
	saves   atomic.Int32
	lists   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *cancellationStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saves.Add(1)
	if auth.Label == "blocked" {
		close(s.entered)
		<-s.release
	}
	return "", nil
}
func (s *cancellationStore) List(context.Context) ([]*Auth, error) {
	s.lists.Add(1)
	return nil, nil
}
func (*cancellationStore) Delete(context.Context, string) error { return nil }

type cancellationHook struct{ calls atomic.Int32 }

func (h *cancellationHook) OnAuthRegistered(context.Context, *Auth) { h.calls.Add(1) }
func (h *cancellationHook) OnAuthUpdated(context.Context, *Auth)    { h.calls.Add(1) }
func (*cancellationHook) OnResult(context.Context, Result)          {}

func assertCanceledMutation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context cancellation, got %v", err)
		}
	default:
		t.Error("canceled operation still waiting for lock")
	}
}

func TestOriginalManagerCanceledLoadUnblocksUnrelatedMutation(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			store := &cancellationStore{entered: make(chan struct{}), release: make(chan struct{})}
			manager := NewManager(store, nil, nil)
			saveDone := make(chan error, 1)
			go func() {
				_, err := manager.Register(
					context.Background(),
					&Auth{ID: "A", Provider: "audit", Label: "blocked", Metadata: map[string]any{"type": "audit"}},
				)
				saveDone <- err
			}()
			<-store.entered
			ctx, cancel := context.WithCancel(context.Background())
			loadDone := make(chan error, 1)
			go func() { loadDone <- manager.Load(ctx) }()
			synctest.Wait()
			bDone := make(chan error, 1)
			go func() {
				_, err := manager.Register(context.Background(), &Auth{ID: "B", Provider: "audit"})
				bDone <- err
			}()
			synctest.Wait()
			select {
			case <-bDone:
				t.Error("B overtook queued reload")
			default:
			}
			cancel()
			synctest.Wait()
			assertCanceledMutation(t, loadDone)
			select {
			case err := <-bDone:
				if err != nil {
					t.Error(err)
				}
			default:
				t.Error("canceled reload still blocks unrelated B")
			}
			if store.lists.Load() != 0 {
				t.Error("canceled reload called List")
			}
			close(store.release)
			synctest.Wait()
			if err := <-saveDone; err != nil {
				t.Error(err)
			}
			if !manager.authLoadGate.TryAcquire(authLoadGateCapacity) {
				t.Error("reload barrier permit leaked")
			} else {
				manager.authLoadGate.Release(authLoadGateCapacity)
			}
		},
	)
}

func TestOriginalManagerCanceledMutationWait(t *testing.T) {
	for _, operation := range []string{"Register", "Update", "Prepare", "Refresh"} {
		for _, waitOn := range []string{"barrier", "gate"} {
			t.Run(
				operation+"/"+waitOn, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							store := &cancellationStore{}
							hook := &cancellationHook{}
							manager := NewManager(store, nil, hook)
							base, err := manager.Register(
								WithSkipPersist(context.Background()),
								&Auth{ID: "A", Provider: "audit", Metadata: map[string]any{"type": "audit"}},
							)
							if err != nil {
								t.Fatal(err)
							}
							executor := &originalLifecycleExecutor{
								id: "audit",
								mutate: func(_ context.Context, auth *Auth) (*Auth, error) {
									updated := auth.Clone()
									updated.Metadata["access_token"] = authAccessToken(auth) + "-minted"
									return updated, nil
								},
							}
							manager.RegisterExecutor(executor)
							hook.calls.Store(0)
							var release func()
							if waitOn == "barrier" {
								if errAcquire := manager.authLoadGate.Acquire(
									context.Background(), authLoadGateCapacity,
								); errAcquire != nil {
									t.Fatal(errAcquire)
								}
								release = func() { manager.authLoadGate.Release(authLoadGateCapacity) }
							} else {
								var errLock error
								release, errLock = manager.lockAuthMutation(context.Background(), base.ID)
								if errLock != nil {
									t.Fatal(errLock)
								}
							}
							ctx, cancel := context.WithCancel(context.Background())
							done := make(chan error, 1)
							go func() {
								var errMutation error
								switch operation {
								case "Register":
									_, errMutation = manager.Register(ctx, base.Clone())
								case "Update":
									_, errMutation = manager.Update(ctx, base.Clone())
								case "Prepare":
									_, errMutation = manager.PrepareRequestAuth(ctx, executor, base)
								case "Refresh":
									_, errMutation = manager.ForceRefreshAuth(ctx, base.ID)
								}
								done <- errMutation
							}()
							synctest.Wait()
							cancel()
							synctest.Wait()
							assertCanceledMutation(t, done)
							release()
							synctest.Wait()
							if store.saves.Load() != 0 || hook.calls.Load() != 0 {
								t.Error("canceled mutation invoked Save or hook")
							}
							if got, _ := manager.GetByID(base.ID); got.Generation != base.Generation ||
								got.registrationGeneration != base.registrationGeneration {
								t.Error("canceled mutation published auth")
							}
							if !manager.authLoadGate.TryAcquire(authLoadGateCapacity) {
								t.Error("mutation leaked reload barrier permit")
							} else {
								manager.authLoadGate.Release(authLoadGateCapacity)
							}
							manager.authMutationLocksMu.Lock()
							remaining := len(manager.authMutationLocks)
							manager.authMutationLocksMu.Unlock()
							if remaining != 0 {
								t.Error("mutation leaked credential gate")
							}
							manager.requestPrepareLocksMu.Lock()
							preparations := len(manager.requestPrepareLocks)
							manager.requestPrepareLocksMu.Unlock()
							manager.refreshLocksMu.Lock()
							refreshes := len(manager.refreshLocks)
							manager.refreshLocksMu.Unlock()
							if preparations != 0 || refreshes != 0 {
								t.Errorf(
									"canceled operation retained locks: prepare=%d refresh=%d", preparations, refreshes,
								)
							}
						},
					)
				},
			)
		}
	}
}

func TestOriginalManagerMutationNilContext(t *testing.T) {
	manager := NewManager(&cancellationStore{}, nil, nil)
	//lint:ignore SA1012 验证已发布 SDK 的 nil context 注册合同
	auth, err := manager.Register(nil, &Auth{ID: "A", Provider: "audit", Metadata: map[string]any{"type": "audit"}})
	if err != nil {
		t.Fatal(err)
	}
	//lint:ignore SA1012 验证已发布 SDK 的 nil context 更新合同
	if _, errUpdate := manager.Update(nil, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	executor := &originalLifecycleExecutor{
		id: "audit",
		mutate: func(_ context.Context, auth *Auth) (*Auth, error) {
			updated := auth.Clone()
			updated.Metadata["access_token"] = authAccessToken(auth) + "-minted"
			return updated, nil
		},
	}
	manager.RegisterExecutor(executor)
	//lint:ignore SA1012 验证已发布 SDK 的 nil context 准备合同
	if _, errPrepare := manager.PrepareRequestAuth(nil, executor, auth); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	//lint:ignore SA1012 验证已发布 SDK 的 nil context 刷新合同
	if _, errRefresh := manager.ForceRefreshAuth(nil, auth.ID); errRefresh != nil {
		t.Fatal(errRefresh)
	}
	//lint:ignore SA1012 验证已发布 SDK 的 nil context 加载合同
	if errLoad := manager.Load(nil); errLoad != nil {
		t.Fatal(errLoad)
	}
}
