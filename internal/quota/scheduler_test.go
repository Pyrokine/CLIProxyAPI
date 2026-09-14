package quota

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestScheduler_RestorePreservesRegisteredDisabledState(t *testing.T) {
	tests := []struct {
		name               string
		registeredDisabled bool
		persistedDisabled  bool
	}{
		{
			name:               "registered disabled stays disabled",
			registeredDisabled: true,
			persistedDisabled:  false,
		},
		{
			name:               "registered enabled stays enabled",
			registeredDisabled: false,
			persistedDisabled:  true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				scheduler := NewScheduler(DefaultConfig(), nil)
				scheduler.Register("claude-user.json", TypeClaude, "claude")
				scheduler.SetDisabled("claude-user.json", TypeClaude, tt.registeredDisabled)

				lastRefresh := time.Now().Add(-time.Hour)
				nextRefresh := time.Now().Add(time.Hour)
				scheduler.Restore(
					map[string]EntryState{
						EntryKey(TypeClaude, "claude-user.json"): {
							FileName:    "claude-user.json",
							Type:        TypeClaude,
							AuthIndex:   "claude",
							Status:      StatusSuccess,
							LastRefresh: &lastRefresh,
							NextRefresh: &nextRefresh,
							Disabled:    tt.persistedDisabled,
						},
					},
				)

				entry := scheduler.GetStatus().Credentials[EntryKey(TypeClaude, "claude-user.json")]
				if entry == nil {
					t.Fatal("missing restored entry")
				}
				if entry.Disabled != tt.registeredDisabled {
					t.Fatalf("Disabled = %v, want registered state %v", entry.Disabled, tt.registeredDisabled)
				}
				if entry.Status != StatusSuccess {
					t.Fatalf("Status = %s, want %s", entry.Status, StatusSuccess)
				}
			},
		)
	}
}

func TestSchedulerRegisterUsesProviderAwareStatusKeys(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("shared.json", TypeClaude, "claude-auth")
	scheduler.Register("shared.json", TypeCodex, "codex-auth")

	status := scheduler.GetStatus()
	claude := status.Credentials[EntryKey(TypeClaude, "shared.json")]
	codex := status.Credentials[EntryKey(TypeCodex, "shared.json")]
	if claude == nil || codex == nil {
		t.Fatalf("credentials = %+v, want claude and codex entries", status.Credentials)
	}
	if claude.Type != TypeClaude || claude.AuthIndex != "claude-auth" {
		t.Fatalf("claude entry = %+v", claude)
	}
	if codex.Type != TypeCodex || codex.AuthIndex != "codex-auth" {
		t.Fatalf("codex entry = %+v", codex)
	}
}

func TestSchedulerGetStatusReturnsIndependentSnapshot(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("claude-user.json", TypeClaude, "claude-auth")
	key := EntryKey(TypeClaude, "claude-user.json")

	entry := scheduler.entries[key]
	entry.mu.Lock()
	entry.Data = append(entry.Data[:0], []byte(`{"remaining":10}`)...)
	entry.mu.Unlock()

	status := scheduler.GetStatus()
	snapshot := status.Credentials[key]
	if snapshot == nil {
		t.Fatal("missing quota status snapshot")
	}

	entry.mu.Lock()
	entry.Data[13] = '0'
	entry.mu.Unlock()

	if got := string(snapshot.Data); got != `{"remaining":10}` {
		t.Fatalf("snapshot data = %s, want independent copy", got)
	}
}

func TestSchedulerRegisterReplacesSameAuthWhenCredentialTypeChanges(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("shared.json", TypeClaude, "auth-1")
	scheduler.Register("shared.json", TypeCodex, "auth-1")

	status := scheduler.GetStatus()
	if status.Credentials[EntryKey(TypeClaude, "shared.json")] != nil {
		t.Fatalf("stale claude entry was not removed: %+v", status.Credentials)
	}
	got := status.Credentials[EntryKey(TypeCodex, "shared.json")]
	if got == nil {
		t.Fatal("missing codex entry")
	}
	if got.Type != TypeCodex || got.AuthIndex != "auth-1" {
		t.Fatalf("entry identity = (%s, %s), want (%s, auth-1)", got.Type, got.AuthIndex, TypeCodex)
	}
	if got.Status != StatusIdle || got.LastRefresh != nil || got.Data != nil || got.FailureCount != 0 {
		t.Fatalf("new entry started with stale quota state: %+v", got)
	}
}

func TestSchedulerRefreshNowFileNameRefreshesMatchingProviderEntries(t *testing.T) {
	refreshed := make(map[CredentialType]int)
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(_ context.Context, entry *Entry) ([]byte, error) {
			refreshed[entry.Type]++
			return []byte(`{}`), nil
		},
	)

	scheduler.Register("shared.json", TypeClaude, "claude-auth")
	scheduler.Register("shared.json", TypeCodex, "codex-auth")

	scheduler.RefreshNow([]string{"shared.json"})

	if refreshed[TypeClaude] != 1 || refreshed[TypeCodex] != 1 {
		t.Fatalf("refreshed = %+v, want both provider entries", refreshed)
	}
}

func TestScheduler_RefreshNowAllowsUnrelatedConcurrentRequests(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(_ context.Context, entry *Entry) ([]byte, error) {
			started <- entry.FileName
			<-release
			return []byte(`{}`), nil
		},
	)

	scheduler.Register("codex-user.json", TypeCodex, "codex")
	scheduler.Register("gemini-user.json", TypeGeminiCli, "gemini")

	done := make(chan struct{}, 2)
	go func() {
		scheduler.RefreshNow([]string{"codex-user.json"})
		done <- struct{}{}
	}()
	go func() {
		scheduler.RefreshNow([]string{"gemini-user.json"})
		done <- struct{}{}
	}()

	seen := make(map[string]bool, 2)
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatalf("concurrent refreshes started for %v, want both credentials", seen)
		}
	}
	close(release)
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent refresh did not return")
		}
	}

	status := scheduler.GetStatus()
	typeByName := map[string]CredentialType{
		"codex-user.json":  TypeCodex,
		"gemini-user.json": TypeGeminiCli,
	}
	for name, credType := range typeByName {
		entry := status.Credentials[EntryKey(credType, name)]
		if entry == nil || entry.LastRefresh == nil || entry.Status != StatusSuccess {
			t.Fatalf("status for %s = %#v, want refreshed success", name, entry)
		}
	}
}

func TestSchedulerRefreshTargetSharesProviderFetch(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(context.Context, *Entry) ([]byte, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			started <- struct{}{}
			<-release
			return []byte(`{"remaining":10}`), nil
		},
	)
	scheduler.Register("shared.json", TypeClaude, "auth-1")
	targets, unresolved := scheduler.ResolveRefreshTargets([]string{EntryKey(TypeClaude, "shared.json")})
	if len(targets) != 1 || len(unresolved) != 0 {
		t.Fatalf("resolved targets = %#v unresolved = %#v", targets, unresolved)
	}

	firstResult := make(chan RefreshResult, 1)
	secondResult := make(chan RefreshResult, 1)
	go func() { firstResult <- scheduler.RefreshTarget(context.Background(), targets[0]) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared refresh did not start")
	}
	go func() { secondResult <- scheduler.RefreshTarget(context.Background(), targets[0]) }()
	deadline := time.Now().Add(time.Second)
	for {
		scheduler.flightMu.Lock()
		waiters := 0
		for _, flight := range scheduler.flights {
			waiters = flight.waiters
		}
		scheduler.flightMu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second caller did not join shared refresh")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)

	first := <-firstResult
	second := <-secondResult
	if first.Outcome != RefreshOutcomeSucceeded || second.Outcome != RefreshOutcomeSucceeded {
		t.Fatalf("shared outcomes = %s and %s", first.Outcome, second.Outcome)
	}
	if !second.Shared {
		t.Fatal("second caller did not report shared execution")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider fetch calls = %d, want 1", calls)
	}
}

func TestSchedulerRefreshTargetCancelsOnlyDepartingWaiter(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(ctx context.Context, _ *Entry) ([]byte, error) {
			started <- struct{}{}
			select {
			case <-release:
				return []byte(`{}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	)
	scheduler.Register("shared.json", TypeClaude, "auth-1")
	targets, _ := scheduler.ResolveRefreshTargets([]string{"shared.json"})

	firstResult := make(chan RefreshResult, 1)
	go func() { firstResult <- scheduler.RefreshTarget(context.Background(), targets[0]) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared refresh did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	secondResult := make(chan RefreshResult, 1)
	go func() { secondResult <- scheduler.RefreshTarget(ctx, targets[0]) }()
	cancel()
	second := <-secondResult
	if second.Outcome != RefreshOutcomeCancelled {
		t.Fatalf("cancelled waiter outcome = %s, want %s", second.Outcome, RefreshOutcomeCancelled)
	}

	close(release)
	first := <-firstResult
	if first.Outcome != RefreshOutcomeSucceeded {
		t.Fatalf("remaining waiter outcome = %s, want %s", first.Outcome, RefreshOutcomeSucceeded)
	}
}

func TestSchedulerRefreshTargetCancellationPreservesLastKnownState(t *testing.T) {
	started := make(chan struct{}, 1)
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(ctx context.Context, _ *Entry) ([]byte, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	key := EntryKey(TypeClaude, "claude.json")
	lastRefresh := time.Now().UTC().Add(-time.Hour)
	entry := scheduler.entries[key]
	entry.mu.Lock()
	entry.Status = StatusSuccess
	entry.LastRefresh = &lastRefresh
	entry.Data = []byte(`{"remaining":5}`)
	entry.FailureCount = 2
	entry.mu.Unlock()
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})

	ctx, cancel := context.WithCancel(context.Background())
	resultChannel := make(chan RefreshResult, 1)
	go func() { resultChannel <- scheduler.RefreshTarget(ctx, targets[0]) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	result := <-resultChannel
	if result.Outcome != RefreshOutcomeCancelled {
		t.Fatalf("outcome = %s, want %s", result.Outcome, RefreshOutcomeCancelled)
	}
	state := scheduler.GetStatus().Credentials[key]
	if state.Status != StatusSuccess || state.FailureCount != 2 ||
		state.LastRefresh == nil || !state.LastRefresh.Equal(lastRefresh) || string(state.Data) != `{"remaining":5}` {
		t.Fatalf("state after cancellation = %#v", state)
	}
}

func TestSchedulerRefreshTargetCancellationPreservesConcurrentDisabledState(t *testing.T) {
	started := make(chan struct{}, 1)
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(ctx context.Context, _ *Entry) ([]byte, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	key := EntryKey(TypeClaude, "claude.json")
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})

	ctx, cancel := context.WithCancel(context.Background())
	resultChannel := make(chan RefreshResult, 1)
	go func() { resultChannel <- scheduler.RefreshTarget(ctx, targets[0]) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	scheduler.SetDisabled("claude.json", TypeClaude, true)
	cancel()
	result := <-resultChannel
	if result.Outcome != RefreshOutcomeCancelled {
		t.Fatalf("outcome = %s, want %s", result.Outcome, RefreshOutcomeCancelled)
	}
	state := scheduler.GetStatus().Credentials[key]
	if state == nil || !state.Disabled || state.Status != StatusIdle {
		t.Fatalf("state after cancellation = %#v", state)
	}
}

func TestSchedulerRefreshTargetRejectsChangedBinding(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(context.Context, *Entry) ([]byte, error) {
			started <- struct{}{}
			<-release
			return []byte(`{"remaining":10}`), nil
		},
	)
	finalizerCalls := 0
	scheduler.setRefreshFinalizer(
		func(RefreshAttempt) error {
			finalizerCalls++
			return nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	key := EntryKey(TypeClaude, "claude.json")
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})

	resultChannel := make(chan RefreshResult, 1)
	go func() { resultChannel <- scheduler.RefreshTarget(context.Background(), targets[0]) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	scheduler.updateAuthIndex("claude.json", TypeClaude, "auth-2")
	close(release)
	result := <-resultChannel
	if result.Outcome != RefreshOutcomeInconclusive || result.Error != errRefreshTargetChanged.Error() {
		t.Fatalf("changed binding result = %#v", result)
	}
	if finalizerCalls != 0 {
		t.Fatalf("stale target finalizer calls = %d, want 0", finalizerCalls)
	}
	state := scheduler.GetStatus().Credentials[key]
	if state.AuthIndex != "auth-2" || state.Status != StatusIdle || state.LastRefresh != nil || state.Data != nil {
		t.Fatalf("changed binding published stale result: %#v", state)
	}
}

func TestSchedulerManualRefreshFinalizerRunsBeforeSuccessPublication(t *testing.T) {
	var fetchStartedAt time.Time
	scheduler := NewScheduler(
		DefaultConfig(), func(*Entry) ([]byte, error) {
			fetchStartedAt = time.Now().UTC()
			return []byte(`{"rate_limit":{"allowed":true}}`), nil
		},
	)
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")

	var attempt RefreshAttempt
	scheduler.refreshNowWithFinalizer(
		[]string{"codex-user.json"}, func(current RefreshAttempt) error {
			attempt = current
			status := scheduler.GetStatus().Credentials[EntryKey(TypeCodex, "codex-user.json")]
			if status == nil || status.Status != statusLoading || status.LastRefresh != nil {
				t.Fatalf("published state before finalizer completed: %#v", status)
			}
			return nil
		},
	)
	if attempt.EntryKey != EntryKey(TypeCodex, "codex-user.json") ||
		attempt.AuthIndex != "codex-auth" || attempt.Type != TypeCodex || attempt.PreviousStatus != StatusIdle {
		t.Fatalf("refresh attempt = %#v", attempt)
	}
	if attempt.StartedAt.IsZero() || fetchStartedAt.IsZero() || attempt.StartedAt.After(fetchStartedAt) {
		t.Fatalf("refresh timing = attempt %v fetch %v", attempt.StartedAt, fetchStartedAt)
	}
	entry := scheduler.GetStatus().Credentials[attempt.EntryKey]
	if entry == nil || entry.Status != StatusSuccess || entry.LastRefresh == nil || entry.Error != "" {
		t.Fatalf("final refresh state = %#v", entry)
	}
}

func TestSchedulerManualRefreshFinalizerFailureIsVisible(t *testing.T) {
	data := []byte(`{"rate_limit":{"allowed":true}}`)
	scheduler := NewScheduler(DefaultConfig(), func(*Entry) ([]byte, error) { return data, nil })
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")

	scheduler.setRefreshFinalizer(
		func(RefreshAttempt) error {
			return errors.New("internal recovery failure")
		},
	)
	scheduler.RefreshNow([]string{"codex-user.json"})
	entry := scheduler.GetStatus().Credentials[EntryKey(TypeCodex, "codex-user.json")]
	if entry == nil || entry.Status != StatusError || entry.Error != refreshFinalizerErrorMessage {
		t.Fatalf("finalizer failure state = %#v", entry)
	}
	if len(entry.Data) != 0 || entry.Freshness != FreshnessOutdated {
		t.Fatalf("finalizer failure published uncommitted data = %#v", entry)
	}
	if entry.FailureCount != 1 || entry.NextRefresh == nil || entry.LastRefresh == nil ||
		entry.LastErrorAt == nil || !entry.NextRefresh.After(*entry.LastRefresh) {
		t.Fatalf("finalizer failure retry state = %#v", entry)
	}
}

func TestSchedulerFetchFailureDoesNotRunFinalizer(t *testing.T) {
	scheduler := NewScheduler(
		DefaultConfig(), func(*Entry) ([]byte, error) {
			return nil, errors.New("provider unavailable")
		},
	)
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")
	called := false
	scheduler.refreshNowWithFinalizer(
		[]string{"codex-user.json"}, func(RefreshAttempt) error {
			called = true
			return nil
		},
	)
	if called {
		t.Fatal("fetch failure ran finalizer")
	}
	entry := scheduler.GetStatus().Credentials[EntryKey(TypeCodex, "codex-user.json")]
	if entry == nil || entry.Status != StatusError || entry.Error != "provider unavailable" {
		t.Fatalf("fetch failure state = %#v", entry)
	}
}

func TestSchedulerDefaultFinalizerRunsForNamedRefresh(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), func(*Entry) ([]byte, error) { return []byte(`{}`), nil })
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")
	calls := 0
	scheduler.setRefreshFinalizer(
		func(RefreshAttempt) error {
			calls++
			return nil
		},
	)

	scheduler.RefreshNow([]string{"codex-user.json"})

	if calls != 1 {
		t.Fatalf("default finalizer calls = %d, want 1", calls)
	}
}

func TestSchedulerDefaultFinalizerRunsForBulkRefresh(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), func(*Entry) ([]byte, error) { return []byte(`{}`), nil })
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")
	calls := 0
	scheduler.setRefreshFinalizer(
		func(RefreshAttempt) error {
			calls++
			return nil
		},
	)

	scheduler.RefreshNow(nil)

	if calls != 1 {
		t.Fatalf("default finalizer calls = %d, want 1", calls)
	}
}

func TestSchedulerDefaultFinalizerRunsForPeriodicRefresh(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	refreshed := make(chan struct{}, 1)
	scheduler := NewScheduler(cfg, func(*Entry) ([]byte, error) { return []byte(`{}`), nil })
	scheduler.setRefreshFinalizer(
		func(RefreshAttempt) error {
			refreshed <- struct{}{}
			return nil
		},
	)
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")
	scheduler.Start()
	t.Cleanup(scheduler.Stop)

	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("periodic refresh did not run default finalizer")
	}
}

func TestSchedulerExplicitFinalizerOverridesDefaultOnce(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), func(*Entry) ([]byte, error) { return []byte(`{}`), nil })
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")
	defaultCalls := 0
	overrideCalls := 0
	scheduler.setRefreshFinalizer(
		func(RefreshAttempt) error {
			defaultCalls++
			return nil
		},
	)

	scheduler.refreshNowWithFinalizer(
		[]string{"codex-user.json"}, func(RefreshAttempt) error {
			overrideCalls++
			return nil
		},
	)

	if defaultCalls != 0 || overrideCalls != 1 {
		t.Fatalf("finalizer calls = default %d override %d, want 0 and 1", defaultCalls, overrideCalls)
	}
}

func TestSchedulerRefreshWithoutFinalizerRemainsCompatible(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), func(*Entry) ([]byte, error) { return []byte(`{}`), nil })
	scheduler.Register("codex-user.json", TypeCodex, "codex-auth")

	scheduler.RefreshNow([]string{"codex-user.json"})

	entry := scheduler.GetStatus().Credentials[EntryKey(TypeCodex, "codex-user.json")]
	if entry == nil || entry.Status != StatusSuccess || entry.Error != "" {
		t.Fatalf("refresh state = %#v, want success", entry)
	}
}

func TestSchedulerDeleteAndReregisterAdvancesGeneration(t *testing.T) {
	calls := 0
	scheduler := NewSchedulerWithResult(
		DefaultConfig(), func(context.Context, *Entry) (FetchResult, error) {
			calls++
			return FetchResult{Data: []byte(`{}`), Completeness: FetchComplete}, nil
		},
	)
	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-1")
	key := EntryKey(TypeClaude, "claude.json")
	oldTargets, _ := scheduler.ResolveRefreshTargets([]string{key})
	if len(oldTargets) != 1 {
		t.Fatalf("old targets = %#v", oldTargets)
	}

	scheduler.Unregister("claude.json")
	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-1")
	newTargets, _ := scheduler.ResolveRefreshTargets([]string{key})
	if len(newTargets) != 1 || newTargets[0].Generation <= oldTargets[0].Generation {
		t.Fatalf("new target = %#v, old target = %#v", newTargets, oldTargets)
	}

	result := scheduler.RefreshTarget(context.Background(), oldTargets[0])
	if result.Outcome != RefreshOutcomeInconclusive || result.Error != errRefreshTargetChanged.Error() {
		t.Fatalf("old target result = %#v", result)
	}
	if calls != 0 {
		t.Fatalf("stale target provider calls = %d, want 0", calls)
	}
}

func TestSchedulerRestoredTombstonePreventsGenerationReuse(t *testing.T) {
	key := EntryKey(TypeClaude, "claude.json")
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Restore(
		map[string]EntryState{
			key: {
				FileName:           "claude.json",
				Type:               TypeClaude,
				AuthIndex:          "auth-1",
				CredentialRevision: "digest-1",
				Generation:         7,
				Tombstone:          true,
			},
		},
	)

	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-1")
	entry := scheduler.GetStatus().Credentials[key]
	if entry == nil || entry.Generation != 8 || entry.Freshness != FreshnessOutdated {
		t.Fatalf("reactivated entry = %#v", entry)
	}
}

func TestSchedulerRestoreDoesNotDowngradeCurrentGeneration(t *testing.T) {
	key := EntryKey(TypeClaude, "claude.json")
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-1")
	scheduler.Unregister("claude.json")
	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-1")
	before := scheduler.GetStatus().Credentials[key]

	scheduler.Restore(
		map[string]EntryState{
			key: {
				FileName:           "claude.json",
				Type:               TypeClaude,
				AuthIndex:          "auth-1",
				CredentialRevision: "digest-1",
				Generation:         1,
				StateRevision:      1,
				Status:             StatusSuccess,
				Freshness:          FreshnessFresh,
				Data:               []byte(`{"remaining":5}`),
			},
		},
	)

	after := scheduler.GetStatus().Credentials[key]
	if before == nil || after == nil || after.Generation != before.Generation ||
		after.StateRevision != before.StateRevision || after.Freshness != FreshnessOutdated || len(after.Data) != 0 {
		t.Fatalf("entry before=%#v after stale restore=%#v", before, after)
	}
}

func TestSchedulerCredentialRevisionChangeDropsPreviousQuota(t *testing.T) {
	scheduler := NewScheduler(
		DefaultConfig(), func(*Entry) ([]byte, error) {
			return []byte(`{"remaining":5}`), nil
		},
	)
	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-1")
	scheduler.RefreshNow([]string{"claude.json"})
	key := EntryKey(TypeClaude, "claude.json")
	before := scheduler.GetStatus().Credentials[key]

	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "digest-2")
	after := scheduler.GetStatus().Credentials[key]
	if before == nil || after == nil || after.Generation <= before.Generation {
		t.Fatalf("generation before=%#v after=%#v", before, after)
	}
	if after.Status != StatusIdle || after.Freshness != FreshnessOutdated || len(after.Data) != 0 ||
		after.LastSuccessAt != nil || after.ObservedAt != nil || after.SuccessfulEmpty {
		t.Fatalf("rotated credential retained quota state: %#v", after)
	}
}

func TestSchedulerMissingCredentialRevisionDoesNotRestoreKnownBinding(t *testing.T) {
	key := EntryKey(TypeClaude, "claude.json")
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.RegisterBinding("claude.json", TypeClaude, "auth-1", "")
	scheduler.Restore(
		map[string]EntryState{
			key: {
				FileName:           "claude.json",
				Type:               TypeClaude,
				AuthIndex:          "auth-1",
				CredentialRevision: "digest-1",
				Generation:         7,
				StateRevision:      3,
				Status:             StatusSuccess,
				Freshness:          FreshnessFresh,
				Data:               []byte(`{"remaining":5}`),
			},
		},
	)

	entry := scheduler.GetStatus().Credentials[key]
	if entry == nil || entry.Generation != 8 || entry.Freshness != FreshnessOutdated || len(entry.Data) != 0 {
		t.Fatalf("entry with unknown current revision = %#v", entry)
	}
}

func TestSchedulerFetchFailurePreservesLastKnownGood(t *testing.T) {
	calls := 0
	observedAt := time.Date(2026, 9, 9, 1, 2, 3, 0, time.UTC)
	scheduler := NewSchedulerWithResult(
		DefaultConfig(), func(context.Context, *Entry) (FetchResult, error) {
			calls++
			if calls == 1 {
				return FetchResult{
					Data:         []byte(`{"remaining":5}`),
					Source:       "claude_usage",
					ObservedAt:   observedAt,
					Completeness: FetchComplete,
				}, nil
			}
			return FetchResult{}, errors.New("provider unavailable")
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	key := EntryKey(TypeClaude, "claude.json")
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})
	first := scheduler.RefreshTarget(context.Background(), targets[0])
	if first.Outcome != RefreshOutcomeSucceeded {
		t.Fatalf("first result = %#v", first)
	}
	fresh := scheduler.GetStatus().Credentials[key]
	lastSuccessAt := cloneTime(fresh.LastSuccessAt)

	second := scheduler.RefreshTarget(context.Background(), targets[0])
	if second.Outcome != RefreshOutcomeFailed {
		t.Fatalf("second result = %#v", second)
	}
	stale := scheduler.GetStatus().Credentials[key]
	if stale.Freshness != freshnessStale || stale.Status != StatusError ||
		string(stale.Data) != `{"remaining":5}` || stale.Source != "claude_usage" ||
		stale.ObservedAt == nil || !stale.ObservedAt.Equal(observedAt) ||
		stale.LastSuccessAt == nil || lastSuccessAt == nil || !stale.LastSuccessAt.Equal(*lastSuccessAt) ||
		stale.LastError != "provider unavailable" || stale.LastErrorAt == nil || stale.RetryAt == nil ||
		stale.FetchedAt == nil || !stale.FetchedAt.After(*stale.LastSuccessAt) {
		t.Fatalf("stale quota state = %#v", stale)
	}
}

func TestSchedulerNonAuthoritativeEmptyPreservesLastKnownGood(t *testing.T) {
	calls := 0
	scheduler := NewSchedulerWithResult(
		DefaultConfig(), func(context.Context, *Entry) (FetchResult, error) {
			calls++
			if calls == 1 {
				return FetchResult{Data: []byte(`{"remaining":5}`), Completeness: FetchComplete}, nil
			}
			return FetchResult{Completeness: FetchComplete}, nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	key := EntryKey(TypeClaude, "claude.json")
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})
	_ = scheduler.RefreshTarget(context.Background(), targets[0])
	result := scheduler.RefreshTarget(context.Background(), targets[0])
	if result.Outcome != RefreshOutcomeFailed {
		t.Fatalf("empty result = %#v", result)
	}
	entry := scheduler.GetStatus().Credentials[key]
	if entry.Freshness != freshnessStale || string(entry.Data) != `{"remaining":5}` ||
		entry.SuccessfulEmpty || entry.LastError != "quota fetch returned empty data without authority" {
		t.Fatalf("non-authoritative empty state = %#v", entry)
	}
}

func TestSchedulerAuthoritativeEmptyClearsLastKnownGood(t *testing.T) {
	calls := 0
	scheduler := NewSchedulerWithResult(
		DefaultConfig(), func(context.Context, *Entry) (FetchResult, error) {
			calls++
			if calls == 1 {
				return FetchResult{Data: []byte(`{"remaining":5}`), Completeness: FetchComplete}, nil
			}
			return FetchResult{Completeness: FetchComplete, AuthoritativeEmpty: true}, nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	key := EntryKey(TypeClaude, "claude.json")
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})
	_ = scheduler.RefreshTarget(context.Background(), targets[0])
	result := scheduler.RefreshTarget(context.Background(), targets[0])
	if result.Outcome != RefreshOutcomeSucceeded {
		t.Fatalf("empty result = %#v", result)
	}
	entry := scheduler.GetStatus().Credentials[key]
	if entry.Status != StatusSuccess || entry.Freshness != FreshnessFresh ||
		!entry.SuccessfulEmpty || len(entry.Data) != 0 || entry.LastSuccessAt == nil {
		t.Fatalf("authoritative empty state = %#v", entry)
	}
}

func TestSchedulerPartialRefreshPublishesPartialSnapshot(t *testing.T) {
	scheduler := NewSchedulerWithResult(
		DefaultConfig(), func(context.Context, *Entry) (FetchResult, error) {
			return FetchResult{
				Data:         []byte(`{"usage":{"remaining":5}}`),
				Source:       "claude_usage",
				Completeness: FetchPartial,
				PartialError: "profile request failed",
			}, nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-1")
	scheduler.RefreshNow([]string{"claude.json"})
	entry := scheduler.GetStatus().Credentials[EntryKey(TypeClaude, "claude.json")]
	if entry.Status != StatusSuccess || entry.Freshness != freshnessPartial ||
		entry.Error != "" || entry.LastError != "profile request failed" || entry.LastErrorAt == nil ||
		string(entry.Data) != `{"usage":{"remaining":5}}` {
		t.Fatalf("partial quota state = %#v", entry)
	}
}

func TestSchedulerFinalizerFailureKeepsPreviousSnapshot(t *testing.T) {
	calls := 0
	scheduler := NewSchedulerWithResult(
		DefaultConfig(), func(context.Context, *Entry) (FetchResult, error) {
			calls++
			return FetchResult{
				Data:         []byte(`{"revision":` + strconv.Itoa(calls) + `}`),
				Completeness: FetchComplete,
			}, nil
		},
	)
	scheduler.Register("codex.json", TypeCodex, "auth-1")
	key := EntryKey(TypeCodex, "codex.json")
	targets, _ := scheduler.ResolveRefreshTargets([]string{key})
	_ = scheduler.RefreshTarget(context.Background(), targets[0])
	before := scheduler.GetStatus().Credentials[key]
	scheduler.setRefreshFinalizer(func(RefreshAttempt) error { return errors.New("recovery failed") })

	result := scheduler.RefreshTarget(context.Background(), targets[0])
	if result.Outcome != RefreshOutcomeFailed {
		t.Fatalf("finalizer result = %#v", result)
	}
	after := scheduler.GetStatus().Credentials[key]
	if string(after.Data) != `{"revision":1}` || after.Freshness != freshnessStale ||
		after.LastSuccessAt == nil || before.LastSuccessAt == nil || !after.LastSuccessAt.Equal(*before.LastSuccessAt) {
		t.Fatalf("finalizer failure replaced last-known-good: before=%#v after=%#v", before, after)
	}
}

func TestSummarizeEntriesByProvider(t *testing.T) {
	claudeLast := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	claudeNext := time.Date(2026, 7, 7, 11, 0, 0, 0, time.UTC)
	codexLast := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)
	codexNext := time.Date(2026, 7, 7, 10, 30, 0, 0, time.UTC)
	entries := map[string]*Entry{
		EntryKey(TypeClaude, "claude-a.json"): {
			FileName:     "claude-a.json",
			Type:         TypeClaude,
			Status:       StatusSuccess,
			LastRefresh:  &claudeLast,
			NextRefresh:  &claudeNext,
			FailureCount: 1,
		},
		EntryKey(TypeClaude, "claude-b.json"): {
			FileName:     "claude-b.json",
			Type:         TypeClaude,
			Status:       StatusBanned,
			Disabled:     true,
			FailureCount: 2,
		},
		EntryKey(TypeCodex, "codex.json"): {
			FileName:     "codex.json",
			Type:         TypeCodex,
			Status:       StatusError,
			LastRefresh:  &codexLast,
			NextRefresh:  &codexNext,
			FailureCount: 3,
		},
	}

	byProvider := summarizeEntriesByProvider(entries)
	claude := byProvider[string(TypeClaude)]
	if claude.Provider != string(TypeClaude) || claude.CredentialCount != 2 {
		t.Fatalf("claude summary identity = %+v", claude)
	}
	if claude.Success != 1 || claude.Banned != 1 || claude.Disabled != 1 || claude.FailureCount != 3 {
		t.Fatalf("claude summary counters = %+v", claude)
	}
	if claude.LatestRefreshAt == nil || !claude.LatestRefreshAt.Equal(claudeLast) {
		t.Fatalf("claude latest refresh = %v, want %v", claude.LatestRefreshAt, claudeLast)
	}
	if claude.NextRefreshAt == nil || !claude.NextRefreshAt.Equal(claudeNext) {
		t.Fatalf("claude next refresh = %v, want %v", claude.NextRefreshAt, claudeNext)
	}

	codex := byProvider[string(TypeCodex)]
	if codex.Error != 1 || codex.FailureCount != 3 {
		t.Fatalf("codex summary counters = %+v", codex)
	}
}
