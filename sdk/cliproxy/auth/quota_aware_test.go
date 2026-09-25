package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type staticQuotaSchedulingProvider struct {
	byProvider map[string]map[string]QuotaSchedulingSnapshot
}

func (p staticQuotaSchedulingProvider) Snapshots(
	provider,
	_ string,
	authIndexes []string,
) map[string]QuotaSchedulingSnapshot {
	available := p.byProvider[provider]
	result := make(map[string]QuotaSchedulingSnapshot, len(authIndexes))
	for _, authIndex := range authIndexes {
		if snapshot, ok := available[authIndex]; ok {
			result[authIndex] = snapshot
		}
	}
	return result
}

func quotaAwareTestAuth(id, index, provider string, priority int, weight int64) *Auth {
	auth := &Auth{
		ID:       id,
		Provider: provider,
		Attributes: map[string]string{
			"priority":      fmt.Sprintf("%d", priority),
			AttributeWeight: fmt.Sprintf("%d", weight),
		},
	}
	auth.AssignIndex(index)
	return auth
}

func newQuotaAwareSchedulerForTest(
	provider QuotaSchedulingProvider,
	auths ...*Auth,
) *authScheduler {
	scheduler := newSchedulerForTest(&QuotaAwareSelector{}, auths...)
	scheduler.setQuotaSchedulingProvider(provider)
	return scheduler
}

func TestSchedulerQuotaAwareHonorsEligibilityAndPriorityBeforeQuota(t *testing.T) {
	disabled := quotaAwareTestAuth("disabled", "disabled-index", "gemini", 20, 1)
	disabled.Disabled = true
	scheduler := newQuotaAwareSchedulerForTest(
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				"gemini": {
					"disabled-index": {Status: QuotaSchedulingStatusFresh, Headroom: 1},
					"high-index":     {Status: QuotaSchedulingStatusFresh, Headroom: 0.1},
					"low-index":      {Status: QuotaSchedulingStatusFresh, Headroom: 0.9},
				},
			},
		},
		disabled,
		quotaAwareTestAuth("high", "high-index", "gemini", 10, 1),
		quotaAwareTestAuth("low", "low-index", "gemini", 1, 1),
	)

	picked, errPick := scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != "high" {
		t.Fatalf("pickSingle() = %#v, want high", picked)
	}
}

func TestSchedulerQuotaAwareOrdersFreshPools(t *testing.T) {
	t.Run(
		"headroom", func(t *testing.T) {
			scheduler := newQuotaAwareSchedulerForTest(
				staticQuotaSchedulingProvider{
					byProvider: map[string]map[string]QuotaSchedulingSnapshot{
						"gemini": {
							"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.4},
							"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.8},
						},
					},
				},
				quotaAwareTestAuth("a", "a-index", "gemini", 0, 1),
				quotaAwareTestAuth("b", "b-index", "gemini", 0, 1),
			)
			picked, errPick := scheduler.pickSingle(
				context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
			)
			if errPick != nil || picked == nil || picked.ID != "b" {
				t.Fatalf("pickSingle() = (%#v, %v), want b", picked, errPick)
			}
		},
	)

	t.Run(
		"reset_proximity", func(t *testing.T) {
			now := time.Now()
			earlier := now.Add(time.Hour)
			later := now.Add(2 * time.Hour)
			scheduler := newQuotaAwareSchedulerForTest(
				staticQuotaSchedulingProvider{
					byProvider: map[string]map[string]QuotaSchedulingSnapshot{
						"gemini": {
							"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5, ResetAt: &later},
							"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5, ResetAt: &earlier},
						},
					},
				},
				quotaAwareTestAuth("a", "a-index", "gemini", 0, 1),
				quotaAwareTestAuth("b", "b-index", "gemini", 0, 1),
			)
			picked, errPick := scheduler.pickSingle(
				context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
			)
			if errPick != nil || picked == nil || picked.ID != "b" {
				t.Fatalf("pickSingle() = (%#v, %v), want b", picked, errPick)
			}
		},
	)

	t.Run(
		"stable_pool_cursor", func(t *testing.T) {
			scheduler := newQuotaAwareSchedulerForTest(
				staticQuotaSchedulingProvider{
					byProvider: map[string]map[string]QuotaSchedulingSnapshot{
						"gemini": {
							"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
							"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
						},
					},
				},
				quotaAwareTestAuth("a", "a-index", "gemini", 0, 1),
				quotaAwareTestAuth("b", "b-index", "gemini", 0, 1),
			)
			for index, want := range []string{"a", "b", "a"} {
				picked, errPick := scheduler.pickSingle(
					context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
				)
				if errPick != nil || picked == nil || picked.ID != want {
					t.Fatalf("pickSingle() #%d = (%#v, %v), want %s", index, picked, errPick, want)
				}
			}
		},
	)
}

func TestSchedulerQuotaAwareFallsBackToWeightedForIncompleteQuota(t *testing.T) {
	tests := []struct {
		name     string
		second   QuotaSchedulingSnapshot
		includeB bool
	}{
		{name: "unknown", includeB: false},
		{
			name:     "stale",
			second:   QuotaSchedulingSnapshot{Status: QuotaSchedulingStatusStale, Headroom: 0.9},
			includeB: true,
		},
		{
			name:     "invalid_fresh",
			second:   QuotaSchedulingSnapshot{Status: QuotaSchedulingStatusFresh, Headroom: 2},
			includeB: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				snapshots := map[string]QuotaSchedulingSnapshot{
					"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.1},
				}
				if test.includeB {
					snapshots["b-index"] = test.second
				}
				scheduler := newQuotaAwareSchedulerForTest(
					staticQuotaSchedulingProvider{
						byProvider: map[string]map[string]QuotaSchedulingSnapshot{
							"gemini": snapshots,
						},
					},
					quotaAwareTestAuth("a", "a-index", "gemini", 0, 2),
					quotaAwareTestAuth("b", "b-index", "gemini", 0, 1),
				)
				counts := make(map[string]int)
				for index := range 30 {
					picked, errPick := scheduler.pickSingle(
						context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
					)
					if errPick != nil {
						t.Fatalf("pickSingle() #%d error = %v", index, errPick)
					}
					counts[picked.ID]++
				}
				if counts["a"] != 20 || counts["b"] != 10 {
					t.Fatalf("weighted fallback counts = %#v, want a:b=20:10", counts)
				}
			},
		)
	}
}

func TestSchedulerQuotaAwarePreservesWeightedCreditsAcrossRetryTriedFilter(t *testing.T) {
	tests := []struct {
		name       string
		sharedPool bool
	}{
		{name: "weighted_fallback"},
		{name: "fresh_pool", sharedPool: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ids := []string{"a", "b", "c", "d"}
			weights := []int64{5, 3, 2, 1}
			auths := make([]*Auth, 0, len(weights))
			snapshots := make(map[string]QuotaSchedulingSnapshot)
			for index, weight := range weights {
				authID := ids[index]
				authIndex := authID + "-index"
				if test.sharedPool {
					authIndex = "shared-index"
					snapshots[authIndex] = QuotaSchedulingSnapshot{
						Status:   QuotaSchedulingStatusFresh,
						Headroom: 0.5,
					}
				}
				auths = append(auths, quotaAwareTestAuth(authID, authIndex, "gemini", 0, weight))
			}
			scheduler := newQuotaAwareSchedulerForTest(
				staticQuotaSchedulingProvider{
					byProvider: map[string]map[string]QuotaSchedulingSnapshot{"gemini": snapshots},
				},
				auths...,
			)

			assertWeightedRetryDistribution(t, func(tried map[string]struct{}) (*Auth, error) {
				return scheduler.pickSingle(
					context.Background(), "gemini", "", cliproxyexecutor.Options{}, tried,
				)
			})
		})
	}
}

func TestSchedulerQuotaAwareUsesInFlightPressure(t *testing.T) {
	scheduler := newQuotaAwareSchedulerForTest(
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				"gemini": {
					"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.8},
					"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
				},
			},
		},
		quotaAwareTestAuth("a", "a-index", "gemini", 0, 1),
		quotaAwareTestAuth("b", "b-index", "gemini", 0, 1),
	)

	firstCtx, firstRequest := withQuotaInFlightRequest(context.Background())
	first, errPick := scheduler.pickSingle(
		firstCtx, "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || first == nil || first.ID != "a" {
		t.Fatalf("first pickSingle() = (%#v, %v), want a", first, errPick)
	}
	firstLease := firstRequest.take()
	if firstLease == nil {
		t.Fatal("first quota in-flight lease = nil")
	}
	defer firstLease.Release()

	secondCtx, secondRequest := withQuotaInFlightRequest(context.Background())
	second, errPick := scheduler.pickSingle(
		secondCtx, "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || second == nil || second.ID != "b" {
		t.Fatalf("second pickSingle() = (%#v, %v), want b", second, errPick)
	}
	secondLease := secondRequest.take()
	if secondLease == nil {
		t.Fatal("second quota in-flight lease = nil")
	}
	secondLease.Release()
}

func TestSchedulerQuotaAwareWeightsCredentialsWithinSharedPool(t *testing.T) {
	scheduler := newQuotaAwareSchedulerForTest(
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				"gemini-cli": {
					"shared-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.7},
				},
			},
		},
		quotaAwareTestAuth("child-a", "shared-index", "gemini-cli", 0, 2),
		quotaAwareTestAuth("child-b", "shared-index", "gemini-cli", 0, 1),
	)
	counts := make(map[string]int)
	for index := range 30 {
		picked, errPick := scheduler.pickSingle(
			context.Background(), "gemini-cli", "", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		counts[picked.ID]++
	}
	if counts["child-a"] != 20 || counts["child-b"] != 10 {
		t.Fatalf("shared pool counts = %#v, want child-a:child-b=20:10", counts)
	}
}

func TestSchedulerQuotaAwareComparesPoolsAcrossProviders(t *testing.T) {
	scheduler := newQuotaAwareSchedulerForTest(
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				"claude": {
					"claude-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.9},
				},
				"gemini": {
					"gemini-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.4},
				},
			},
		},
		quotaAwareTestAuth("claude", "claude-index", "claude", 0, 1),
		quotaAwareTestAuth("gemini", "gemini-index", "gemini", 0, 1),
	)
	picked, provider, errPick := scheduler.pickMixed(
		context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "claude" || provider != "claude" {
		t.Fatalf("pickMixed() = (%#v, %q, %v), want claude", picked, provider, errPick)
	}
}

func TestSchedulerQuotaAwareDoesNotDisableZeroHeadroom(t *testing.T) {
	scheduler := newQuotaAwareSchedulerForTest(
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				"gemini": {
					"zero-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0},
				},
			},
		},
		quotaAwareTestAuth("zero", "zero-index", "gemini", 0, 1),
	)
	picked, errPick := scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "zero" {
		t.Fatalf("pickSingle() = (%#v, %v), want zero", picked, errPick)
	}
}

func TestQuotaInFlightLeaseExpiryAndReleaseAreIdempotent(t *testing.T) {
	auth := quotaAwareTestAuth("a", "a-index", "gemini", 0, 1)
	scheduler := newSchedulerForTest(&QuotaAwareSelector{}, auth)
	now := time.Now()

	scheduler.mu.Lock()
	lease := scheduler.acquireQuotaInFlightLocked(auth, "gemini", now)
	if lease == nil || scheduler.quotaInFlightByPool[quotaPoolKey(auth, "gemini")] != 1 {
		scheduler.mu.Unlock()
		t.Fatalf("acquired lease = %#v, counts = %#v", lease, scheduler.quotaInFlightByPool)
	}
	scheduler.expireQuotaInFlightLocked(now.Add(quotaAwareInFlightLeaseTTL))
	remaining := scheduler.quotaInFlightByPool[quotaPoolKey(auth, "gemini")]
	scheduler.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("in-flight count after expiry = %d, want 0", remaining)
	}

	lease.Release()
	lease.Release()
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if len(scheduler.quotaInFlight) != 0 || len(scheduler.quotaInFlightByPool) != 0 {
		t.Fatalf(
			"in-flight state after idempotent release = (%#v, %#v)", scheduler.quotaInFlight,
			scheduler.quotaInFlightByPool,
		)
	}
}
