package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type requestPreparationGateExecutor struct {
	schedulerTestExecutor
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (e *requestPreparationGateExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth != nil && auth.Metadata["prepared"] != true
}

func (e *requestPreparationGateExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.calls.Add(1) == 1 {
		close(e.entered)
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return auth, ctx.Err()
	}
	updated := auth.Clone()
	updated.Metadata["prepared"] = true
	return updated, nil
}

type requestPreparationWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *requestPreparationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestPrepareRequestAuthCanceledWait(t *testing.T) {
	for _, mode := range []string{"already-canceled", "waiting-canceled", "waiting-deadline"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth, err := manager.Register(t.Context(), &Auth{
				ID: "prepare-wait", Provider: "test", Metadata: map[string]any{"type": "test"},
			})
			if err != nil {
				t.Fatal(err)
			}
			executor := &requestPreparationGateExecutor{entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(executor.release) }) }
			var workers sync.WaitGroup
			t.Cleanup(func() {
				release()
				workers.Wait()
			})
			firstDone := make(chan error, 1)
			workers.Go(func() {
				_, err := manager.PrepareRequestAuth(context.Background(), executor, auth)
				firstDone <- err
			})
			select {
			case <-executor.entered:
			case <-time.After(time.Second):
				t.Fatal("first preparation did not acquire its gate")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expectedErr := error(context.Canceled)
			if mode == "waiting-deadline" {
				var cancelDeadline context.CancelFunc
				ctx, cancelDeadline = context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancelDeadline()
				expectedErr = context.DeadlineExceeded
			}
			if mode == "already-canceled" {
				cancel()
			}
			waitContext := &requestPreparationWaitContext{Context: ctx, waiting: make(chan struct{})}
			secondDone := make(chan error, 1)
			workers.Go(func() {
				_, err := manager.PrepareRequestAuth(waitContext, executor, auth)
				secondDone <- err
			})
			if mode != "already-canceled" {
				select {
				case <-waitContext.waiting:
				case <-time.After(time.Second):
					t.Fatal("second preparation did not wait on its context")
				}
				manager.requestPrepareLocksMu.Lock()
				refs := manager.requestPrepareLocks[auth.ID].refs
				manager.requestPrepareLocksMu.Unlock()
				if refs != 2 {
					t.Fatalf("preparation references = %d, want 2", refs)
				}
				if mode == "waiting-canceled" {
					cancel()
				}
			}
			select {
			case err := <-secondDone:
				if !errors.Is(err, expectedErr) {
					t.Fatalf("canceled preparation returned %v, want %v", err, expectedErr)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("canceled preparation remains blocked behind another request's preparation")
			}
			if calls := executor.calls.Load(); calls != 1 {
				t.Fatalf("preparation calls = %d, want 1", calls)
			}
			manager.requestPrepareLocksMu.Lock()
			refs := manager.requestPrepareLocks[auth.ID].refs
			manager.requestPrepareLocksMu.Unlock()
			if refs != 1 {
				t.Fatalf("preparation references after cancellation = %d, want 1", refs)
			}
			release()
			select {
			case err := <-firstDone:
				if err != nil {
					t.Fatalf("first preparation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("first preparation did not finish after release")
			}
			manager.requestPrepareLocksMu.Lock()
			remaining := len(manager.requestPrepareLocks)
			manager.requestPrepareLocksMu.Unlock()
			if remaining != 0 {
				t.Fatalf("preparation locks after completion = %d, want 0", remaining)
			}
			prepared, err := manager.PrepareRequestAuth(t.Context(), executor, auth)
			if err != nil || prepared == nil || prepared.Metadata["prepared"] != true || executor.calls.Load() != 1 {
				t.Fatalf("subsequent preparation = %#v, %v; calls=%d", prepared, err, executor.calls.Load())
			}
		})
	}
}
