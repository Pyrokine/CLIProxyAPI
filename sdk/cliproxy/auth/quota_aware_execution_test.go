package auth

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type blockingOnceQuotaSchedulingProvider struct {
	delegate QuotaSchedulingProvider
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (p *blockingOnceQuotaSchedulingProvider) Snapshots(
	provider,
	model string,
	authIndexes []string,
) map[string]QuotaSchedulingSnapshot {
	p.once.Do(
		func() {
			close(p.started)
			<-p.release
		},
	)
	return p.delegate.Snapshots(provider, model, authIndexes)
}

func quotaAwareManagerForTest(
	t *testing.T,
	provider,
	model string,
	quotaProvider QuotaSchedulingProvider,
	executor *routeTraceTestExecutor,
	auths ...*Auth,
) *Manager {
	t.Helper()
	manager := NewManager(nil, &QuotaAwareSelector{}, nil)
	manager.SetRetryConfig(1, 0, 0)
	manager.SetQuotaSchedulingProvider(quotaProvider)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(t, manager, provider, model, auths...)
	return manager
}

func quotaInFlightCounts(manager *Manager) map[string]int64 {
	if manager == nil || manager.scheduler == nil {
		return nil
	}
	manager.scheduler.mu.Lock()
	defer manager.scheduler.mu.Unlock()
	counts := make(map[string]int64, len(manager.scheduler.quotaInFlightByPool))
	maps.Copy(counts, manager.scheduler.quotaInFlightByPool)
	return counts
}

func waitForQuotaInFlightCounts(t *testing.T, manager *Manager, want int) map[string]int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		counts := quotaInFlightCounts(manager)
		total := int64(0)
		for _, count := range counts {
			total += count
		}
		if total == int64(want) {
			return counts
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota in-flight counts = %#v, want total %d", counts, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerQuotaAwareDecisionIncludesQuotaSignals(t *testing.T) {
	const (
		provider = "quota-decision-provider"
		model    = "quota-decision-model"
	)
	resetAt := time.Now().Add(time.Hour)
	quotaProvider := staticQuotaSchedulingProvider{
		byProvider: map[string]map[string]QuotaSchedulingSnapshot{
			provider: {
				"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.3},
				"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.8, ResetAt: &resetAt},
			},
		},
	}
	manager := quotaAwareManagerForTest(
		t,
		provider,
		model,
		quotaProvider,
		&routeTraceTestExecutor{provider: provider},
		quotaAwareTestAuth("a", "a-index", provider, 0, 1),
		quotaAwareTestAuth("b", "b-index", provider, 0, 1),
	)
	recorder := &routeTraceTestRecorder{}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if string(response.Payload) != "b" {
		t.Fatalf("Execute() payload = %q, want b", response.Payload)
	}

	event := findRouteTraceEvent(t, recorder.Events(), "route.decision", 1)
	if event.Decision == nil || event.Decision.Strategy != "quota-aware" || event.Decision.Selected == nil {
		t.Fatalf("quota-aware decision = %+v", event.Decision)
	}
	selectedAuth, ok := manager.GetByID("b")
	if !ok || selectedAuth == nil {
		t.Fatal("selected auth b is missing")
	}
	selectedIndex := routeTraceAuthIndex(selectedAuth)
	if event.Decision.Selected.AuthIndex != selectedIndex ||
		event.Decision.Selected.MatchedRule != "quota_aware" {
		t.Fatalf("quota-aware selection = %+v", event.Decision.Selected)
	}
	candidates := make(map[string]cliproxyexecutor.RouteDecisionCandidate, len(event.Decision.Candidates))
	for _, candidate := range event.Decision.Candidates {
		candidates[candidate.AuthIndex] = candidate
	}
	selected := candidates[selectedIndex]
	if !selected.Selected || selected.QuotaStatus != "fresh" || selected.QuotaHeadroom == nil ||
		*selected.QuotaHeadroom != 0.8 || selected.QuotaResetAfterMS == nil || *selected.QuotaResetAfterMS <= 0 ||
		selected.InFlight == nil || *selected.InFlight != 0 ||
		!containsRouteDecisionValue(selected.MatchedRules, "quota_available_score") {
		t.Fatalf("selected quota-aware candidate = %+v", selected)
	}
}

func TestManagerQuotaAwareLeaseCoversNonStreamExecution(t *testing.T) {
	const (
		provider = "quota-lease-provider"
		model    = "quota-lease-model"
	)
	started := make(chan struct{})
	release := make(chan struct{})
	executor := &routeTraceTestExecutor{
		provider: provider,
		executeWithContextFn: func(_ context.Context, auth *Auth) (cliproxyexecutor.Response, error) {
			close(started)
			<-release
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	}
	manager := quotaAwareManagerForTest(
		t,
		provider,
		model,
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				provider: {
					"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 1},
				},
			},
		},
		executor,
		quotaAwareTestAuth("a", "a-index", provider, 0, 1),
	)
	result := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(
			context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model},
			cliproxyexecutor.Options{},
		)
		result <- errExecute
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("executor did not start")
	}
	counts := waitForQuotaInFlightCounts(t, manager, 1)
	if counts[provider+"\x00a-index"] != 1 {
		t.Fatalf("running quota in-flight counts = %#v", counts)
	}
	close(release)
	if errExecute := <-result; errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	waitForQuotaInFlightCounts(t, manager, 0)
}

func TestManagerQuotaAwareReleasesStaleSelectionLeaseBeforeRetry(t *testing.T) {
	const (
		provider = "quota-stale-provider"
		model    = "quota-stale-model"
	)
	delegate := staticQuotaSchedulingProvider{
		byProvider: map[string]map[string]QuotaSchedulingSnapshot{
			provider: {
				"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.9},
				"b-index": {Status: QuotaSchedulingStatusFresh, Headroom: 0.8},
			},
		},
	}
	quotaProvider := &blockingOnceQuotaSchedulingProvider{
		delegate: delegate,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	executeStarted := make(chan struct{})
	executeRelease := make(chan struct{})
	executor := &routeTraceTestExecutor{
		provider: provider,
		executeWithContextFn: func(_ context.Context, auth *Auth) (cliproxyexecutor.Response, error) {
			if auth.ID != "b" {
				return cliproxyexecutor.Response{}, errors.New("stale auth reached executor")
			}
			close(executeStarted)
			<-executeRelease
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	}
	manager := quotaAwareManagerForTest(
		t,
		provider,
		model,
		quotaProvider,
		executor,
		quotaAwareTestAuth("a", "a-index", provider, 0, 1),
		quotaAwareTestAuth("b", "b-index", provider, 0, 1),
	)
	result := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(
			context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model},
			cliproxyexecutor.Options{},
		)
		result <- errExecute
	}()

	select {
	case <-quotaProvider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota provider did not start")
	}
	manager.mu.Lock()
	stale := manager.auths["a"].Clone()
	stale.Disabled = true
	manager.auths["a"] = stale
	manager.mu.Unlock()
	close(quotaProvider.release)

	select {
	case <-executeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement executor did not start")
	}
	counts := waitForQuotaInFlightCounts(t, manager, 1)
	if counts[provider+"\x00a-index"] != 0 || counts[provider+"\x00b-index"] != 1 {
		t.Fatalf("retry quota in-flight counts = %#v", counts)
	}
	close(executeRelease)
	if errExecute := <-result; errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	waitForQuotaInFlightCounts(t, manager, 0)
}

func TestManagerQuotaAwareReleasesLeaseWhenSelectionBecomesUnavailable(t *testing.T) {
	const (
		provider = "quota-unavailable-provider"
		model    = "quota-unavailable-model"
	)
	delegate := staticQuotaSchedulingProvider{
		byProvider: map[string]map[string]QuotaSchedulingSnapshot{
			provider: {
				"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 1},
			},
		},
	}
	quotaProvider := &blockingOnceQuotaSchedulingProvider{
		delegate: delegate,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	manager := quotaAwareManagerForTest(
		t,
		provider,
		model,
		quotaProvider,
		&routeTraceTestExecutor{provider: provider},
		quotaAwareTestAuth("a", "a-index", provider, 0, 1),
	)
	result := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(
			context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model},
			cliproxyexecutor.Options{},
		)
		result <- errExecute
	}()

	select {
	case <-quotaProvider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota provider did not start")
	}
	manager.mu.Lock()
	delete(manager.auths, "a")
	manager.mu.Unlock()
	close(quotaProvider.release)
	if errExecute := <-result; errExecute == nil {
		t.Fatal("Execute() error = nil, want unavailable selection")
	}
	waitForQuotaInFlightCounts(t, manager, 0)
}

func TestManagerQuotaAwareLeaseTransfersToStreamLifecycle(t *testing.T) {
	const (
		provider = "quota-stream-provider"
		model    = "quota-stream-model"
	)
	upstream := make(chan cliproxyexecutor.StreamChunk, 1)
	upstream <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
	executor := &routeTraceTestExecutor{
		provider: provider,
		streamFn: func(*Auth) (*cliproxyexecutor.StreamResult, error) {
			return &cliproxyexecutor.StreamResult{Chunks: upstream}, nil
		},
	}
	manager := quotaAwareManagerForTest(
		t,
		provider,
		model,
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				provider: {
					"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 1},
				},
			},
		},
		executor,
		quotaAwareTestAuth("a", "a-index", provider, 0, 1),
	)

	stream, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Stream: true},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	waitForQuotaInFlightCounts(t, manager, 1)
	first, ok := <-stream.Chunks
	if !ok || string(first.Payload) != "first" {
		t.Fatalf("first stream chunk = (%+v, %v)", first, ok)
	}
	if counts := quotaInFlightCounts(manager); counts[provider+"\x00a-index"] != 1 {
		t.Fatalf("open stream quota in-flight counts = %#v", counts)
	}
	close(upstream)
	for range stream.Chunks {
	}
	waitForQuotaInFlightCounts(t, manager, 0)
}

func TestManagerQuotaAwareReleasesLeaseOnStreamBootstrapFailure(t *testing.T) {
	const (
		provider = "quota-stream-error-provider"
		model    = "quota-stream-error-model"
	)
	upstream := make(chan cliproxyexecutor.StreamChunk, 1)
	upstream <- cliproxyexecutor.StreamChunk{Err: errors.New("bootstrap failed")}
	close(upstream)
	executor := &routeTraceTestExecutor{
		provider: provider,
		streamFn: func(*Auth) (*cliproxyexecutor.StreamResult, error) {
			return &cliproxyexecutor.StreamResult{Chunks: upstream}, nil
		},
	}
	manager := quotaAwareManagerForTest(
		t,
		provider,
		model,
		staticQuotaSchedulingProvider{
			byProvider: map[string]map[string]QuotaSchedulingSnapshot{
				provider: {
					"a-index": {Status: QuotaSchedulingStatusFresh, Headroom: 1},
				},
			},
		},
		executor,
		quotaAwareTestAuth("a", "a-index", provider, 0, 1),
	)

	stream, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Stream: true},
	)
	if errExecute != nil || stream == nil {
		t.Fatalf("ExecuteStream() = (%#v, %v), want error stream", stream, errExecute)
	}
	chunk, ok := <-stream.Chunks
	if !ok || chunk.Err == nil || chunk.Err.Error() != "bootstrap failed" {
		t.Fatalf("bootstrap error chunk = (%+v, %v)", chunk, ok)
	}
	waitForQuotaInFlightCounts(t, manager, 0)
}
