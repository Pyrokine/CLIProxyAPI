package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
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
