package quota

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSchedulerProbeTargetDoesNotPublishQuotaStateOrRunFinalizer(t *testing.T) {
	finalizerCalls := 0
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(_ context.Context, _ *Entry) ([]byte, error) {
			return []byte(`{"quota":"ok"}`), nil
		},
	)
	scheduler.SetRefreshContextFinalizer(
		func(_ context.Context, _ RefreshAttempt) error {
			finalizerCalls++
			return nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-v1")

	key := EntryKey(TypeClaude, "claude.json")
	entry := scheduler.entries[key]
	lastRefresh := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	nextRefresh := lastRefresh.Add(time.Hour)
	entry.mu.Lock()
	entry.Status = StatusBanned
	entry.LastRefresh = &lastRefresh
	entry.NextRefresh = &nextRefresh
	entry.Error = "existing quota state"
	entry.FailureCount = 3
	entry.Data = []byte(`{"cached":true}`)
	entry.mu.Unlock()
	before := entry.snapshot()

	targets, unresolved := scheduler.ResolveRefreshTargets([]string{key})
	if len(unresolved) != 0 || len(targets) != 1 {
		t.Fatalf("ResolveRefreshTargets() targets=%+v unresolved=%v", targets, unresolved)
	}
	result := scheduler.ProbeTarget(context.Background(), targets[0])
	if result.Outcome != ProbeOutcomePassed || string(result.Data) != `{"quota":"ok"}` {
		t.Fatalf("ProbeTarget() = %+v", result)
	}
	if finalizerCalls != 0 {
		t.Fatalf("refresh finalizer calls = %d, want 0", finalizerCalls)
	}
	if after := entry.snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("quota state changed by probe\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestSchedulerProbeTargetDoesNotInterfereWithConcurrentRefresh(t *testing.T) {
	refreshEntered := make(chan struct{})
	probeEntered := make(chan struct{})
	releaseRefresh := make(chan struct{})
	releaseProbe := make(chan struct{})
	finalizerCalls := 0
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(_ context.Context, entry *Entry) ([]byte, error) {
			switch entry.Status {
			case StatusIdle:
				close(refreshEntered)
				<-releaseRefresh
				return []byte(`{"source":"refresh"}`), nil
			case statusLoading:
				close(probeEntered)
				<-releaseProbe
				return []byte(`{"source":"probe"}`), nil
			default:
				t.Fatalf("fetch entry status = %s", entry.Status)
				return nil, nil
			}
		},
	)
	scheduler.SetRefreshContextFinalizer(
		func(_ context.Context, _ RefreshAttempt) error {
			finalizerCalls++
			return nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-v1")
	targets, unresolved := scheduler.ResolveRefreshTargets([]string{"claude.json"})
	if len(unresolved) != 0 || len(targets) != 1 {
		t.Fatalf("ResolveRefreshTargets() targets=%+v unresolved=%v", targets, unresolved)
	}
	target := targets[0]

	refreshResultCh := make(chan RefreshResult, 1)
	go func() {
		refreshResultCh <- scheduler.RefreshTarget(context.Background(), target)
	}()
	<-refreshEntered

	probeResultCh := make(chan ProbeResult, 1)
	go func() {
		probeResultCh <- scheduler.ProbeTarget(context.Background(), target)
	}()
	<-probeEntered
	close(releaseProbe)

	ProbeResult := <-probeResultCh
	if ProbeResult.Outcome != ProbeOutcomePassed || string(ProbeResult.Data) != `{"source":"probe"}` {
		t.Fatalf("ProbeTarget() = %+v", ProbeResult)
	}
	entry := scheduler.GetStatus().Credentials[target.EntryKey]
	if entry.Status != statusLoading || entry.LastRefresh != nil || entry.FailureCount != 0 || len(entry.Data) != 0 {
		t.Fatalf("probe changed concurrent refresh state: %+v", entry)
	}
	if finalizerCalls != 0 {
		t.Fatalf("refresh finalizer calls before refresh completion = %d, want 0", finalizerCalls)
	}

	close(releaseRefresh)
	refreshResult := <-refreshResultCh
	if refreshResult.Outcome != RefreshOutcomeSucceeded {
		t.Fatalf("RefreshTarget() = %+v", refreshResult)
	}
	entry = scheduler.GetStatus().Credentials[target.EntryKey]
	if entry.Status != StatusSuccess || string(entry.Data) != `{"source":"refresh"}` {
		t.Fatalf("refresh result was overwritten by probe: %+v", entry)
	}
	if finalizerCalls != 1 {
		t.Fatalf("refresh finalizer calls = %d, want 1", finalizerCalls)
	}
}

func TestSchedulerProbeTargetRejectsChangedBinding(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(_ context.Context, _ *Entry) ([]byte, error) {
			close(entered)
			<-release
			return []byte(`{"quota":"ok"}`), nil
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-v1")
	targets, _ := scheduler.ResolveRefreshTargets([]string{"claude.json"})

	resultCh := make(chan ProbeResult, 1)
	go func() {
		resultCh <- scheduler.ProbeTarget(context.Background(), targets[0])
	}()
	<-entered
	scheduler.updateAuthIndex("claude.json", TypeClaude, "auth-v2")
	close(release)

	result := <-resultCh
	if result.Outcome != ProbeOutcomeOutdated {
		t.Fatalf("ProbeTarget() outcome = %s, want %s", result.Outcome, ProbeOutcomeOutdated)
	}
}

func TestSchedulerProbeTargetCancellationIsInconclusive(t *testing.T) {
	entered := make(chan struct{})
	scheduler := NewSchedulerWithContext(
		DefaultConfig(), func(ctx context.Context, _ *Entry) ([]byte, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	scheduler.Register("claude.json", TypeClaude, "auth-v1")
	targets, _ := scheduler.ResolveRefreshTargets([]string{"claude.json"})
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan ProbeResult, 1)
	go func() {
		resultCh <- scheduler.ProbeTarget(ctx, targets[0])
	}()
	<-entered
	cancel()

	result := <-resultCh
	if result.Outcome != ProbeOutcomeInconclusive {
		t.Fatalf("ProbeTarget() outcome = %s, want %s", result.Outcome, ProbeOutcomeInconclusive)
	}
	status := scheduler.GetStatus().Credentials[EntryKey(TypeClaude, "claude.json")]
	if status.Status != StatusIdle || status.LastRefresh != nil || status.FailureCount != 0 {
		t.Fatalf("quota state changed after cancelled probe: %+v", status)
	}
}
