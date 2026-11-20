package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestQuotaRestrictedAutomaticTokenRefresh(t *testing.T) {
	for _, test := range []struct {
		name     string
		restrict func(*Auth)
	}{
		{"disabled", func(a *Auth) { a.Disabled = true }},
		{"disabled status", func(a *Auth) { a.Status = StatusDisabled }},
		{"exhausted", func(a *Auth) { a.Quota.Exceeded = true }},
		{"legacy 429", func(a *Auth) { a.LastError = &Error{HTTPStatus: http.StatusTooManyRequests} }},
		{"legacy forbidden", func(a *Auth) { a.LastError = &Error{HTTPStatus: http.StatusForbidden} }},
		{"legacy payment required", func(a *Auth) { a.LastError = &Error{HTTPStatus: http.StatusPaymentRequired} }},
		{
			"credential forbidden", func(a *Auth) {
				a.LastError = &Error{HTTPStatus: http.StatusForbidden}
				a.StateEffectScope = cliproxyexecutor.StateEffectScopeCredential
			},
		},
		{
			"credential payment required", func(a *Auth) {
				a.LastError = &Error{HTTPStatus: http.StatusPaymentRequired}
				a.StateEffectScope = cliproxyexecutor.StateEffectScopeCredential
			},
		},
		{
			"unauthorized", func(a *Auth) {
				a.Status = StatusError
				a.Unavailable = true
				a.LastError = &Error{HTTPStatus: http.StatusUnauthorized}
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ctx := context.Background()
				manager := NewManager(nil, nil, nil)
				exec := &refreshTimingExecutor{}
				manager.RegisterExecutor(exec)
				auth := &Auth{
					ID: "restricted-token-refresh", Provider: exec.Identifier(), Status: StatusActive,
					Metadata: map[string]any{"access_token": "synthetic", "refresh_interval_seconds": 1},
				}
				test.restrict(auth)
				registered, err := manager.Register(ctx, auth)
				if err != nil {
					t.Fatal(err)
				}
				if manager.shouldRefresh(registered, time.Now()) {
					t.Error("restricted credential passed automatic refresh eligibility")
				}
				if job := manager.markRefreshPending(registered.ID, registered.Generation, time.Now()); job != nil {
					manager.finishAutoRefreshJob(job)
					t.Error("restricted credential was queued")
				}
				manager.checkRefreshes(ctx)
				manager.refreshWorkers.Wait()
				if calls := exec.calls.Load(); calls != 0 {
					t.Fatalf("restricted credential made %d token refresh calls", calls)
				}
			},
		)
	}
}

func TestPausedCredentialManualRefreshAndExplicitResume(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &refreshTimingExecutor{}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(
		t.Context(), &Auth{
			ID: "paused-refresh", Provider: executor.Identifier(), Disabled: true, Status: StatusDisabled,
			Runtime: alwaysDueRefresh{}, Metadata: map[string]any{"access_token": "old", "refresh_token": "refresh"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.checkRefreshes(t.Context())
	manager.refreshWorkers.Wait()
	if executor.calls.Load() != 0 {
		t.Fatal("paused credential triggered background token refresh")
	}
	if _, err := manager.ForceRefreshAuth(t.Context(), registered.ID); err != nil {
		t.Fatalf("explicit manual refresh failed: %v", err)
	}
	current, _ := manager.GetByID(registered.ID)
	if executor.calls.Load() != 1 || !current.Disabled || current.Status != StatusDisabled ||
		current.Metadata["access_token"] != "refreshed" {
		t.Fatalf("manual refresh changed pause state or lost token: calls=%d auth=%+v", executor.calls.Load(), current)
	}
	manager.checkRefreshes(t.Context())
	manager.refreshWorkers.Wait()
	if executor.calls.Load() != 1 {
		t.Fatal("manual refresh resumed background work without an explicit enable")
	}
	current.Disabled = false
	current.Status = StatusActive
	current.Metadata["disabled"] = false
	if _, err := manager.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	manager.checkRefreshes(t.Context())
	manager.refreshWorkers.Wait()
	if executor.calls.Load() != 2 {
		t.Fatalf("explicit enable did not resume automatic refresh: calls=%d", executor.calls.Load())
	}
}

func TestQuotaQueuedTokenRefreshRechecksCredentialRejection(t *testing.T) {
	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden} {
		for _, scope := range []cliproxyexecutor.StateEffectScope{
			cliproxyexecutor.StateEffectScopeCredential, cliproxyexecutor.StateEffectScopeExactModel,
		} {
			t.Run(
				http.StatusText(status)+"/"+string(scope), func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
					exec := &refreshTimingExecutor{}
					manager.RegisterExecutor(exec)
					registered, err := manager.Register(
						t.Context(), &Auth{
							ID: "queued-rejected-refresh", Provider: exec.Identifier(), Status: StatusActive,
							Metadata: map[string]any{"access_token": "synthetic", "refresh_interval_seconds": 1},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					semaphore := manager.refreshSemaphore
					semaphore <- struct{}{}
					manager.checkRefreshes(t.Context())
					manager.MarkResult(
						t.Context(), Result{
							AuthID: registered.ID, AuthGeneration: registered.Generation, Model: "rejected-model",
							Error: &Error{HTTPStatus: status, Message: "synthetic credential rejection"},
							Disposition: cliproxyexecutor.ExecutionDisposition{
								DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
								Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
								RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
								EffectScope:    scope,
							},
						},
					)
					<-semaphore
					manager.refreshWorkers.Wait()
					wantCalls := int64(0)
					if scope == cliproxyexecutor.StateEffectScopeExactModel {
						wantCalls = 1
					}
					if calls := exec.calls.Load(); calls != wantCalls {
						t.Fatalf("automatic refresh calls=%d, want %d for scope %s", calls, wantCalls, scope)
					}
					current, _ := manager.GetByID(registered.ID)
					if !current.NextRefreshAfter.IsZero() {
						t.Fatalf("finished worker retained its pending slot: %+v", current)
					}
				},
			)
		}
	}
}

func TestQuotaQueuedTokenRefreshRechecksNew429AndRecovers(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
	exec := &refreshTimingExecutor{}
	manager.RegisterExecutor(exec)
	registered, err := manager.Register(
		ctx, &Auth{
			ID: "queued-quota-token-refresh", Provider: exec.Identifier(), Status: StatusActive,
			Metadata: map[string]any{"access_token": "synthetic", "refresh_interval_seconds": 1},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	semaphore := manager.refreshSemaphore
	semaphore <- struct{}{}
	manager.checkRefreshes(ctx)
	manager.mu.RLock()
	queued := manager.autoRefreshJobs[registered.ID]
	manager.mu.RUnlock()
	if queued == nil {
		<-semaphore
		t.Fatal("healthy credential was not queued")
	}
	manager.MarkResult(
		ctx, Result{
			AuthID: registered.ID, AuthGeneration: registered.Generation,
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "new credential quota"},
			Disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
				Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
				EffectScope:    cliproxyexecutor.StateEffectScopeCredential, EffectKey: registered.ID,
			},
		},
	)
	<-semaphore
	manager.refreshWorkers.Wait()
	current, _ := manager.GetByID(registered.ID)
	if exec.calls.Load() != 0 || !current.Quota.Exceeded || !current.NextRefreshAfter.IsZero() {
		t.Fatalf("queued refresh ignored new 429 or retained its slot: calls=%d auth=%+v", exec.calls.Load(), current)
	}
	if _, err := manager.RecoverQuotaAfterRefresh(ctx, registered.ID, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	manager.checkRefreshes(ctx)
	manager.refreshWorkers.Wait()
	current, _ = manager.GetByID(registered.ID)
	if exec.calls.Load() != 1 || current.Metadata["access_token"] != "refreshed" || current.Quota.Exceeded {
		t.Fatalf("authoritative recovery did not resume token refresh: calls=%d auth=%+v", exec.calls.Load(), current)
	}
}
