package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type refreshTimingObserver struct{ checks atomic.Int64 }

func (o *refreshTimingObserver) ShouldRefresh(time.Time, *Auth) bool {
	o.checks.Add(1)
	return false
}

func TestAutoRefreshBoundsPollingAndStopsWithParent(t *testing.T) {
	for _, test := range []struct {
		name               string
		interval, expected time.Duration
	}{
		{"default", 0, 5 * time.Second},
		{"short", 2 * time.Second, 2 * time.Second},
		{"limit", 30 * time.Second, 30 * time.Second},
		{"service legacy interval", 15 * time.Minute, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				observer := &refreshTimingObserver{}
				_, err := manager.Register(context.Background(), &Auth{
					ID: "synthetic-refresh-timing", Provider: "synthetic-refresh-timing", Runtime: observer,
					Metadata: map[string]any{"access_token": "synthetic-refresh-token"},
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				defer func() {
					if err := manager.StopAutoRefresh(context.Background()); err != nil {
						t.Error(err)
					}
				}()
				manager.StartAutoRefresh(ctx, test.interval)
				synctest.Wait()
				if got := observer.checks.Load(); got != 1 {
					t.Fatalf("initial checks=%d", got)
				}
				time.Sleep(test.expected - time.Nanosecond)
				synctest.Wait()
				if got := observer.checks.Load(); got != 1 {
					t.Fatalf("early checks=%d", got)
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if got := observer.checks.Load(); got != 2 {
					t.Fatalf("checks at %s=%d, want 2", test.expected, got)
				}
				cancel()
				synctest.Wait()
				time.Sleep(time.Minute)
				synctest.Wait()
				if got := observer.checks.Load(); got != 2 {
					t.Fatalf("checks after parent cancellation=%d", got)
				}
			})
		})
	}
}

func TestAutoRefreshDoesNotDuplicatePendingCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "pending-refresh", Provider: "pending-refresh",
		Metadata: map[string]any{"access_token": "old", "refresh_token": "refresh"},
	})
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	now := time.Now()
	job := manager.markRefreshPending(registered.ID, registered.Generation, now)
	if job == nil {
		t.Fatal("initial refresh was not scheduled")
	}
	if manager.markRefreshPending(registered.ID, registered.Generation, now.Add(2*refreshPendingBackoff)) != nil {
		t.Fatal("running refresh was scheduled again after pending window")
	}
	manager.finishAutoRefreshJob(job)
	current, _ := manager.GetByID(registered.ID)
	if !current.NextRefreshAfter.IsZero() {
		t.Fatalf("completed job left pending window: %v", current.NextRefreshAfter)
	}
}

type refreshTimingExecutor struct {
	schedulerTestExecutor
	calls atomic.Int64
}

func (*refreshTimingExecutor) Identifier() string { return "synthetic-auto-refresh-job" }

func (e *refreshTimingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.calls.Add(1)
	updated := auth.Clone()
	updated.Metadata["access_token"] = "refreshed"
	return updated, nil
}

func TestAutoRefreshLimitsPendingJobs(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
	first, err := manager.Register(context.Background(), &Auth{ID: "first", Provider: "synthetic-auto-refresh-job"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Register(context.Background(), &Auth{ID: "second", Provider: "synthetic-auto-refresh-job"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	job := manager.markRefreshPending(first.ID, first.Generation, now)
	if job == nil {
		t.Fatal("first job was not scheduled")
	}
	if manager.markRefreshPending(second.ID, second.Generation, now) != nil {
		t.Fatal("scheduler exceeded configured capacity")
	}
	manager.finishAutoRefreshJob(job)
	if manager.markRefreshPending(second.ID, second.Generation, now) == nil {
		t.Fatal("capacity was not released when job finished")
	}
}

func TestAutoRefreshUpdatedCredentialDoesNotInheritStalePendingWindow(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "updated-refresh", Provider: "synthetic-auto-refresh-job",
		Metadata: map[string]any{"access_token": "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := manager.markRefreshPending(registered.ID, registered.Generation, time.Now())
	if job == nil {
		t.Fatal("initial job was not scheduled")
	}
	updated := registered.Clone()
	updated.Metadata["access_token"] = "replacement"
	replacement, err := manager.Update(context.Background(), updated)
	if err != nil || replacement == nil {
		t.Fatalf("Update() = (%#v, %v)", replacement, err)
	}
	if !replacement.NextRefreshAfter.IsZero() {
		t.Fatalf("replacement retained stale pending marker: %v", replacement.NextRefreshAfter)
	}
	manager.finishAutoRefreshJob(job)
	if manager.markRefreshPending(replacement.ID, replacement.Generation, time.Now()) == nil {
		t.Fatal("replacement was not eligible after old job finished")
	}
}

func TestAutoRefreshQueuedJobSurvivesRequestResult(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
	executor := &refreshTimingExecutor{}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID: "result-during-refresh-queue", Provider: executor.Identifier(),
		Metadata: map[string]any{"access_token": "old", "refresh_interval_seconds": 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	semaphore := manager.refreshSemaphore
	manager.mu.RUnlock()
	semaphore <- struct{}{}
	released := false
	defer func() {
		if !released {
			<-semaphore
			manager.refreshWorkers.Wait()
		}
	}()
	ctx := context.Background()
	manager.checkRefreshes(ctx)
	manager.mu.RLock()
	queued := manager.autoRefreshJobs[registered.ID]
	manager.mu.RUnlock()
	if queued == nil {
		t.Fatal("credential refresh was not queued")
	}
	manager.MarkResult(ctx, Result{AuthID: registered.ID, AuthGeneration: registered.Generation, Success: true})
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Generation != registered.Generation || current.Success != 1 {
		t.Fatalf("request result changed credential generation: %#v", current)
	}
	<-semaphore
	released = true
	manager.refreshWorkers.Wait()
	current, _ = manager.GetByID(registered.ID)
	if executor.calls.Load() != 1 || current.Metadata["access_token"] != "refreshed" {
		t.Fatalf("queued refresh after request result: calls=%d auth=%#v", executor.calls.Load(), current)
	}
}

func TestAutoRefreshQueuedJobDoesNotRefreshRecreatedCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
	executor := &refreshTimingExecutor{}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "recreated-refresh", Provider: executor.Identifier(),
		Metadata: map[string]any{"email": "test@example.invalid", "access_token": "old", "refresh_interval_seconds": 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	semaphore := manager.refreshSemaphore
	manager.mu.RUnlock()
	semaphore <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.checkRefreshes(ctx)
	manager.mu.RLock()
	queued := manager.autoRefreshJobs[registered.ID]
	manager.mu.RUnlock()
	if queued == nil {
		t.Fatal("old credential was not queued")
	}
	if err := manager.Remove(WithRuntimeOnlyRemove(WithSkipPersist(context.Background())), registered.ID); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	replacement, err := manager.Register(WithAllowAuthRecreate(context.Background()), &Auth{
		ID: registered.ID, Provider: executor.Identifier(),
		Metadata: map[string]any{"email": "test@example.invalid", "access_token": "replacement", "refresh_interval_seconds": 3600},
	})
	if err != nil || replacement == nil {
		t.Fatalf("Register(replacement) = (%#v, %v)", replacement, err)
	}
	<-semaphore
	manager.refreshWorkers.Wait()
	current, _ := manager.GetByID(registered.ID)
	if executor.calls.Load() != 0 || current.Metadata["access_token"] != "replacement" {
		t.Fatalf("stale job refreshed replacement: calls=%d token=%v", executor.calls.Load(), current.Metadata["access_token"])
	}
	manager.mu.RLock()
	remaining := len(manager.autoRefreshJobs)
	manager.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("completed job retained %d pending entries", remaining)
	}
	manager.checkRefreshes(ctx)
	manager.refreshWorkers.Wait()
	if executor.calls.Load() != 1 {
		t.Fatalf("replacement was not refreshed on next check: calls=%d", executor.calls.Load())
	}
}

func TestAutoRefreshCanceledJobReleasesPendingWindow(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
	executor := &refreshTimingExecutor{}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "cancelled-refresh", Provider: executor.Identifier(),
		Metadata: map[string]any{"email": "test@example.invalid", "refresh_interval_seconds": 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	semaphore := manager.refreshSemaphore
	manager.mu.RUnlock()
	semaphore <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	manager.checkRefreshes(ctx)
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := manager.stopAutoRefreshWorkers(stopCtx); err != nil {
		t.Fatal(err)
	}
	<-semaphore
	manager.mu.RLock()
	remaining := len(manager.autoRefreshJobs)
	manager.mu.RUnlock()
	current, _ := manager.GetByID(registered.ID)
	if remaining != 0 || !current.NextRefreshAfter.IsZero() || executor.calls.Load() != 0 {
		t.Fatalf("canceled job left pending state: jobs=%d next=%v calls=%d", remaining, current.NextRefreshAfter, executor.calls.Load())
	}
}

func TestShouldRefreshUsesEvaluationTime(t *testing.T) {
	now := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	manager := NewManager(nil, nil, nil)
	const provider = "synthetic-evaluation-clock"
	RegisterRefreshLeadProvider(provider, func() *time.Duration { return new(50 * time.Minute) })
	t.Cleanup(func() {
		refreshLeadMu.Lock()
		delete(refreshLeadFactories, provider)
		refreshLeadMu.Unlock()
	})
	for _, test := range []struct {
		name      string
		remaining time.Duration
		refresh   bool
	}{
		{"outside lead", 51 * time.Minute, false},
		{"at lead", 50 * time.Minute, true},
		{"inside lead", 49 * time.Minute, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := &Auth{Provider: provider, Metadata: map[string]any{
				"expired": now.Add(test.remaining).Format(time.RFC3339),
			}}
			if got := manager.shouldRefresh(credential, now); got != test.refresh {
				t.Fatalf("shouldRefresh=%t, want %t", got, test.refresh)
			}
		})
	}
}
