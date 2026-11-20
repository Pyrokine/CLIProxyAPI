package auth

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAutoRefreshOriginalEligibilityBoundaries(t *testing.T) {
	for provider, lead := range map[string]time.Duration{
		"disabled-schedule": 10 * time.Minute, "provider-lead-expiry": 10 * time.Minute,
		"relative-expiry": 30 * time.Minute,
	} {
		refreshLeadMu.Lock()
		previous, found := refreshLeadFactories[provider]
		refreshLeadMu.Unlock()
		RegisterRefreshLeadProvider(provider, func() *time.Duration { return &lead })
		t.Cleanup(
			func() {
				refreshLeadMu.Lock()
				defer refreshLeadMu.Unlock()
				if found {
					refreshLeadFactories[provider] = previous
				} else {
					delete(refreshLeadFactories, provider)
				}
			},
		)
	}
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	for _, sample := range []struct {
		name      string
		auth      *Auth
		start     time.Time
		boundary  time.Time
		wantAfter bool
	}{
		{
			name:  "TestNextRefreshCheckAt_DisabledUnschedule",
			auth:  &Auth{ID: "a1", Provider: "test", Disabled: true},
			start: now, boundary: now.Add(15 * time.Minute),
		},
		{
			name: "TestNextRefreshCheckAt_DisabledWithInvalidGrantUnschedule/normal-disabled",
			auth: &Auth{
				ID: "normal-disabled", Provider: "disabled-schedule", Disabled: true, Status: StatusDisabled,
				Metadata: map[string]any{"email": "x@example.com", "expires_at": expiry.Format(time.RFC3339)},
			},
			start: now, boundary: expiry.Add(-10 * time.Minute),
		},
		{
			name: "TestNextRefreshCheckAt_DisabledWithInvalidGrantUnschedule/invalid-grant-disabled",
			auth: &Auth{
				ID: "invalid-grant-disabled", Provider: "disabled-schedule", Disabled: true, Status: StatusDisabled,
				LastError: &Error{
					HTTPStatus: 400, Message: `{"error": "invalid_grant", "error_description": "Bad Request"}`,
				},
				Metadata: map[string]any{"email": "x@example.com", "expires_at": expiry.Format(time.RFC3339)},
			},
			start: now, boundary: expiry.Add(-10 * time.Minute),
		},
		{
			name: "TestNextRefreshCheckAt_NextRefreshAfterGate",
			auth: &Auth{
				ID: "a1", Provider: "test", NextRefreshAfter: now.Add(30 * time.Minute),
				Metadata: map[string]any{"email": "x@example.com"}, Runtime: alwaysDueRefresh{},
			},
			start: now, boundary: now.Add(30 * time.Minute), wantAfter: true,
		},
		{
			name: "TestNextRefreshCheckAt_PreferredInterval_PicksEarliestCandidate",
			auth: &Auth{
				ID: "a1", Provider: "test", LastRefreshedAt: now,
				Metadata: map[string]any{
					"email": "x@example.com", "expires_at": now.Add(20 * time.Minute).Format(time.RFC3339),
					"refresh_interval_seconds": 900,
				},
			},
			start: now, boundary: now.Add(5 * time.Minute), wantAfter: true,
		},
		{
			name: "TestNextRefreshCheckAt_ProviderLead_Expiry",
			auth: &Auth{
				ID: "a1", Provider: "provider-lead-expiry",
				Metadata: map[string]any{"email": "x@example.com", "expires_at": expiry.Format(time.RFC3339)},
			},
			start: now, boundary: expiry.Add(-10 * time.Minute), wantAfter: true,
		},
		{
			name: "TestNextRefreshCheckAt_RelativeExpiry",
			auth: &Auth{
				ID: "relative-expiry-auth", Provider: "relative-expiry",
				Metadata: map[string]any{
					"access_token": "test-access", "expires_in": 3600,
					"timestamp": int(now.Add(-15 * time.Minute).UnixMilli()),
				},
			},
			start: now, boundary: now.Add(15 * time.Minute), wantAfter: true,
		},
		{
			name: "TestNextRefreshCheckAt_RefreshEvaluatorFallback",
			auth: &Auth{
				ID: "a1", Provider: "test", Metadata: map[string]any{"email": "x@example.com"},
				Runtime: &refreshTimingObserver{},
			},
			start: now, boundary: now.Add(15 * time.Minute),
		},
		{
			name: "TestAuthAutoRefreshLoop_PopDue_AfterSystemSuspendResume",
			auth: &Auth{
				ID: "gemini-oauth", Provider: "provider-lead-expiry",
				Metadata: map[string]any{
					"email": "x@example.com", "expires_at": now.Add(11 * time.Hour).Format(time.RFC3339),
				},
			},
			start: now.Add(10 * time.Hour), boundary: now.Add(12 * time.Hour), wantAfter: true,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				if manager.shouldRefresh(sample.auth, sample.start) {
					t.Fatal("credential refreshed before its original eligibility boundary")
				}
				if sample.name != "TestAuthAutoRefreshLoop_PopDue_AfterSystemSuspendResume" && manager.shouldRefresh(
					sample.auth, sample.boundary.Add(-time.Nanosecond),
				) {
					t.Fatal("credential refreshed before its original eligibility boundary")
				}
				if got := manager.shouldRefresh(sample.auth, sample.boundary); got != sample.wantAfter {
					t.Fatalf("eligibility at %s=%t, want %t", sample.boundary, got, sample.wantAfter)
				}
			},
		)
	}
	t.Run(
		"TestNextRefreshCheckAt_APIKeyUnschedule", func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			executor := &refreshTimingExecutor{}
			manager.RegisterExecutor(executor)
			if _, err := manager.Register(
				t.Context(), &Auth{
					ID: "a1", Provider: executor.Identifier(), Attributes: map[string]string{"api_key": "k"},
					Runtime: alwaysDueRefresh{},
				},
			); err != nil {
				t.Fatal(err)
			}
			manager.checkRefreshes(t.Context())
			manager.refreshWorkers.Wait()
			if executor.calls.Load() != 0 {
				t.Fatal("API-key credential entered automatic OAuth token refresh")
			}
		},
	)
}

func TestAutoRefreshOriginalQueueCapacityAndRecovery(t *testing.T) {
	for _, sample := range []struct{ name, prefix string }{
		{"TestIssue6199AutoRefreshFullQueueDoesNotBlockScheduler", "full-queue"},
		{"TestIssue6199AutoRefreshFullQueueRetriesAfterCapacityReturns", "capacity"},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 2})
						executor := &refreshTimingExecutor{}
						manager.RegisterExecutor(executor)
						now := time.Now()
						capacity := cap(manager.refreshSemaphore)
						var queued []*autoRefreshJob
						var first, overflow *Auth
						for i := 0; i <= capacity; i++ {
							id := fmt.Sprintf("%s-%02d", sample.prefix, i)
							credential, err := manager.Register(
								t.Context(), &Auth{
									ID: id, Provider: executor.Identifier(), Status: StatusActive,
									LastRefreshedAt: now.Add(-time.Hour),
									Metadata: map[string]any{
										"access_token": "expired-access", "refresh_token": "refresh-" + id,
										"expires_at":               now.Add(-time.Hour).Format(time.RFC3339),
										"refresh_interval_seconds": 1,
									},
								},
							)
							if err != nil {
								t.Fatal(err)
							}
							if i == capacity {
								overflow = credential
								break
							}
							job := manager.markRefreshPending(credential.ID, credential.Generation, now)
							if job == nil {
								t.Fatal("original queued credential lost its pending slot")
							}
							queued = append(queued, job)
							// 待处理槽位需保留至队列容量和恢复行为检查完成
							// noinspection GoDeferInLoop
							defer manager.finishAutoRefreshJob(job)
							if first == nil {
								first = credential
							}
						}
						done := make(chan struct{})
						go func() {
							manager.checkRefreshes(t.Context())
							close(done)
						}()
						synctest.Wait()
						select {
						case <-done:
						default:
							t.Fatal("full refresh queue blocked the scheduler without a consumer")
						}
						manager.mu.RLock()
						pending, retained := len(manager.autoRefreshJobs), manager.auths[overflow.ID] != nil
						manager.mu.RUnlock()
						if pending != capacity || !retained || executor.calls.Load() != 0 {
							t.Fatalf(
								"full queue: pending=%d capacity=%d retained=%t calls=%d", pending, capacity, retained,
								executor.calls.Load(),
							)
						}
						manager.finishAutoRefreshJob(queued[0])
						first.LastRefreshedAt = now
						first.Metadata["expires_at"] = now.Add(time.Hour).Format(time.RFC3339)
						if _, err := manager.Update(t.Context(), first); err != nil {
							t.Fatal(err)
						}
						manager.checkRefreshes(t.Context())
						manager.refreshWorkers.Wait()
						current, _ := manager.GetByID(overflow.ID)
						if executor.calls.Load() != 1 || current.Metadata["access_token"] != "refreshed" {
							t.Fatalf(
								"overflow credential was not retried after capacity returned: calls=%d auth=%+v",
								executor.calls.Load(), current,
							)
						}
					},
				)
			},
		)
	}
}

func TestAutoRefreshOriginalCanceledQueueRestarts(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
			executor := &refreshTimingExecutor{}
			manager.RegisterExecutor(executor)
			now := time.Now()
			credential, err := manager.Register(
				t.Context(), &Auth{
					ID: "restart-queued", Provider: executor.Identifier(), Status: StatusActive,
					LastRefreshedAt: now.Add(-time.Hour),
					Metadata: map[string]any{
						"access_token": "expired-access", "refresh_token": "refresh-restart-queued",
						"expires_at": now.Add(-time.Hour).Format(time.RFC3339), "refresh_interval_seconds": 1,
					},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			manager.refreshSemaphore <- struct{}{}
			ctx, cancel := context.WithCancel(t.Context())
			manager.checkRefreshes(ctx)
			cancel()
			if err := manager.StopAutoRefresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			<-manager.refreshSemaphore
			current, _ := manager.GetByID(credential.ID)
			if executor.calls.Load() != 0 || !current.NextRefreshAfter.IsZero() {
				t.Fatalf(
					"canceled queue executed or retained pending state: calls=%d auth=%+v", executor.calls.Load(),
					current,
				)
			}
			manager.StartAutoRefresh(t.Context(), refreshCheckInterval)
			synctest.Wait()
			current, _ = manager.GetByID(credential.ID)
			if executor.calls.Load() != 1 || current.Metadata["access_token"] != "refreshed" {
				t.Fatalf(
					"canceled credential did not refresh after restart: calls=%d auth=%+v", executor.calls.Load(),
					current,
				)
			}
			if err := manager.StopAutoRefresh(t.Context()); err != nil {
				t.Fatal(err)
			}
		},
	)
}
