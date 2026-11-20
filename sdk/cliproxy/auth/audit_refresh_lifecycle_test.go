package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type auditUncancelableRefreshExecutor struct {
	schedulerTestExecutor
	entered    chan struct{}
	release    chan struct{}
	healthy    chan struct{}
	stuckCalls atomic.Int32
}

func (*auditUncancelableRefreshExecutor) Identifier() string { return "audit-refresh" }
func (e *auditUncancelableRefreshExecutor) Refresh(_ context.Context, credential *Auth) (*Auth, error) {
	if credential.ID == "stuck" {
		if e.stuckCalls.Add(1) == 1 {
			close(e.entered)
		}
		<-e.release
	} else {
		close(e.healthy)
	}
	updated := credential.Clone()
	updated.Metadata["access_token"] = "refreshed-" + credential.ID
	return updated, nil
}

func TestAuditAutoRefreshRestartIsolatesUncancelableProvider(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&config.Config{AuthAutoRefreshWorkers: 1})
			executor := &auditUncancelableRefreshExecutor{
				entered: make(chan struct{}), release: make(chan struct{}), healthy: make(chan struct{}),
			}
			release := sync.OnceFunc(func() { close(executor.release) })
			defer release()
			manager.RegisterExecutor(executor)
			parent, errParent := manager.Register(
				t.Context(), &Auth{
					ID: "parent", Provider: "unregistered-owner",
					Metadata: map[string]any{"access_token": "old-parent"},
				},
			)
			if errParent != nil {
				t.Fatal(errParent)
			}
			original, err := manager.Register(
				t.Context(), &Auth{
					ID: "stuck", Provider: executor.Identifier(),
					Attributes: map[string]string{"gemini_virtual_parent": "parent"},
					Metadata:   map[string]any{"access_token": "old-stuck", "refresh_interval_seconds": 3600},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			manager.StartAutoRefresh(t.Context(), time.Second)
			<-executor.entered
			stopStarted := time.Now()
			//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
			if err := manager.StopAutoRefresh(nil); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(
				err, errAuthDurableStateUncertain,
			) {
				t.Fatalf("uncancelable provider stop error=%v", err)
			}
			if elapsed := time.Since(stopStarted); elapsed != 5*time.Second {
				t.Fatalf("uncancelable provider stop took %s, want 5s", elapsed)
			}
			replacement := original.Clone()
			replacement.Metadata["access_token"] = "replacement-stuck"
			if _, err := manager.Update(t.Context(), replacement); err != nil {
				t.Fatal(err)
			}
			newParent := parent.Clone()
			newParent.Metadata["access_token"] = "replacement-parent"
			if _, err := manager.Update(t.Context(), newParent); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Register(
				t.Context(), &Auth{
					ID: "healthy", Provider: executor.Identifier(),
					Metadata: map[string]any{"access_token": "old-healthy", "refresh_interval_seconds": 3600},
				},
			); err != nil {
				t.Fatal(err)
			}
			restartStarted := time.Now()
			manager.StartAutoRefresh(t.Context(), time.Second)
			if elapsed := time.Since(restartStarted); elapsed != 5*time.Second {
				t.Fatalf("uncancelable provider restart took %s, want 5s", elapsed)
			}
			<-executor.healthy
			synctest.Wait()
			if executor.stuckCalls.Load() != 1 {
				t.Fatal("canceled provider was invoked again")
			}
			current, _ := manager.GetByID("healthy")
			if current.Metadata["access_token"] != "refreshed-healthy" {
				t.Fatalf("healthy refresh was blocked: %+v", current)
			}
			if err := manager.StopAutoRefresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			release()
			synctest.Wait()
			current, _ = manager.GetByID("stuck")
			if current.Metadata["access_token"] != "replacement-stuck" || !current.LastRefreshedAt.IsZero() {
				t.Fatalf("old refresh changed replacement: %+v", current)
			}
			manager.refreshLocksMu.Lock()
			retained := len(manager.refreshLocks)
			manager.refreshLocksMu.Unlock()
			if retained != 0 {
				t.Fatalf("completed provider retained %d refresh locks", retained)
			}
			manager.StartAutoRefresh(t.Context(), time.Second)
			synctest.Wait()
			current, _ = manager.GetByID("stuck")
			if executor.stuckCalls.Load() != 2 || current.Metadata["access_token"] != "refreshed-stuck" {
				t.Fatalf(
					"current registration was not refreshed after old provider completion: calls=%d auth=%+v",
					executor.stuckCalls.Load(), current,
				)
			}
			if err := manager.StopAutoRefresh(t.Context()); err != nil {
				t.Fatal(err)
			}
		},
	)
}
