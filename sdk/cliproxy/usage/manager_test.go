package usage

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
)

type capturedUsageRecord struct {
	ctx    context.Context
	record Record
}

type deferredCapturePlugin struct {
	records chan capturedUsageRecord
}

type usagePluginFunc func(context.Context, Record)

type registerNamedCompatibility interface {
	RegisterNamed(string, Plugin)
}

var _ registerNamedCompatibility = (*Manager)(nil)
var _ = RegisterNamedPlugin

func (f usagePluginFunc) HandleUsage(ctx context.Context, record Record) {
	f(ctx, record)
}

func (p *deferredCapturePlugin) HandleUsage(ctx context.Context, record Record) {
	select {
	case p.records <- capturedUsageRecord{ctx: ctx, record: record}:
	default:
	}
}

type deferredStatusError struct {
	status int
	msg    string
}

func (e *deferredStatusError) Error() string   { return e.msg }
func (e *deferredStatusError) StatusCode() int { return e.status }

func waitForDeferredRecord(t *testing.T, records <-chan capturedUsageRecord, model string) capturedUsageRecord {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case got := <-records:
			if got.record.Model == model {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for deferred usage record for %q", model)
		}
	}
}

func registerTestNamedPlugin(t *testing.T, name string, plugin Plugin) {
	t.Helper()
	RegisterNamedPlugin(name, plugin)
	t.Cleanup(
		func() {
			UnregisterNamedPlugin(name).Wait()
		},
	)
}

func assertNoDeferredRecord(t *testing.T, records <-chan capturedUsageRecord, model string) {
	t.Helper()
	deadline := time.After(50 * time.Millisecond)
	for {
		select {
		case got := <-records:
			if got.record.Model == model {
				t.Fatalf("received duplicate deferred usage record: %#v", got.record)
			}
		case <-deadline:
			return
		}
	}
}

func TestDeferredRecordPreservesContextAfterCancellation(t *testing.T) {
	plugin := &deferredCapturePlugin{records: make(chan capturedUsageRecord, 64)}
	registerTestNamedPlugin(t, "test-deferred-context", plugin)

	type contextKey struct{}
	baseCtx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "preserved"))
	deferred := NewDeferredRecord()
	ctx := WithDeferredRecord(baseCtx, deferred)
	headers := http.Header{"X-Upstream-Request-Id": {"deferred-1"}}
	if !DeferRecord(ctx, Record{
		Model: "deferred-context-model", Completed: true, ResponseHeaders: headers,
	}) {
		t.Fatal("DeferRecord rejected the first record")
	}
	headers.Set("X-Upstream-Request-Id", "mutated")
	cancel()
	deferred.Complete(ctx, nil)
	deferred.Complete(ctx, errors.New("late failure"))

	got := waitForDeferredRecord(t, plugin.records, "deferred-context-model")
	if got.ctx == nil || got.ctx.Value(contextKey{}) != "preserved" {
		t.Fatalf("published context value = %v, want preserved", got.ctx)
	}
	if got.ctx.Err() != nil {
		t.Fatalf("published context error = %v, want nil", got.ctx.Err())
	}
	if got.record.Failed || !got.record.Completed {
		t.Fatalf("record outcome = %#v, want completed success", got.record)
	}
	if got.record.ResponseHeaders.Get("X-Upstream-Request-Id") != "deferred-1" {
		t.Fatalf("deferred response headers = %#v", got.record.ResponseHeaders)
	}
	assertNoDeferredRecord(t, plugin.records, "deferred-context-model")
}

func TestDeferredRecordCompleteBeforeStorePublishesFailure(t *testing.T) {
	plugin := &deferredCapturePlugin{records: make(chan capturedUsageRecord, 64)}
	registerTestNamedPlugin(t, "test-deferred-complete-first", plugin)

	deferred := NewDeferredRecord()
	failure := &deferredStatusError{status: 429, msg: "rate limited"}
	deferred.Complete(context.Background(), failure)
	ctx := WithDeferredRecord(context.Background(), deferred)
	if !DeferRecord(ctx, Record{Model: "deferred-complete-first-model"}) {
		t.Fatal("DeferRecord rejected the first record")
	}

	got := waitForDeferredRecord(t, plugin.records, "deferred-complete-first-model")
	if !got.record.Failed || got.record.Completed {
		t.Fatalf("record outcome = %#v, want failure", got.record)
	}
	if got.record.Fail.StatusCode != 429 || got.record.Fail.Body != "rate limited" {
		t.Fatalf("failure metadata = %#v", got.record.Fail)
	}
	assertNoDeferredRecord(t, plugin.records, "deferred-complete-first-model")
}

func TestDeferredRecordWithOutcomeMapsContextStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "canceled", err: context.Canceled, want: clienterror.StatusClientClosedRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "url error wraps canceled",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.Canceled},
			want: clienterror.StatusClientClosedRequest,
		},
		{name: "plain error", err: errors.New("boom"), want: 0},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				got := deferredRecordWithOutcome(Record{Completed: true}, test.err)
				if !got.Failed || got.Completed {
					t.Fatalf("record outcome = %#v, want failed and incomplete", got)
				}
				if got.Fail.StatusCode != test.want {
					t.Fatalf("StatusCode = %d, want %d", got.Fail.StatusCode, test.want)
				}
				if got.Fail.Body == "" {
					t.Fatal("expected failure body")
				}
			},
		)
	}
}

func TestDeferredRecordWithOutcomePreservesFailureMetadata(t *testing.T) {
	want := Failure{StatusCode: http.StatusTeapot, Body: "existing failure"}
	got := deferredRecordWithOutcome(Record{Completed: true, Fail: want}, context.Canceled)
	if got.Fail != want {
		t.Fatalf("failure metadata = %#v, want %#v", got.Fail, want)
	}
}

func TestDeferredRecordConcurrentStoreAndCompletePublishesExactlyOnce(t *testing.T) {
	plugin := &deferredCapturePlugin{records: make(chan capturedUsageRecord, 128)}
	registerTestNamedPlugin(t, "test-deferred-concurrent", plugin)

	deferred := NewDeferredRecord()
	ctx := WithDeferredRecord(context.Background(), deferred)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if DeferRecord(ctx, Record{Model: "deferred-concurrent-model"}) {
				accepted.Add(1)
			}
		}()
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				deferred.Complete(ctx, nil)
				return
			}
			deferred.Complete(ctx, errors.New("concurrent failure"))
		}(i)
	}
	wg.Wait()

	_ = waitForDeferredRecord(t, plugin.records, "deferred-concurrent-model")
	if accepted.Load() != 1 {
		t.Fatalf("accepted records = %d, want 1", accepted.Load())
	}
	if DeferRecord(ctx, Record{Model: "deferred-concurrent-model"}) {
		t.Fatal("DeferRecord accepted a record after publication")
	}
	assertNoDeferredRecord(t, plugin.records, "deferred-concurrent-model")
}

func TestWithoutDeferredRecordIsolatesNestedExecution(t *testing.T) {
	deferred := NewDeferredRecord()
	parentCtx := WithDeferredRecord(context.Background(), deferred)
	nestedCtx := WithoutDeferredRecord(parentCtx)

	if DeferRecord(nestedCtx, Record{Model: "nested"}) {
		t.Fatal("nested record claimed the parent deferred holder")
	}
	if !DeferRecord(parentCtx, Record{Model: "parent"}) {
		t.Fatal("parent record did not claim its deferred holder")
	}
}

func TestManagerDeliverySnapshotsRecordAndDetachesRequestCancellation(t *testing.T) {
	manager := NewManager(2)
	defer manager.Stop()

	blockEntered := make(chan struct{})
	releaseBlock := make(chan struct{})
	var releaseBlockOnce sync.Once
	releaseBlockedDelivery := func() {
		releaseBlockOnce.Do(func() { close(releaseBlock) })
	}
	defer releaseBlockedDelivery()
	captured := make(chan capturedUsageRecord, 1)
	manager.Register(usagePluginFunc(func(ctx context.Context, record Record) {
		switch record.Model {
		case "block":
			close(blockEntered)
			<-releaseBlock
		case "target":
			captured <- capturedUsageRecord{ctx: ctx, record: record}
		}
	}))
	if errPublish := manager.Publish(context.Background(), Record{Model: "block"}); errPublish != nil {
		t.Fatalf("Publish(block) error = %v", errPublish)
	}
	select {
	case <-blockEntered:
	case <-time.After(time.Second):
		t.Fatal("blocking delivery did not start")
	}

	type contextKey struct{}
	requestCtx, cancelRequest := context.WithCancel(
		context.WithValue(context.Background(), contextKey{}, "preserved"),
	)
	requestCtx = internallogging.WithClientRequestMetadata(requestCtx, internallogging.ClientRequestMetadata{
		SessionID:       "slot:child",
		ParentSessionID: "slot:parent",
	})
	requestCtx = WithStream(requestCtx, true)
	generate := false
	budget := 7
	headers := http.Header{"X-Upstream-Request-Id": {"request-1"}}
	record := Record{
		BaseURL:         " https://upstream.example/v1 ",
		Model:           "target",
		SessionID:       "slot:child",
		Generate:        &generate,
		Thinking:        &Thinking{Mode: "budget", Budget: &budget},
		ResponseHeaders: headers,
	}
	if errPublish := manager.Publish(requestCtx, record); errPublish != nil {
		t.Fatalf("Publish(target) error = %v", errPublish)
	}
	headers.Set("X-Upstream-Request-Id", "mutated")
	generate = true
	budget = 99
	cancelRequest()
	releaseBlockedDelivery()

	select {
	case got := <-captured:
		if got.ctx.Value(contextKey{}) != "preserved" || got.ctx.Err() != nil {
			t.Fatalf("delivery context value=%v error=%v", got.ctx.Value(contextKey{}), got.ctx.Err())
		}
		if got.record.BaseURL != "https://upstream.example/v1" || !got.record.Stream {
			t.Fatalf("delivery target metadata = baseURL:%q stream:%t", got.record.BaseURL, got.record.Stream)
		}
		if got.record.SessionID != coresession.NormalizeToCanonicalUUID("slot:child") ||
			got.record.ParentSessionID != coresession.NormalizeToCanonicalUUID("slot:parent") {
			t.Fatalf("delivery session hierarchy = (%q, %q)", got.record.SessionID, got.record.ParentSessionID)
		}
		if got.record.ResponseHeaders.Get("X-Upstream-Request-Id") != "request-1" {
			t.Fatalf("delivery headers = %#v", got.record.ResponseHeaders)
		}
		if got.record.Generate == nil || *got.record.Generate || got.record.Thinking == nil ||
			got.record.Thinking.Budget == nil || *got.record.Thinking.Budget != 7 {
			t.Fatalf("delivery pointer fields changed: %#v", got.record)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for detached usage delivery")
	}
}

func TestPrepareRecordForDeliverySessionFallbackIsSameOrigin(t *testing.T) {
	ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
		SessionID:       "session:current",
		ParentSessionID: "session:parent",
	})
	tests := []struct {
		name        string
		record      Record
		wantSession string
		wantParent  string
	}{
		{
			name:        "context fallback",
			wantSession: "session:current",
			wantParent:  "session:parent",
		},
		{
			name:        "same origin inherits parent",
			record:      Record{SessionID: "session:current"},
			wantSession: "session:current",
			wantParent:  "session:parent",
		},
		{
			name:        "independent session rejects stale parent",
			record:      Record{SessionID: "session:independent"},
			wantSession: "session:independent",
		},
		{
			name:        "self parent is removed",
			record:      Record{SessionID: "session:self", ParentSessionID: "session:self"},
			wantSession: "session:self",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := prepareRecordForDelivery(ctx, test.record)
			if got.SessionID != coresession.NormalizeToCanonicalUUID(test.wantSession) ||
				got.ParentSessionID != coresession.NormalizeToCanonicalUUID(test.wantParent) {
				t.Fatalf("session hierarchy = (%q, %q)", got.SessionID, got.ParentSessionID)
			}
		})
	}
}

func TestManagerIsolatesMutableHeadersBetweenPlugins(t *testing.T) {
	manager := NewManager(1)
	defer manager.Stop()

	captured := make(chan http.Header, 1)
	manager.Register(usagePluginFunc(func(_ context.Context, record Record) {
		record.ResponseHeaders.Set("X-Plugin", "mutated")
	}))
	manager.Register(usagePluginFunc(func(_ context.Context, record Record) {
		captured <- record.ResponseHeaders
	}))
	if errPublish := manager.Publish(context.Background(), Record{
		Model:           "isolated",
		ResponseHeaders: http.Header{"X-Plugin": {"original"}},
	}); errPublish != nil {
		t.Fatalf("Publish(isolated) error = %v", errPublish)
	}

	select {
	case headers := <-captured:
		if headers.Get("X-Plugin") != "original" {
			t.Fatalf("second plugin headers = %#v", headers)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second plugin")
	}
}

func TestPublishUsesRegistrationSnapshot(t *testing.T) {
	manager := NewManager(0)
	defer manager.Stop()

	entered := make(chan struct{})
	release := make(chan struct{})
	manager.RegisterNamed(
		"blocker", usagePluginFunc(
			func(_ context.Context, record Record) {
				if record.Model != "block" {
					return
				}
				close(entered)
				<-release
			},
		),
	)
	if errPublish := manager.Publish(context.Background(), Record{Model: "block"}); errPublish != nil {
		t.Fatalf("Publish(block) error = %v", errPublish)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter blocking plugin")
	}

	oldRecords := make(chan Record, 1)
	newRecords := make(chan Record, 1)
	manager.RegisterNamed(
		"test", usagePluginFunc(
			func(_ context.Context, record Record) {
				oldRecords <- record
			},
		),
	)
	if errPublish := manager.Publish(context.Background(), Record{Model: "stale"}); errPublish != nil {
		t.Fatalf("Publish(stale) error = %v", errPublish)
	}
	manager.UnregisterNamed("test")
	manager.RegisterNamed(
		"test", usagePluginFunc(
			func(_ context.Context, record Record) {
				newRecords <- record
			},
		),
	)
	close(release)

	select {
	case got := <-oldRecords:
		if got.Model != "stale" {
			t.Fatalf("old plugin record = %q, want stale", got.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("old plugin did not receive the queued record")
	}
	select {
	case got := <-newRecords:
		t.Fatalf("new plugin received record published before registration: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRegisterNamedRetirementWaitsForCapturedGeneration(t *testing.T) {
	manager := NewManager(0)
	defer manager.Stop()

	entered := make(chan struct{})
	release := make(chan struct{})
	oldRecords := make(chan Record, 1)
	newRecords := make(chan Record, 1)
	manager.RegisterNamed(
		"target", usagePluginFunc(
			func(_ context.Context, record Record) {
				oldRecords <- record
				close(entered)
				<-release
			},
		),
	)
	if errPublish := manager.Publish(context.Background(), Record{Model: "old"}); errPublish != nil {
		t.Fatalf("Publish(old) error = %v", errPublish)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old generation did not receive the record")
	}

	retirement := manager.RegisterNamedWithRetirement(
		"target", usagePluginFunc(
			func(_ context.Context, record Record) {
				newRecords <- record
			},
		),
	)
	retired := make(chan struct{})
	go func() {
		retirement.Wait()
		close(retired)
	}()
	select {
	case <-retired:
		t.Fatal("replacement retirement completed before the captured callback returned")
	case <-time.After(50 * time.Millisecond):
	}

	if errPublish := manager.Publish(context.Background(), Record{Model: "new"}); errPublish != nil {
		t.Fatalf("Publish(new) error = %v", errPublish)
	}
	close(release)
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("replacement retirement did not complete after the callback returned")
	}
	select {
	case got := <-oldRecords:
		if got.Model != "old" {
			t.Fatalf("old generation record = %q, want old", got.Model)
		}
	default:
		t.Fatal("old generation record was not captured")
	}
	select {
	case got := <-newRecords:
		if got.Model != "new" {
			t.Fatalf("new generation record = %q, want new", got.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("new generation did not receive the post-replacement record")
	}
}

func TestUnregisterNamedRetirementWaitsForCapturedGeneration(t *testing.T) {
	manager := NewManager(0)
	defer manager.Stop()

	entered := make(chan struct{})
	release := make(chan struct{})
	calls := make(chan Record, 2)
	manager.RegisterNamed(
		"target", usagePluginFunc(
			func(_ context.Context, record Record) {
				calls <- record
				close(entered)
				<-release
			},
		),
	)
	if errPublish := manager.Publish(context.Background(), Record{Model: "captured"}); errPublish != nil {
		t.Fatalf("Publish(captured) error = %v", errPublish)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("registered generation did not receive the record")
	}

	retirement := manager.UnregisterNamed("target")
	retired := make(chan struct{})
	go func() {
		retirement.Wait()
		close(retired)
	}()
	select {
	case <-retired:
		t.Fatal("unregister retirement completed before the captured callback returned")
	case <-time.After(50 * time.Millisecond):
	}

	if errPublish := manager.Publish(context.Background(), Record{Model: "detached"}); errPublish != nil {
		t.Fatalf("Publish(detached) error = %v", errPublish)
	}
	close(release)
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("unregister retirement did not complete after the callback returned")
	}
	select {
	case got := <-calls:
		if got.Model != "captured" {
			t.Fatalf("registered generation record = %q, want captured", got.Model)
		}
	default:
		t.Fatal("registered generation record was not captured")
	}
	select {
	case got := <-calls:
		t.Fatalf("detached generation received a new record: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStopContextReturnsWhileUsageCallbackStalls(t *testing.T) {
	manager := NewManager(0)
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.Register(
		usagePluginFunc(
			func(context.Context, Record) {
				close(entered)
				<-release
			},
		),
	)
	if errPublish := manager.Publish(context.Background(), Record{Model: "blocked"}); errPublish != nil {
		t.Fatalf("Publish(blocked) error = %v", errPublish)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("usage callback did not start")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	errStop := manager.StopContext(stopCtx)
	cancelStop()
	if !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("StopContext() error = %v, want deadline exceeded", errStop)
	}

	close(release)
	if errStop = manager.StopContext(context.Background()); errStop != nil {
		t.Fatalf("StopContext() after callback release error = %v", errStop)
	}
}

func TestPublishAfterStopReleasesCapturedGeneration(t *testing.T) {
	manager := NewManager(0)
	manager.RegisterNamed("target", usagePluginFunc(func(context.Context, Record) {}))
	manager.Stop()

	if errPublish := manager.Publish(context.Background(), Record{Model: "closed"}); !errors.Is(
		errPublish,
		ErrManagerStopped,
	) {
		t.Fatalf("Publish(closed) error = %v, want %v", errPublish, ErrManagerStopped)
	}
	retirement := manager.UnregisterNamed("target")
	retired := make(chan struct{})
	go func() {
		retirement.Wait()
		close(retired)
	}()
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("closed manager retained the rejected publication lease")
	}
	manager.Stop()
}

func TestManagerQueueCapacityAppliesBackpressure(t *testing.T) {
	manager := NewManager(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.Register(
		usagePluginFunc(
			func(context.Context, Record) {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
			},
		),
	)
	if err := manager.Publish(context.Background(), Record{Model: "active"}); err != nil {
		t.Fatalf("Publish(active) error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("active callback did not start")
	}
	if err := manager.Publish(context.Background(), Record{Model: "queued"}); err != nil {
		t.Fatalf("Publish(queued) error = %v", err)
	}

	publishCtx, cancelPublish := context.WithTimeout(context.Background(), 30*time.Millisecond)
	errPublish := manager.Publish(publishCtx, Record{Model: "blocked"})
	cancelPublish()
	if !errors.Is(errPublish, context.DeadlineExceeded) {
		t.Fatalf("Publish(blocked) error = %v, want deadline exceeded", errPublish)
	}
	manager.mu.Lock()
	queued := len(manager.queue)
	manager.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queued records = %d, want capacity 1", queued)
	}
	close(release)
	manager.Stop()
}

func TestManagerCanRestartAfterStop(t *testing.T) {
	manager := NewManager(2)
	records := make(chan string, 2)
	manager.Register(
		usagePluginFunc(
			func(_ context.Context, record Record) {
				records <- record.Model
			},
		),
	)
	manager.Start(context.Background())
	if err := manager.Publish(context.Background(), Record{Model: "first"}); err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}
	manager.Stop()
	manager.Start(context.Background())
	if err := manager.Publish(context.Background(), Record{Model: "second"}); err != nil {
		t.Fatalf("Publish(second) error = %v", err)
	}
	manager.Stop()

	for _, want := range []string{"first", "second"} {
		select {
		case got := <-records:
			if got != want {
				t.Fatalf("record = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestPublishRecordUsesContextManager(t *testing.T) {
	manager := NewManager(1)
	records := make(chan Record, 1)
	manager.Register(
		usagePluginFunc(
			func(_ context.Context, record Record) {
				records <- record
			},
		),
	)
	ctx := WithManager(context.Background(), manager)
	if err := PublishRecord(ctx, Record{Model: "instance"}); err != nil {
		t.Fatalf("PublishRecord() error = %v", err)
	}
	manager.Stop()
	select {
	case got := <-records:
		if got.Model != "instance" {
			t.Fatalf("record model = %q, want instance", got.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("context manager did not receive record")
	}
}
