package auth

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func cleanupSchedulerCheckpoint(t *testing.T, scheduler *authScheduler) {
	t.Helper()
	t.Cleanup(
		func() {
			if errStop := scheduler.stopCheckpoint(context.Background()); errStop != nil {
				t.Errorf("stop checkpoint: %v", errStop)
			}
		},
	)
}

func TestRoundRobinSelectorUsesStableSuccessorAfterCandidateRemoval(t *testing.T) {
	selector := &RoundRobinSelector{}
	auths := []*Auth{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	for index, want := range []string{"a", "b"} {
		picked, errPick := selector.Pick(
			context.Background(), "gemini", "model", cliproxyexecutor.Options{}, auths,
		)
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", index, errPick)
		}
		if picked == nil || picked.ID != want {
			t.Fatalf("Pick() #%d = %#v, want %s", index, picked, want)
		}
	}

	picked, errPick := selector.Pick(
		context.Background(),
		"gemini",
		"model",
		cliproxyexecutor.Options{},
		[]*Auth{{ID: "d"}, {ID: "c"}, {ID: "a"}},
	)
	if errPick != nil {
		t.Fatalf("Pick() after removal error = %v", errPick)
	}
	if picked == nil || picked.ID != "c" {
		t.Fatalf("Pick() after removal = %#v, want c", picked)
	}
}

func TestSchedulerStableSuccessorSurvivesRebuild(t *testing.T) {
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "b", Provider: "gemini"},
		&Auth{ID: "c", Provider: "gemini"},
	)
	for index, want := range []string{"a", "b"} {
		picked, errPick := scheduler.pickSingle(
			context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if picked == nil || picked.ID != want {
			t.Fatalf("pickSingle() #%d = %#v, want %s", index, picked, want)
		}
	}

	scheduler.rebuild(
		[]*Auth{
			{ID: "d", Provider: "gemini"},
			{ID: "c", Provider: "gemini"},
			{ID: "a", Provider: "gemini"},
		},
	)
	picked, errPick := scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickSingle() after rebuild error = %v", errPick)
	}
	if picked == nil || picked.ID != "c" {
		t.Fatalf("pickSingle() after rebuild = %#v, want c", picked)
	}
}

func TestSchedulerStableSuccessorSurvivesAllCandidatesCooling(t *testing.T) {
	readyAuths := func() []*Auth {
		return []*Auth{
			{ID: "a", Provider: "gemini"},
			{ID: "b", Provider: "gemini"},
		}
	}
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, readyAuths()...)
	picked, errPick := scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("first pickSingle() = (%#v, %v), want a", picked, errPick)
	}

	nextRetry := time.Now().Add(time.Hour)
	scheduler.rebuild(
		[]*Auth{
			{ID: "a", Provider: "gemini", Unavailable: true, NextRetryAfter: nextRetry},
			{ID: "b", Provider: "gemini", Unavailable: true, NextRetryAfter: nextRetry},
		},
	)
	scheduler.rebuild(readyAuths())
	picked, errPick = scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("pickSingle() after cooldown rebuild = (%#v, %v), want b", picked, errPick)
	}
}

func TestSchedulerVirtualParentStableSuccessorSurvivesRebuild(t *testing.T) {
	auth := func(id, parent string) *Auth {
		return &Auth{
			ID:       id,
			Provider: "gemini-cli",
			Attributes: map[string]string{
				"gemini_virtual_parent": parent,
			},
		}
	}
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		auth("a-1", "a"),
		auth("b-1", "b"),
		auth("c-1", "c"),
	)
	for index, want := range []string{"a-1", "b-1"} {
		picked, errPick := scheduler.pickSingle(
			context.Background(), "gemini-cli", "", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if picked == nil || picked.ID != want {
			t.Fatalf("pickSingle() #%d = %#v, want %s", index, picked, want)
		}
	}

	scheduler.rebuild([]*Auth{auth("c-1", "c"), auth("a-1", "a")})
	picked, errPick := scheduler.pickSingle(
		context.Background(), "gemini-cli", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickSingle() after rebuild error = %v", errPick)
	}
	if picked == nil || picked.ID != "c-1" {
		t.Fatalf("pickSingle() after rebuild = %#v, want c-1", picked)
	}
}

func TestSchedulerMixedStableSuccessorIgnoresProviderReordering(t *testing.T) {
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini"},
		&Auth{ID: "gemini-b", Provider: "gemini"},
		&Auth{ID: "claude-a", Provider: "claude"},
	)
	picked, provider, errPick := scheduler.pickMixed(
		context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickMixed() error = %v", errPick)
	}
	if picked == nil || picked.ID != "gemini-a" || provider != "gemini" {
		t.Fatalf("pickMixed() = (%#v, %q), want gemini-a via gemini", picked, provider)
	}

	scheduler.rebuild(
		[]*Auth{
			{ID: "claude-a", Provider: "claude"},
			{ID: "gemini-b", Provider: "gemini"},
		},
	)
	picked, provider, errPick = scheduler.pickMixed(
		context.Background(), []string{"claude", "gemini"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickMixed() after reorder error = %v", errPick)
	}
	if picked == nil || picked.ID != "gemini-b" || provider != "gemini" {
		t.Fatalf("pickMixed() after reorder = (%#v, %q), want gemini-b via gemini", picked, provider)
	}
}

func TestSchedulerWeightedTieUsesStableSuccessorAfterWeightChange(t *testing.T) {
	authA := &Auth{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}}
	authB := &Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}}
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{}, authA, authB)
	picked, errPick := scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != "a" {
		t.Fatalf("pickSingle() = %#v, want a", picked)
	}

	authA.Attributes[AttributeWeight] = "2"
	authB.Attributes[AttributeWeight] = "2"
	scheduler.upsertAuth(authB)
	scheduler.upsertAuth(authA)
	picked, errPick = scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil {
		t.Fatalf("pickSingle() after weight change error = %v", errPick)
	}
	if picked == nil || picked.ID != "b" {
		t.Fatalf("pickSingle() after weight change = %#v, want b", picked)
	}
}

func TestSchedulerCheckpointRestoresRoundRobinProgress(t *testing.T) {
	auths := []*Auth{
		{ID: "a", Provider: "gemini"},
		{ID: "b", Provider: "gemini"},
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, errPick := first.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("first pickSingle() = (%#v, %v), want a", picked, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, errPick = second.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("restored pickSingle() = (%#v, %v), want b", picked, errPick)
	}
}

func TestSchedulerCheckpointRestoresCursorSavedWhileAllCandidatesCooling(t *testing.T) {
	readyAuths := func() []*Auth {
		return []*Auth{
			{ID: "a", Provider: "gemini"},
			{ID: "b", Provider: "gemini"},
		}
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(&RoundRobinSelector{}, readyAuths()...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, errPick := first.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("first pickSingle() = (%#v, %v), want a", picked, errPick)
	}
	nextRetry := time.Now().Add(time.Hour)
	first.rebuild(
		[]*Auth{
			{ID: "a", Provider: "gemini", Unavailable: true, NextRetryAfter: nextRetry},
			{ID: "b", Provider: "gemini", Unavailable: true, NextRetryAfter: nextRetry},
		},
	)
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(&RoundRobinSelector{}, readyAuths()...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, errPick = second.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("restored pickSingle() = (%#v, %v), want b", picked, errPick)
	}
}

func TestSchedulerCheckpointRestoresWeightedProgress(t *testing.T) {
	auths := []*Auth{
		{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "2"}},
		{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(&WeightedRoundRobinSelector{}, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, errPick := first.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("first weighted pickSingle() = (%#v, %v), want a", picked, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(&WeightedRoundRobinSelector{}, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, errPick = second.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("restored weighted pickSingle() = (%#v, %v), want b", picked, errPick)
	}
}

func TestSchedulerCheckpointRestoresWeightedSurvivorCreditsAfterPoolChange(t *testing.T) {
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "3"}},
		&Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "2"}},
		&Auth{ID: "c", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
	)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, errPick := first.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("first weighted pickSingle() = (%#v, %v), want a", picked, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "2"}},
		&Auth{ID: "c", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
		&Auth{ID: "d", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
	)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)

	provider := second.providers["gemini"]
	if provider == nil || provider.modelShards[""] == nil || provider.modelShards[""].readyByPriority[0] == nil {
		t.Fatal("restored weighted ready view is missing")
	}
	state := provider.modelShards[""].readyByPriority[0].all.weightedState
	if len(state.current) != 2 || state.current["b"] != 2 || state.current["c"] != 1 {
		t.Fatalf("restored survivor credits = %#v, want b:2 c:1", state.current)
	}
	if _, restoredRemoved := state.current["a"]; restoredRemoved {
		t.Fatalf("removed auth credit was restored: %#v", state.current)
	}
	wantWeights := map[string]int64{"b": 2, "c": 1, "d": 1}
	if !maps.Equal(state.weights, wantWeights) {
		t.Fatalf("restored weights = %#v, want %#v", state.weights, wantWeights)
	}
	if state.lastPicked != "a" {
		t.Fatalf("restored last picked = %q, want a", state.lastPicked)
	}
}

func TestSchedulerCheckpointRestoresVirtualParentProgress(t *testing.T) {
	auth := func(id, parent string) *Auth {
		return &Auth{
			ID:       id,
			Provider: "gemini-cli",
			Attributes: map[string]string{
				"gemini_virtual_parent": parent,
			},
		}
	}
	auths := []*Auth{auth("a-1", "a"), auth("b-1", "b")}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, errPick := first.pickSingle(
		context.Background(), "gemini-cli", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a-1" {
		t.Fatalf("first virtual-parent pickSingle() = (%#v, %v), want a-1", picked, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, errPick = second.pickSingle(
		context.Background(), "gemini-cli", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "b-1" {
		t.Fatalf("restored virtual-parent pickSingle() = (%#v, %v), want b-1", picked, errPick)
	}
}

func TestSchedulerCheckpointRestoresMixedProgressAcrossProviderOrder(t *testing.T) {
	auths := []*Auth{
		{ID: "gemini-a", Provider: "gemini"},
		{ID: "gemini-b", Provider: "gemini"},
		{ID: "claude-a", Provider: "claude"},
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, _, errPick := first.pickMixed(
		context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "gemini-a" {
		t.Fatalf("first pickMixed() = (%#v, %v), want gemini-a", picked, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, provider, errPick := second.pickMixed(
		context.Background(), []string{"claude", "gemini"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "gemini-b" || provider != "gemini" {
		t.Fatalf("restored pickMixed() = (%#v, %q, %v), want gemini-b via gemini", picked, provider, errPick)
	}
}

func TestSchedulerCheckpointRestoresQuotaAwarePoolProgress(t *testing.T) {
	auths := []*Auth{
		quotaAwareTestAuth("a", "a-index", "gemini", 0, 1),
		quotaAwareTestAuth("b", "b-index", "gemini", 0, 1),
	}
	provider := staticQuotaSchedulingProvider{
		byProvider: map[string]map[string]QuotaSchedulingSnapshot{
			"gemini": {
				"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
				"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
			},
		},
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newQuotaAwareSchedulerForTest(provider, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, errPick := first.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("first quota-aware pickSingle() = (%#v, %v), want a", picked, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}
	encoded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if !strings.Contains(string(encoded), `"version":2`) ||
		!strings.Contains(string(encoded), `"quota"`) {
		t.Fatalf("quota-aware checkpoint = %s", encoded)
	}

	second := newQuotaAwareSchedulerForTest(provider, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, errPick = second.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("restored quota-aware pickSingle() = (%#v, %v), want b", picked, errPick)
	}
}

func TestSchedulerCheckpointRestoresMixedQuotaAwarePoolProgress(t *testing.T) {
	auths := []*Auth{
		quotaAwareTestAuth("claude", "claude-index", "claude", 0, 1),
		quotaAwareTestAuth("gemini", "gemini-index", "gemini", 0, 1),
	}
	provider := staticQuotaSchedulingProvider{
		byProvider: map[string]map[string]QuotaSchedulingSnapshot{
			"claude": {
				"claude-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
			},
			"gemini": {
				"gemini-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.5},
			},
		},
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newQuotaAwareSchedulerForTest(provider, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	picked, selectedProvider, errPick := first.pickMixed(
		context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "claude" || selectedProvider != "claude" {
		t.Fatalf("first quota-aware pickMixed() = (%#v, %q, %v), want claude", picked, selectedProvider, errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newQuotaAwareSchedulerForTest(provider, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("restore startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, selectedProvider, errPick = second.pickMixed(
		context.Background(), []string{"claude", "gemini"}, "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "gemini" || selectedProvider != "gemini" {
		t.Fatalf("restored quota-aware pickMixed() = (%#v, %q, %v), want gemini", picked, selectedProvider, errPick)
	}
}

func TestDecodeSchedulerCheckpointAcceptsVersionOne(t *testing.T) {
	state, errDecode := decodeSchedulerCheckpoint([]byte(`{"version":1,"strategy":"round-robin"}`))
	if errDecode != nil {
		t.Fatalf("decodeSchedulerCheckpoint() error = %v", errDecode)
	}
	if state.strategy != schedulerStrategyRoundRobin {
		t.Fatalf("decoded strategy = %v, want round-robin", state.strategy)
	}
}

func TestSchedulerCheckpointDoesNotCrossStrategies(t *testing.T) {
	auths := []*Auth{
		{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
		{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
	}
	store := NewFileSchedulerCheckpointStore(t.TempDir())
	first := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	if errStart := first.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	if _, errPick := first.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	); errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if errStop := first.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("stopCheckpoint() error = %v", errStop)
	}

	second := newSchedulerForTest(&WeightedRoundRobinSelector{}, auths...)
	if errStart := second.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("weighted startCheckpoint() error = %v", errStart)
	}
	cleanupSchedulerCheckpoint(t, second)
	picked, errPick := second.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("weighted pickSingle() = (%#v, %v), want fresh strategy pick a", picked, errPick)
	}
}

func TestFileSchedulerCheckpointStoreUsesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	store := NewFileSchedulerCheckpointStore(dir)
	data := []byte(`{"version":1,"strategy":"round-robin"}` + "\n")
	if errSave := store.Save(context.Background(), data); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	path := filepath.Join(dir, schedulerCheckpointFileName)
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("Stat() error = %v", errStat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("checkpoint mode = %o, want 600", got)
	}
	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if string(loaded) != string(data) {
		t.Fatalf("Load() = %q, want %q", loaded, data)
	}
}

func TestFileSchedulerCheckpointStoreSecuresExistingFileAndDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	if errMkdir := os.Mkdir(dir, 0o755); errMkdir != nil {
		t.Fatalf("Mkdir() error = %v", errMkdir)
	}
	path := filepath.Join(dir, schedulerCheckpointFileName)
	data := []byte(`{"version":1,"strategy":"round-robin"}` + "\n")
	if errWrite := os.WriteFile(path, data, 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	store := NewFileSchedulerCheckpointStore(dir)
	if _, errLoad := store.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	fileInfo, errFile := os.Stat(path)
	if errFile != nil {
		t.Fatalf("Stat(checkpoint) error = %v", errFile)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("checkpoint mode = %o, want 600", got)
	}
	dirInfo, errDir := os.Stat(dir)
	if errDir != nil {
		t.Fatalf("Stat(directory) error = %v", errDir)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 700", got)
	}
}

func TestFileSchedulerCheckpointStoreRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	data := []byte(`{"version":1,"strategy":"round-robin"}` + "\n")
	if errWrite := os.WriteFile(target, data, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	if errLink := os.Symlink(target, filepath.Join(dir, schedulerCheckpointFileName)); errLink != nil {
		t.Skipf("Symlink() unavailable: %v", errLink)
	}
	store := NewFileSchedulerCheckpointStore(dir)
	if _, errLoad := store.Load(context.Background()); errLoad == nil {
		t.Fatal("Load() error = nil, want symlink rejection")
	}
}

func TestFileSchedulerCheckpointStoreAtomicallyReplacesData(t *testing.T) {
	dir := t.TempDir()
	store := NewFileSchedulerCheckpointStore(dir)
	first := []byte(`{"version":1,"strategy":"round-robin","mixed_cursors":{"key":"a"}}` + "\n")
	second := []byte(`{"version":1,"strategy":"round-robin","mixed_cursors":{"key":"b"}}` + "\n")
	if errSave := store.Save(context.Background(), first); errSave != nil {
		t.Fatalf("first Save() error = %v", errSave)
	}
	if errSave := store.Save(context.Background(), second); errSave != nil {
		t.Fatalf("second Save() error = %v", errSave)
	}
	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if string(loaded) != string(second) {
		t.Fatalf("Load() = %q, want %q", loaded, second)
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("ReadDir() error = %v", errRead)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary checkpoint remains: %s", entry.Name())
		}
	}
}

func TestDecodeSchedulerCheckpointRejectsUnknownAndTrailingJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{
			name: "unknown_field",
			data: `{"version":1,"strategy":"round-robin","unknown":true}`,
		},
		{
			name: "trailing_value",
			data: `{"version":1,"strategy":"round-robin"} {"version":1}`,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if _, errDecode := decodeSchedulerCheckpoint([]byte(test.data)); errDecode == nil {
					t.Fatal("decodeSchedulerCheckpoint() error = nil")
				}
			},
		)
	}
}

type blockingSchedulerCheckpointStore struct {
	saveStarted chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	mu          sync.Mutex
	saveCount   int
}

func newBlockingSchedulerCheckpointStore() *blockingSchedulerCheckpointStore {
	return &blockingSchedulerCheckpointStore{
		saveStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (*blockingSchedulerCheckpointStore) Load(context.Context) ([]byte, error) {
	return nil, nil
}

func (s *blockingSchedulerCheckpointStore) Save(ctx context.Context, _ []byte) error {
	s.startOnce.Do(
		func() {
			close(s.saveStarted)
		},
	)
	select {
	case <-s.release:
		s.mu.Lock()
		s.saveCount++
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSchedulerCheckpointStopTimeoutCanBeRetried(t *testing.T) {
	store := newBlockingSchedulerCheckpointStore()
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "b", Provider: "gemini"},
	)
	if errStart := scheduler.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	if _, errPick := scheduler.pickSingle(
		context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
	); errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	select {
	case <-store.saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background checkpoint save did not start")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelStop()
	if errStop := scheduler.stopCheckpoint(stopCtx); !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("first stopCheckpoint() error = %v, want deadline exceeded", errStop)
	}
	if errStart := scheduler.startCheckpoint(context.Background(), store); errStart == nil {
		t.Fatal("startCheckpoint() error = nil while previous checkpoint is stopping")
	}

	close(store.release)
	if errStop := scheduler.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("retried stopCheckpoint() error = %v", errStop)
	}
	store.mu.Lock()
	saveCount := store.saveCount
	store.mu.Unlock()
	if saveCount < 2 {
		t.Fatalf("checkpoint saves = %d, want background and final saves", saveCount)
	}
	if errStart := scheduler.startCheckpoint(context.Background(), store); errStart != nil {
		t.Fatalf("startCheckpoint() after retried stop error = %v", errStart)
	}
	if errStop := scheduler.stopCheckpoint(context.Background()); errStop != nil {
		t.Fatalf("final stopCheckpoint() error = %v", errStop)
	}
}

type failingSchedulerCheckpointStore struct{}

func (failingSchedulerCheckpointStore) Load(context.Context) ([]byte, error) {
	return nil, nil
}

func (failingSchedulerCheckpointStore) Save(context.Context, []byte) error {
	return errors.New("save failed")
}

func TestSchedulerCheckpointSaveFailureDoesNotChangeSelection(t *testing.T) {
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "b", Provider: "gemini"},
	)
	if errStart := scheduler.startCheckpoint(context.Background(), failingSchedulerCheckpointStore{}); errStart != nil {
		t.Fatalf("startCheckpoint() error = %v", errStart)
	}
	for index, want := range []string{"a", "b"} {
		picked, errPick := scheduler.pickSingle(
			context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if picked == nil || picked.ID != want {
			t.Fatalf("pickSingle() #%d = %#v, want %s", index, picked, want)
		}
	}
	if errStop := scheduler.stopCheckpoint(context.Background()); errStop == nil {
		t.Fatal("stopCheckpoint() error = nil, want persistence failure")
	}
}
