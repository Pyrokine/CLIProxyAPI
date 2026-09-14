package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// TestStore_QueryStaysWithinMemoryBudget is the regression test for the
// 2026-04-26 LA OOM. The pre-SQLite path read 60MB+ of detail JSONs into
// memory (5-10x amplified by Go's JSON unmarshal) when any dim filter was
// active. With SQLite, every read is a SQL aggregate and the working set
// never depends on dataset size — this test asserts that even a large
// query keeps HeapAlloc below 50MB above the baseline.
//
// 50 events is small but the test serves a different purpose: it exercises
// every read path (Query, QueryEvents, SnapshotFiltered) to catch the case
// where someone reintroduces "read-then-slice" by accident. A larger event
// count is left for benchmarks; this is a guard, not a load test.
func TestStore_QueryStaysWithinMemoryBudget(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()

	ctx := context.Background()

	// Insert 50 events spread over 24 hours.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	base := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	for i := range 50 {
		// The rune conversion documents the intentional integer-to-code-point conversion for go vet.
		// noinspection GoRedundantConversion
		d := FlatDetail{
			Timestamp: base.Add(time.Duration(i*30) * time.Minute),
			Model:     "m" + string(rune('0'+i%5)),
			APIKey:    "k" + string(rune('0'+i%3)),
			Source:    "src" + string(rune('0'+i%4)),
			Tokens:    tokenStats{InputTokens: int64(i * 10), OutputTokens: int64(i * 20), TotalTokens: int64(i * 30)},
			Failed:    i%7 == 0,
		}
		if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
			t.Fatalf("persist[%d]: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Baseline memory before the read paths kick in.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	from := base.Add(-time.Hour)
	to := base.Add(48 * time.Hour)

	// Hit every read path that the LA incident touched.
	if _, err := store.querySummary(ctx, from, to, EventFilters{Model: "m1,m2", APIKey: "k0"}, "hourly", true); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, _, err := store.queryEventDetails(ctx, from, to, EventFilters{}, 1, 100, "timestamp", false); err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	// QuerySnapshotFiltered rebuilds the detail-heavy v1 shape used by legacy /usage.
	p := &Persister{store: store}
	if _, err := p.querySnapshotFiltered(from, to, EventFilters{}); err != nil {
		t.Fatalf("QuerySnapshotFiltered: %v", err)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Allow up to 50MB of growth. The pre-SQLite path used to grow by
	// 300-600MB on the same workload, so this is a generous guard.
	const budget = 50 * 1024 * 1024
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > budget {
		t.Fatalf("HeapAlloc grew by %d bytes during reads, budget %d", growth, budget)
	}
}

func TestStore_QueryWithAuthIndexAggregatesCredentialUsage(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	from := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	events := []FlatDetail{
		{
			Timestamp: from.Add(10 * time.Minute),
			Model:     "model-a",
			Source:    "config-a",
			AuthIndex: "auth-a",
			APIKey:    "client-key",
			Provider:  "claude",
			Tokens: tokenStats{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
			},
		},
		{
			Timestamp: from.Add(time.Hour + 5*time.Minute),
			Model:     "model-a",
			Source:    "config-a",
			AuthIndex: "auth-a",
			APIKey:    "client-key",
			Provider:  "claude",
			Tokens: tokenStats{
				InputTokens:  20,
				OutputTokens: 10,
				TotalTokens:  30,
			},
			Failed: true,
		},
		{
			Timestamp: from.Add(2*time.Hour + 15*time.Minute),
			Model:     "model-b",
			Source:    "config-b",
			AuthIndex: "auth-b",
			APIKey:    "client-key",
			Provider:  "openai-compatibility",
			Tokens: tokenStats{
				InputTokens:  30,
				OutputTokens: 15,
				TotalTokens:  45,
			},
		},
		{
			Timestamp: from.Add(2*time.Hour + 30*time.Minute),
			Model:     "model-b",
			Source:    "oauth.json",
			APIKey:    "client-key",
			Provider:  "claude",
			Tokens:    tokenStats{TotalTokens: 1},
		},
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := range events {
		if _, err = persistEvent(ctx, tx, store, &events[i], nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist event %d: %v", i, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	to := from.Add(3 * time.Hour)
	regular, err := store.querySummary(ctx, from, to, EventFilters{}, "hourly", true)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(regular.ByAuthIndex) != 0 || len(regular.TimeSeriesByAuthIndex) != 0 {
		t.Fatalf("regular query returned auth-index groups: %+v", regular.ByAuthIndex)
	}

	result, err := store.queryWithAuthIndex(ctx, from, to, EventFilters{}, "hourly", true)
	if err != nil {
		t.Fatalf("QueryWithAuthIndex: %v", err)
	}
	if result.Totals.Requests != int64(len(events)) {
		t.Fatalf("total requests = %d, want %d", result.Totals.Requests, len(events))
	}
	statsA := result.ByAuthIndex["auth-a"]
	if statsA == nil {
		t.Fatal("missing auth-a aggregate")
	}
	if statsA.Requests != 2 || statsA.Success != 1 || statsA.Failure != 1 || statsA.Tokens.TotalTokens != 45 {
		t.Fatalf("auth-a aggregate = %+v", statsA)
	}
	statsB := result.ByAuthIndex["auth-b"]
	if statsB == nil || statsB.Requests != 1 || statsB.Success != 1 || statsB.Failure != 0 {
		t.Fatalf("auth-b aggregate = %+v", statsB)
	}
	if len(result.ByAuthIndex) != 2 {
		t.Fatalf("auth-index group count = %d, want 2", len(result.ByAuthIndex))
	}

	seriesA := result.TimeSeriesByAuthIndex["auth-a"]
	if len(seriesA) != 4 {
		t.Fatalf("auth-a point count = %d, want 4", len(seriesA))
	}
	requests := make([]int64, len(seriesA))
	for i := range seriesA {
		requests[i] = seriesA[i].Requests
	}
	if got, want := fmt.Sprint(requests), "[1 1 0 0]"; got != want {
		t.Fatalf("auth-a requests = %s, want %s", got, want)
	}
}

// TestSnapshotFilteredPreservesRuntimeMetadata verifies that filtered snapshots retain persisted request metadata.
func TestSnapshotFilteredPreservesRuntimeMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	d := FlatDetail{
		Timestamp:       ts,
		Model:           "m",
		APIKey:          "k",
		ReasoningEffort: "medium",
		ServiceTier:     "flex",
		ExecutorType:    "OpenAICompatExecutor",
		Tokens:          tokenStats{TotalTokens: 1},
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p := &Persister{store: store}
	snap, err := p.querySnapshotFiltered(ts.Add(-time.Minute), ts.Add(time.Minute), EventFilters{})
	if err != nil {
		t.Fatalf("QuerySnapshotFiltered: %v", err)
	}
	detail := snap.APIs[coreusage.HashAPIKeyIdentity("k")].Models["m"].Details[0]
	if got := detail.ReasoningEffort; got != "medium" {
		t.Fatalf("ReasoningEffort = %q, want medium", got)
	}
	if got := detail.ServiceTier; got != "flex" {
		t.Fatalf("ServiceTier = %q, want flex", got)
	}
	if got := detail.ExecutorType; got != "OpenAICompatExecutor" {
		t.Fatalf("ExecutorType = %q, want OpenAICompatExecutor", got)
	}
}

func TestQueryEventsPreservesUsageV2AndDefaultsLegacyGenerate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	breakdown := coreusage.NewPartialSubsetTokenBreakdown(7, 2, 0, 3, 1, 12)
	detail := FlatDetail{
		Timestamp:         timestamp,
		Model:             "model-v2",
		Generate:          false,
		ClientIP:          "203.0.113.55",
		XForwardedFor:     "198.51.100.5, 203.0.113.55",
		UserAgent:         "sqlite-test-client/1.0",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    breakdown,
		Tokens: tokenStats{
			InputTokens: 7, OutputTokens: 3, ReasoningTokens: 1, CachedTokens: 2, TotalTokens: 12,
		},
		generateRecorded: true,
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = persistEvent(ctx, transaction, store, &detail, nil); err != nil {
		_ = transaction.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err = transaction.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, _, err := store.queryEventDetails(
		ctx, timestamp.Add(-time.Minute), timestamp.Add(time.Minute), EventFilters{}, 1, 10, "timestamp", false,
	)
	if err != nil {
		t.Fatalf("QueryEvents(v2): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("QueryEvents(v2) count = %d, want 1", len(events))
	}
	got := events[0]
	if got.Generate || got.ClientIP != detail.ClientIP || got.XForwardedFor != detail.XForwardedFor || got.UserAgent != detail.UserAgent {
		t.Fatalf("v2 event metadata changed: %+v", got)
	}
	if got.AccountingVersion != coreusage.TokenAccountingSchemaVersion || got.TokenBreakdown != breakdown {
		t.Fatalf("v2 accounting changed: version=%d breakdown=%+v", got.AccountingVersion, got.TokenBreakdown)
	}

	if _, err = store.db.ExecContext(ctx, "UPDATE events SET metadata = NULL"); err != nil {
		t.Fatalf("clear metadata: %v", err)
	}
	events, _, err = store.queryEventDetails(
		ctx, timestamp.Add(-time.Minute), timestamp.Add(time.Minute), EventFilters{}, 1, 10, "timestamp", false,
	)
	if err != nil {
		t.Fatalf("QueryEvents(legacy): %v", err)
	}
	if len(events) != 1 || !events[0].Generate || events[0].AccountingVersion != 0 || events[0].TokenBreakdown.Valid() {
		t.Fatalf("legacy event = %+v, want Generate=true without v2 metadata", events)
	}
}

func TestQuerySnapshotFilteredUsesExactEventRangeForAllAggregates(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, minute := range []int{10, 30, 50} {
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(minute) * time.Minute),
			Model:     "m",
			APIKey:    "k",
			Failed:    index == 1,
			Tokens:    tokenStats{TotalTokens: int64(index + 1)},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p := &Persister{store: store}
	snap, err := p.querySnapshotFiltered(
		base.Add(20*time.Minute),
		base.Add(40*time.Minute),
		EventFilters{},
	)
	if err != nil {
		t.Fatalf("QuerySnapshotFiltered: %v", err)
	}
	if snap.TotalRequests != 1 || snap.SuccessCount != 0 || snap.FailureCount != 1 || snap.TotalTokens != 2 {
		t.Fatalf(
			"totals = requests:%d success:%d failure:%d tokens:%d, want 1/0/1/2", snap.TotalRequests, snap.SuccessCount,
			snap.FailureCount, snap.TotalTokens,
		)
	}
	if snap.RequestsByDay["2026-05-20"] != 1 || snap.RequestsByHour["10"] != 1 || snap.TokensByDay["2026-05-20"] != 2 || snap.TokensByHour["10"] != 2 {
		t.Fatalf(
			"time maps = %#v %#v %#v %#v", snap.RequestsByDay, snap.RequestsByHour, snap.TokensByDay, snap.TokensByHour,
		)
	}
	api := snap.APIs[coreusage.HashAPIKeyIdentity("k")]
	model := api.Models["m"]
	if api.TotalRequests != 1 || api.TotalTokens != 2 || model.TotalRequests != 1 || model.TotalTokens != 2 || len(model.Details) != 1 {
		t.Fatalf("API/model aggregate mismatch: api=%+v model=%+v", api, model)
	}
}

func TestQuerySnapshotLimitedContextRejectsOversizedResult(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index := range 3 {
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(index) * time.Minute),
			Model:     "m",
			Failed:    index == 2,
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p := &Persister{store: store}
	if _, err = p.QuerySnapshotLimitedContext(ctx, 2); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("QuerySnapshotLimitedContext error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}
	snapshot, err := p.QuerySnapshotLimitedContext(ctx, 3)
	if err != nil {
		t.Fatalf("QuerySnapshotLimitedContext exact limit: %v", err)
	}
	if snapshot.TotalRequests != 3 || snapshot.FailureCount != 1 {
		t.Fatalf(
			"snapshot totals = requests:%d failures:%d, want 3/1",
			snapshot.TotalRequests,
			snapshot.FailureCount,
		)
	}

	aggregateSnapshot, err := p.QueryAggregateSnapshotContext(ctx)
	if err != nil {
		t.Fatalf("QueryAggregateSnapshotContext: %v", err)
	}
	if aggregateSnapshot.TotalRequests != 3 || aggregateSnapshot.SuccessCount != 2 ||
		aggregateSnapshot.FailureCount != 1 || aggregateSnapshot.TotalTokens != 3 {
		t.Fatalf(
			"aggregate totals = requests:%d success:%d failures:%d tokens:%d, want 3/2/1/3",
			aggregateSnapshot.TotalRequests,
			aggregateSnapshot.SuccessCount,
			aggregateSnapshot.FailureCount,
			aggregateSnapshot.TotalTokens,
		)
	}
	dayKey := base.Format("2006-01-02")
	hourKey := formatHour(base.Hour())
	if aggregateSnapshot.RequestsByDay[dayKey] != 3 || aggregateSnapshot.RequestsByHour[hourKey] != 3 ||
		aggregateSnapshot.TokensByDay[dayKey] != 3 || aggregateSnapshot.TokensByHour[hourKey] != 3 {
		t.Fatalf(
			"aggregate time maps = %#v %#v %#v %#v, want 3 in %s/%s",
			aggregateSnapshot.RequestsByDay,
			aggregateSnapshot.RequestsByHour,
			aggregateSnapshot.TokensByDay,
			aggregateSnapshot.TokensByHour,
			dayKey,
			hourKey,
		)
	}
	if len(aggregateSnapshot.APIs) != 0 {
		t.Fatalf("aggregate APIs = %#v, want bounded empty map", aggregateSnapshot.APIs)
	}

	largeMetadata := `{"response_headers":{"X-Test":["` + strings.Repeat("x", 8<<10) + `"]}}`
	if _, err = store.db.ExecContext(
		ctx,
		"UPDATE events SET metadata = ? WHERE ts_ns = ?",
		largeMetadata,
		base.UnixNano(),
	); err != nil {
		t.Fatalf("set large metadata: %v", err)
	}
	if _, err = p.querySnapshotFilteredContext(
		ctx,
		time.Time{},
		time.Time{},
		EventFilters{},
		3,
		4<<10,
	); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("byte-limited snapshot error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}

	cancelledContext, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = p.QuerySnapshotLimitedContext(cancelledContext, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled QuerySnapshotLimitedContext error = %v, want %v", err, context.Canceled)
	}
	if _, err = p.QueryAggregateSnapshotContext(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled QueryAggregateSnapshotContext error = %v, want %v", err, context.Canceled)
	}
	if _, _, err = p.QueryRequestCountsContext(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled QueryRequestCountsContext error = %v, want %v", err, context.Canceled)
	}

	totalRequests, failureCount, err := p.QueryRequestCountsContext(ctx)
	if err != nil {
		t.Fatalf("QueryRequestCountsContext: %v", err)
	}
	if totalRequests != 3 || failureCount != 1 {
		t.Fatalf("request counts = total:%d failures:%d, want 3/1", totalRequests, failureCount)
	}
}

func TestQuerySnapshotDetailBudgetAccountsForStructureAndEscaping(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	detail := FlatDetail{Timestamp: timestamp, Model: "m"}
	if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p := &Persister{store: store}
	headerValues := strings.Repeat(`"",`, 1_000) + `""`
	nodeHeavyMetadata := `{"response_headers":{"X-Test":[` + headerValues + `]}}`
	if _, err = store.db.ExecContext(
		ctx,
		"UPDATE events SET metadata = ? WHERE ts_ns = ?",
		nodeHeavyMetadata,
		timestamp.UnixNano(),
	); err != nil {
		t.Fatalf("set node-heavy metadata: %v", err)
	}
	if _, err = p.querySnapshotFilteredContext(
		ctx,
		time.Time{},
		time.Time{},
		EventFilters{},
		1,
		16<<10,
	); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("node-heavy snapshot error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}

	if _, err = store.db.ExecContext(
		ctx, "UPDATE events SET metadata = '{}' WHERE ts_ns = ?", timestamp.UnixNano(),
	); err != nil {
		t.Fatalf("reset metadata: %v", err)
	}
	if _, err = store.db.ExecContext(ctx, "UPDATE dim_model SET name = ?", strings.Repeat("<", 2<<10)); err != nil {
		t.Fatalf("set escaped model name: %v", err)
	}
	if _, err = p.querySnapshotFilteredContext(
		ctx,
		time.Time{},
		time.Time{},
		EventFilters{},
		1,
		8<<10,
	); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("escaped-string snapshot error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}
}

func TestPostImportMaintenanceDistinguishesRequestAndInternalTimeouts(t *testing.T) {
	result := ImportResult{}
	if err := handlePostImportMaintenanceError(
		context.Background(),
		&result,
		"checkpoint after import",
		context.DeadlineExceeded,
	); err != nil {
		t.Fatalf("internal maintenance timeout error = %v, want nil", err)
	}
	if result.Warning != "usage imported, but post-import maintenance did not finish" {
		t.Fatalf("internal maintenance warning = %q", result.Warning)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = ImportResult{}
	if err := handlePostImportMaintenanceError(
		ctx,
		&result,
		"checkpoint after import",
		context.Canceled,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("request cancellation error = %v, want %v", err, context.Canceled)
	}
	if result.Warning != "" {
		t.Fatalf("request cancellation warning = %q, want empty", result.Warning)
	}
}

func TestCheckpointContextHonorsCancellation(t *testing.T) {
	store := openTestStore(t)
	p := &Persister{store: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.checkpointContext(ctx, "TRUNCATE"); !errors.Is(err, context.Canceled) {
		t.Fatalf("checkpointContext() error = %v, want %v", err, context.Canceled)
	}
}

func TestQueryAggregateSnapshotCollapsesHighCardinalityDimensions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const dimensions = 5_000
	bucket := time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(
		ctx,
		`WITH RECURSIVE seq(value) AS (
			SELECT 1
			UNION ALL
			SELECT value + 1 FROM seq WHERE value < ?
		)
		INSERT INTO dim_model(name)
		SELECT printf('model-%05d', value) FROM seq`,
		dimensions,
	); err != nil {
		t.Fatalf("insert dimensions: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`INSERT INTO hour_bucket(
			bucket_ts_ns,
			model_id,
			requests,
			success,
			failure,
			total_tokens
		)
		SELECT ?, id, 1, 1, 0, 1 FROM dim_model`,
		bucket.UnixNano(),
	); err != nil {
		t.Fatalf("insert high-cardinality buckets: %v", err)
	}

	p := &Persister{store: store}
	snapshot, err := p.QueryAggregateSnapshotContext(ctx)
	if err != nil {
		t.Fatalf("QueryAggregateSnapshotContext: %v", err)
	}
	dayKey := bucket.Format("2006-01-02")
	hourKey := formatHour(bucket.Hour())
	if snapshot.TotalRequests != dimensions ||
		snapshot.SuccessCount != dimensions ||
		snapshot.TotalTokens != dimensions ||
		snapshot.RequestsByDay[dayKey] != dimensions ||
		snapshot.RequestsByHour[hourKey] != dimensions ||
		len(snapshot.APIs) != 0 {
		t.Fatalf("aggregate snapshot = %+v, want %d collapsed dimensions", snapshot, dimensions)
	}
}

func TestQuerySnapshotLimitHasBoundedAllocations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "INSERT INTO dim_model(name) VALUES('m')"); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	_, err := store.db.ExecContext(
		ctx,
		`WITH RECURSIVE seq(value) AS (
			SELECT 1
			UNION ALL
			SELECT value + 1 FROM seq WHERE value < 20000
		)
		INSERT INTO events(fingerprint, ts_ns, model_id, failed)
		SELECT CAST(printf('%020d', value) AS BLOB), value,
		       (SELECT id FROM dim_model WHERE name = 'm'), 0
		FROM seq`,
	)
	if err != nil {
		t.Fatalf("insert events: %v", err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	p := &Persister{store: store}
	if _, err = p.querySnapshot(); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("QuerySnapshot error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	const allocationBudget = 8 << 20
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > allocationBudget {
		t.Fatalf("limited snapshot allocated %d bytes, budget %d", allocated, allocationBudget)
	}
}

func TestPadTimeSeriesFloorsNegativeUnixTimestamps(t *testing.T) {
	from := time.Date(1969, 12, 31, 23, 30, 0, 0, time.UTC)
	to := time.Date(1970, 1, 1, 1, 30, 0, 0, time.UTC)
	result := QueryResult{
		TimeSeries: []timePoint{
			{
				Time:     "1969-12-31T23:00:00Z",
				Requests: 1,
			},
		},
		TimeSeriesByModel:      map[string][]timePoint{},
		TimeSeriesByCredential: map[string][]timePoint{},
		TimeSeriesByAPIKey:     map[string][]timePoint{},
	}
	if err := padTimeSeries(&result, from.UnixNano(), to.UnixNano(), nsPerHour); err != nil {
		t.Fatalf("padTimeSeries: %v", err)
	}
	wantTimes := []string{
		"1969-12-31T23:00:00Z",
		"1970-01-01T00:00:00Z",
		"1970-01-01T01:00:00Z",
	}
	if len(result.TimeSeries) != len(wantTimes) {
		t.Fatalf("time series length = %d, want %d", len(result.TimeSeries), len(wantTimes))
	}
	for index, want := range wantTimes {
		if result.TimeSeries[index].Time != want {
			t.Fatalf("time series[%d] = %q, want %q", index, result.TimeSeries[index].Time, want)
		}
	}
	if result.TimeSeries[0].Requests != 1 {
		t.Fatalf("existing pre-epoch bucket requests = %d, want 1", result.TimeSeries[0].Requests)
	}
}

func TestSummarySeriesLimitBoundsBucketsPointsAndOverflow(t *testing.T) {
	fromNs := int64(0)
	toNs := int64(maxSummaryBucketCount-1) * nsPerHour
	limit, err := summarySeriesLimit(fromNs, toNs, nsPerHour)
	if err != nil {
		t.Fatalf("summarySeriesLimit exact bucket limit: %v", err)
	}
	wantLimit := maxSummaryTotalPoints/maxSummaryBucketCount - 1
	if limit != wantLimit {
		t.Fatalf("series limit = %d, want %d", limit, wantLimit)
	}

	_, err = summarySeriesLimit(fromNs, int64(maxSummaryBucketCount)*nsPerHour, nsPerHour)
	if !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("oversized bucket range error = %v, want %v", err, ErrSummaryQueryTooLarge)
	}
	if _, err = timeSeriesBucketCount(math.MinInt64, math.MaxInt64, 1); !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("overflowing bucket span error = %v, want %v", err, ErrSummaryQueryTooLarge)
	}
}

func TestQuerySummaryRangeLimitIsMonotonic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, days := range []int{83, 84, 90, 91} {
		if _, err := store.querySummary(
			ctx,
			from,
			from.Add(time.Duration(days)*24*time.Hour),
			EventFilters{},
			"hourly",
			true,
		); err != nil {
			t.Fatalf("Query %d-day range: %v", days, err)
		}
	}
}

func TestQueryDaySummaryUsesHourBucketsForDirtyDays(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	persist := func(timestamp time.Time) {
		t.Helper()
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		detail := FlatDetail{
			Timestamp: timestamp,
			Model:     "model-a",
			Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
		}
		if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist event: %v", err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	persist(day.Add(time.Hour))
	if err := store.rollupDay(ctx, day.UnixNano()); err != nil {
		t.Fatalf("RollupDay: %v", err)
	}
	persist(day.Add(2 * time.Hour))
	var sourceRows int
	if err := store.db.QueryRowContext(
		ctx, "SELECT COUNT(*) FROM "+daySummaryBucketSource(),
	).Scan(&sourceRows); err != nil {
		t.Fatalf("query day summary source: %v", err)
	}
	if sourceRows != 2 {
		t.Fatalf("day summary source rows = %d, want 2", sourceRows)
	}

	result, err := store.querySummary(
		ctx,
		day.Add(-84*24*time.Hour),
		day.Add(23*time.Hour),
		EventFilters{},
		"daily",
		true,
	)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Totals.Requests != 2 || result.Totals.Tokens.TotalTokens != 2 {
		t.Fatalf("totals = %+v, want both events from the dirty day", result.Totals)
	}
}

func TestEnsureSummaryInputBudgetRejectsExcessRowsBeforeAggregation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(
		ctx, "CREATE TEMP TABLE summary_budget_rows(bucket_ts_ns INTEGER NOT NULL)",
	); err != nil {
		t.Fatalf("create summary budget table: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`WITH RECURSIVE seq(value) AS (
			SELECT 0
			UNION ALL
			SELECT value + 1 FROM seq WHERE value < ?
		)
		INSERT INTO summary_budget_rows(bucket_ts_ns)
		SELECT 0 FROM seq`,
		maxSummaryInputRows,
	); err != nil {
		t.Fatalf("insert summary budget rows: %v", err)
	}

	err := ensureSummaryInputBudget(
		ctx,
		store.db,
		"summary_budget_rows",
		0,
		0,
		resolvedFilter{},
		resolvedCredentialFilter{},
		resolvedFilter{},
	)
	if !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("ensureSummaryInputBudget error = %v, want %v", err, ErrSummaryQueryTooLarge)
	}
}

func TestEnsureAuthIndexInputBudgetRejectsExcessRowsBeforeAggregation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(
		ctx,
		"CREATE TEMP TABLE events(ts_ns INTEGER NOT NULL, auth_index_id INTEGER)",
	); err != nil {
		t.Fatalf("create auth-index budget table: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`WITH RECURSIVE seq(value) AS (
			SELECT 0
			UNION ALL
			SELECT value + 1 FROM seq WHERE value < ?
		)
		INSERT INTO events(ts_ns, auth_index_id)
		SELECT 0, 1 FROM seq`,
		maxSummaryInputRows,
	); err != nil {
		t.Fatalf("insert auth-index budget rows: %v", err)
	}

	err := ensureAuthIndexInputBudget(
		ctx,
		store.db,
		0,
		0,
		resolvedFilter{},
		resolvedCredentialFilter{},
		resolvedFilter{},
	)
	if !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("ensureAuthIndexInputBudget error = %v, want %v", err, ErrSummaryQueryTooLarge)
	}
}

func TestQueryRejectsExcessSummaryDimensions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index := 0; index <= maxSummarySeriesCount; index++ {
		detail := FlatDetail{
			Timestamp: base,
			Model:     fmt.Sprintf("model-%03d", index),
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err = store.querySummary(ctx, base, base.Add(time.Hour), EventFilters{}, "hourly", true)
	if !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("Query error = %v, want %v", err, ErrSummaryQueryTooLarge)
	}
}

func TestQueryRejectsGroupedSeriesPointBudget(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index := range 50 {
		detail := FlatDetail{
			Timestamp: base,
			Model:     fmt.Sprintf("model-%02d", index),
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	to := base.Add(time.Duration(maxSummaryBucketCount-1) * time.Hour)
	_, err = store.querySummary(ctx, base, to, EventFilters{}, "hourly", true)
	if !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("Query error = %v, want %v", err, ErrSummaryQueryTooLarge)
	}
}

func TestQueryEventsSupportsLargeCandidateRange(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	modelID, err := store.resolveModelID(ctx, tx, "candidate-model")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("resolve model: %v", err)
	}
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	const eventCount = 100_001
	if _, err = tx.ExecContext(
		ctx,
		`WITH RECURSIVE seq(value) AS (
			SELECT 0
			UNION ALL
			SELECT value + 1 FROM seq WHERE value < ?
		)
		INSERT INTO events(fingerprint, ts_ns, model_id, failed)
		SELECT CAST(printf('%020d', value) AS BLOB), ? + value, ?, 0 FROM seq`,
		eventCount-1,
		base.UnixNano(),
		modelID,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert event candidates: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, total, err := store.queryEventDetails(
		ctx,
		base,
		base.Add(time.Duration(eventCount-1)*time.Nanosecond),
		EventFilters{},
		1,
		50,
		"timestamp",
		false,
	)
	if err != nil {
		t.Fatalf("QueryEvents() error = %v", err)
	}
	if total != eventCount {
		t.Fatalf("total = %d, want %d", total, eventCount)
	}
	if len(events) != 50 {
		t.Fatalf("len(events) = %d, want 50", len(events))
	}
}

func TestQueryCompactEventsProjectsRequestListFields(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	budget := 4096
	detail := FlatDetail{
		Timestamp:         base,
		Model:             "compact-model",
		Source:            "compact-source",
		AuthIndex:         "compact-auth",
		APIKey:            "compact-api-key",
		Provider:          "compact-provider",
		LatencyMs:         1250,
		RequestID:         "compact-request",
		TimeToFirstByteMs: 250,
		TotalDurationMs:   1500,
		Completed:         true,
		ClientIP:          "192.0.2.1",
		XForwardedFor:     "198.51.100.1",
		UserAgent:         "compact-test-agent",
		ReasoningEffort:   "high",
		Thinking: &Thinking{
			Intensity: "high",
			Mode:      "enabled",
			Level:     "deep",
			Budget:    &budget,
		},
		Tokens: tokenStats{
			InputTokens:      10,
			OutputTokens:     20,
			ReasoningTokens:  5,
			CachedTokens:     4,
			CacheReadTokens:  3,
			CacheWriteTokens: 1,
			TotalTokens:      30,
		},
		CostStatus:  costStatusPartial,
		CostReason:  "partial test price",
		Failed:      true,
		costMicro:   1250,
		costTrusted: true,
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err = persistRequestLogArtifact(
		ctx, tx, logging.RequestLogArtifact{
			RequestID: detail.RequestID,
			State:     logging.RequestLogAvailable,
			Location:  logging.RequestLogLocationHome,
			HomeIP:    "203.0.113.1",
			Revision:  1,
			UpdatedAt: base.Add(time.Second),
		},
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist request log artifact: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, total, err := store.queryCompactEventDetails(
		ctx,
		base.Add(-time.Minute),
		base.Add(time.Minute),
		EventFilters{},
		1,
		50,
		"timestamp",
		true,
	)
	if err != nil {
		t.Fatalf("QueryCompactEvents: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("QueryCompactEvents total/events = %d/%d, want 1/1", total, len(events))
	}
	got := events[0]
	if got.Timestamp != base || got.Model != detail.Model || got.Source != detail.Source ||
		got.AuthIndex != detail.AuthIndex || got.APIKey != detail.APIKey || got.Provider != detail.Provider ||
		got.LatencyMs != detail.LatencyMs || got.RequestID != detail.RequestID ||
		got.TimeToFirstByteMs != detail.TimeToFirstByteMs || got.TotalDurationMs != detail.TotalDurationMs ||
		!got.Completed || !got.MetadataRecorded || got.Thinking == nil || got.Thinking.Budget == nil ||
		*got.Thinking.Budget != budget || got.Tokens != detail.Tokens || got.Cost != 0.00125 ||
		got.CostStatus != costStatusPartial || got.CostReason != detail.CostReason || !got.Failed ||
		got.RequestLogState != logging.RequestLogAvailable || got.RequestLogHomeIP != "203.0.113.1" {
		t.Fatalf("compact event = %#v, want projected request-list fields", got)
	}
	if got.ClientIP != "" || got.XForwardedFor != "" || got.UserAgent != "" ||
		got.ReasoningEffort != "" || got.ResponseHeaders != nil || got.AccountingVersion != 0 ||
		got.TokenBreakdown.Valid() {
		t.Fatalf("compact event retained detail-only metadata: %#v", got)
	}
}

func TestQueryEventsRejectsMetadataPageOverByteBudget(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	modelID, err := store.resolveModelID(ctx, tx, "metadata-model")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("resolve model: %v", err)
	}
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	metadata := `{"request_id":"` + strings.Repeat("x", maxEventMetadataBytes-64) + `"}`
	rows := maxEventsMetadataBytes/len(metadata) + 1
	for index := range rows {
		fingerprint := []byte(fmt.Sprintf("%020d", index))
		if _, err = tx.ExecContext(
			ctx,
			"INSERT INTO events(fingerprint, ts_ns, model_id, failed, metadata) VALUES (?, ?, ?, 0, ?)",
			fingerprint,
			base.Add(time.Duration(index)*time.Nanosecond).UnixNano(),
			modelID,
			metadata,
		); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert metadata event[%d]: %v", index, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, _, err = store.queryEventDetails(
		ctx,
		base.Add(-time.Second),
		base.Add(time.Second),
		EventFilters{},
		1,
		MaxEventsPageSize,
		"timestamp",
		true,
	)
	if !errors.Is(err, ErrEventsPageTooLarge) {
		t.Fatalf("QueryEvents error = %v, want %v", err, ErrEventsPageTooLarge)
	}
}

func TestQueryEventsRejectsOutOfRangeTimestamp(t *testing.T) {
	store := openTestStore(t)
	_, _, err := store.queryEventDetails(
		context.Background(),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		EventFilters{},
		1,
		50,
		"timestamp",
		true,
	)
	if !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("QueryEvents error = %v, want %v", err, ErrTimestampOutOfRange)
	}
}

func TestQueryEventsRejectsDeepOffsetBeforeQuerying(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	pageSize := MaxEventsPageSize
	page := maxEventsOffset/pageSize + 2

	_, _, err := store.queryEventDetails(
		context.Background(),
		base,
		base.Add(time.Hour),
		EventFilters{},
		page,
		pageSize,
		"timestamp",
		true,
	)
	if !errors.Is(err, ErrEventsPageTooDeep) {
		t.Fatalf("QueryEvents error = %v, want %v", err, ErrEventsPageTooDeep)
	}
}

func TestHourBucketDayBoundsUseFloorBeforeUnixEpoch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, timestamp := range []time.Time{
		time.Date(1969, 12, 30, 23, 30, 0, 0, time.UTC),
		time.Date(1969, 12, 31, 1, 30, 0, 0, time.UTC),
	} {
		detail := FlatDetail{
			Timestamp: timestamp,
			Model:     "m",
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	earliest, found, err := store.earliestHourBucketDay(ctx)
	if err != nil {
		t.Fatalf("earliestHourBucketDay: %v", err)
	}
	wantEarliest := time.Date(1969, 12, 30, 0, 0, 0, 0, time.UTC).UnixNano()
	if !found || earliest != wantEarliest {
		t.Fatalf("earliest day = (%d, %t), want (%d, true)", earliest, found, wantEarliest)
	}
	latest, found, err := store.latestHourBucketDay(ctx)
	if err != nil {
		t.Fatalf("latestHourBucketDay: %v", err)
	}
	wantLatest := time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC).UnixNano()
	if !found || latest != wantLatest {
		t.Fatalf("latest day = (%d, %t), want (%d, true)", latest, found, wantLatest)
	}
}

func TestQuerySnapshotFilteredReturnsMalformedMetadataError(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	detail := FlatDetail{
		Timestamp: ts,
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 1},
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx, "UPDATE events SET metadata = '{' WHERE ts_ns = ?", ts.UnixNano(),
	); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}

	p := &Persister{store: store}
	_, err = p.querySnapshotFiltered(ts.Add(-time.Minute), ts.Add(time.Minute), EventFilters{})
	if err == nil || !strings.Contains(err.Error(), "decode snapshot event metadata") {
		t.Fatalf("QuerySnapshotFiltered error = %v, want malformed metadata error", err)
	}
}

func TestQuerySnapshotFilteredReturnsDatabaseError(t *testing.T) {
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p := &Persister{store: store}
	if _, err := p.querySnapshotFiltered(time.Time{}, time.Time{}, EventFilters{}); err == nil {
		t.Fatal("QuerySnapshotFiltered error = nil, want closed database error")
	}
}

func TestQuerySnapshotFilteredRejectsAggregateOverflow(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "INSERT INTO dim_model(name) VALUES('m')"); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, totalTokens := range []int64{math.MaxInt64, 1} {
		detail := FlatDetail{
			Timestamp: time.Unix(0, int64(index)*nsPerHour).UTC(),
			Model:     "m",
			Tokens:    tokenStats{TotalTokens: totalTokens},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist event[%d]: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	p := &Persister{store: store}
	_, err = p.querySnapshotFiltered(time.Time{}, time.Time{}, EventFilters{})
	if err == nil || !strings.Contains(err.Error(), "snapshot token aggregate overflows int64") {
		t.Fatalf("QuerySnapshotFiltered overflow error = %v", err)
	}
}

func TestQueryEventsTransactionKeepsCountAndPageInOneSnapshot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	seed := []byte(`{
		"date":"2026-04-27",
		"details":[
			{"timestamp":"2026-04-27T10:00:00Z","model":"before","tokens":{"total_tokens":1}}
		]
	}`)
	if _, err := importBytes(ctx, store, nil, seed); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read transaction: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var pinnedCount int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&pinnedCount); err != nil {
		t.Fatalf("pin read snapshot: %v", err)
	}
	if pinnedCount != 1 {
		t.Fatalf("pinned event count = %d, want 1", pinnedCount)
	}

	concurrentImport := []byte(`{
		"date":"2026-04-27",
		"details":[
			{"timestamp":"2026-04-27T10:01:00Z","model":"after","tokens":{"total_tokens":2}}
		]
	}`)
	if _, err = importBytes(ctx, store, nil, concurrentImport); err != nil {
		t.Fatalf("concurrent import: %v", err)
	}

	fromNs, err := unixNanoUTC(base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("query start: %v", err)
	}
	toNs, err := unixNanoUTC(base.Add(time.Hour))
	if err != nil {
		t.Fatalf("query end: %v", err)
	}
	events, total, err := store.queryEvents(
		ctx,
		tx,
		fromNs,
		toNs,
		EventFilters{},
		10,
		0,
		"timestamp",
		false,
	)
	if err != nil {
		t.Fatalf("query pinned snapshot: %v", err)
	}
	if total != 1 || len(events) != 1 || events[0].Model != "before" {
		t.Fatalf("pinned query = total %d events %+v, want only pre-import event", total, events)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit read transaction: %v", err)
	}
	committed = true

	events, total, err = store.queryEventDetails(
		ctx,
		base.Add(-time.Hour),
		base.Add(time.Hour),
		EventFilters{},
		1,
		10,
		"timestamp",
		false,
	)
	if err != nil {
		t.Fatalf("query current snapshot: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("current query = total %d len %d, want 2/2", total, len(events))
	}
}

func TestStartRollup_DayBucketReadyOnReturn(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert one event so rollupAllDays has something to aggregate. Without a
	// row in hour_bucket, day_bucket would remain empty even with synchronous
	// rollup, defeating the test.
	d := FlatDetail{
		Timestamp: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 100},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Sanity: day_bucket is empty before StartRollup runs.
	var preCount int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM day_bucket").Scan(&preCount); err != nil {
		t.Fatalf("pre count: %v", err)
	}
	if preCount != 0 {
		t.Fatalf("day_bucket should be empty before rollup, got %d rows", preCount)
	}

	stop := store.startRollup(ctx)
	defer stop()

	// The instant StartRollup returns, day_bucket MUST already contain the
	// rolled-up row. With async rollup this would race and usually be 0.
	var postCount int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM day_bucket").Scan(&postCount); err != nil {
		t.Fatalf("post count: %v", err)
	}
	if postCount == 0 {
		t.Fatalf("day_bucket empty after StartRollup returned — initial rollup must be synchronous")
	}
}

func TestGetDayRollupWatermarkRejectsRFC3339OutsideUnixNanoRange(t *testing.T) {
	tests := []string{
		"0001-01-01T00:00:00Z",
		"9999-12-31T23:59:59.999999999Z",
	}
	for _, raw := range tests {
		t.Run(
			raw, func(t *testing.T) {
				store := openTestStore(t)
				if _, err := store.db.ExecContext(
					context.Background(),
					"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
					dayRollupWatermarkKey,
					raw,
				); err != nil {
					t.Fatalf("write watermark: %v", err)
				}

				_, _, err := store.getDayRollupWatermark(context.Background())
				if err == nil || !strings.Contains(err.Error(), "outside UnixNano range") {
					t.Fatalf("getDayRollupWatermark error = %v, want UnixNano range error", err)
				}
			},
		)
	}
}

func TestDayRollupWatermarkPreservesUnixEpoch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.setDayRollupWatermark(ctx, 0); err != nil {
		t.Fatalf("setDayRollupWatermark: %v", err)
	}
	watermark, hasWatermark, err := store.getDayRollupWatermark(ctx)
	if err != nil {
		t.Fatalf("getDayRollupWatermark: %v", err)
	}
	if !hasWatermark || watermark != 0 {
		t.Fatalf("watermark = (%d, %t), want (0, true)", watermark, hasWatermark)
	}
	next, hasNext, err := store.nextPendingRollupDay(ctx, watermark, hasWatermark)
	if err != nil {
		t.Fatalf("nextPendingRollupDay: %v", err)
	}
	if !hasNext || next != nsPerDay {
		t.Fatalf("next day = (%d, %t), want (%d, true)", next, hasNext, nsPerDay)
	}
}

func TestNextPendingRollupDayChecksOverflowBoundary(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	got, hasDay, err := store.nextPendingRollupDay(ctx, math.MaxInt64-nsPerDay, true)
	if err != nil {
		t.Fatalf("nextPendingRollupDay boundary: %v", err)
	}
	if !hasDay || got != math.MaxInt64 {
		t.Fatalf("nextPendingRollupDay = (%d, %t), want (%d, true)", got, hasDay, int64(math.MaxInt64))
	}

	_, _, err = store.nextPendingRollupDay(ctx, math.MaxInt64-nsPerDay+1, true)
	if err == nil || !strings.Contains(err.Error(), "watermark overflows int64") {
		t.Fatalf("nextPendingRollupDay overflow error = %v", err)
	}
}

func TestRollupDayRejectsEndTimestampOverflow(t *testing.T) {
	store := openTestStore(t)
	err := store.rollupDay(context.Background(), math.MaxInt64-nsPerDay+1)
	if err == nil || !strings.Contains(err.Error(), "rollup end overflows int64") {
		t.Fatalf("RollupDay overflow error = %v", err)
	}
}

func TestRollupDayRangeStopsBeforeTimestampWraparound(t *testing.T) {
	store := openTestStore(t)
	err := store.rollupDayRange(
		context.Background(),
		math.MaxInt64-nsPerDay,
		math.MaxInt64,
	)
	if err == nil || !strings.Contains(err.Error(), "rollup end overflows int64") {
		t.Fatalf("rollupDayRange overflow error = %v", err)
	}
}

func TestRollupDirtyDaysRebuildsLateEventDay(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	dayStart := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	persist := func(timestamp time.Time) {
		t.Helper()
		detail := FlatDetail{
			Timestamp: timestamp,
			Model:     "m",
			APIKey:    "k",
			Tokens:    tokenStats{TotalTokens: 1},
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	readRequests := func() int64 {
		t.Helper()
		var requests int64
		if err := store.db.QueryRowContext(
			ctx,
			"SELECT requests FROM day_bucket WHERE bucket_ts_ns = ?",
			dayStart.UnixNano(),
		).Scan(&requests); err != nil {
			t.Fatalf("query day bucket: %v", err)
		}
		return requests
	}
	readDirtyCount := func() int64 {
		t.Helper()
		var count int64
		if err := store.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM dirty_day WHERE day_ts_ns = ?",
			dayStart.UnixNano(),
		).Scan(&count); err != nil {
			t.Fatalf("query dirty day: %v", err)
		}
		return count
	}

	persist(dayStart.Add(10 * time.Hour))
	if err := store.rollupDay(ctx, dayStart.UnixNano()); err != nil {
		t.Fatalf("initial RollupDay: %v", err)
	}
	if requests := readRequests(); requests != 1 {
		t.Fatalf("initial requests = %d, want 1", requests)
	}
	if dirty := readDirtyCount(); dirty != 0 {
		t.Fatalf("dirty markers after rollup = %d, want 0", dirty)
	}

	persist(dayStart.Add(11 * time.Hour))
	if requests := readRequests(); requests != 1 {
		t.Fatalf("requests before dirty rollup = %d, want stale value 1", requests)
	}
	if dirty := readDirtyCount(); dirty != 1 {
		t.Fatalf("dirty markers after late event = %d, want 1", dirty)
	}
	if err := store.rollupDirtyDays(ctx); err != nil {
		t.Fatalf("rollupDirtyDays: %v", err)
	}
	if requests := readRequests(); requests != 2 {
		t.Fatalf("requests after dirty rollup = %d, want 2", requests)
	}
	if dirty := readDirtyCount(); dirty != 0 {
		t.Fatalf("dirty markers after rebuild = %d, want 0", dirty)
	}
}

func TestDirtyDaySurvivesStoreRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	ctx := context.Background()
	dayStart := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	persist := func(timestamp time.Time) {
		t.Helper()
		detail := FlatDetail{
			Timestamp: timestamp,
			Model:     "m",
			APIKey:    "k",
			Tokens:    tokenStats{TotalTokens: 1},
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	persist(dayStart.Add(10 * time.Hour))
	if err := store.rollupDay(ctx, dayStart.UnixNano()); err != nil {
		t.Fatalf("initial RollupDay: %v", err)
	}
	persist(dayStart.Add(11 * time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	store, err = openStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()
	var dirty int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM dirty_day WHERE day_ts_ns = ?",
		dayStart.UnixNano(),
	).Scan(&dirty); err != nil {
		t.Fatalf("query dirty day after restart: %v", err)
	}
	if dirty != 1 {
		t.Fatalf("dirty markers after restart = %d, want 1", dirty)
	}
	if err := store.rollupDirtyDays(ctx); err != nil {
		t.Fatalf("rollupDirtyDays after restart: %v", err)
	}
	var requests int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT requests FROM day_bucket WHERE bucket_ts_ns = ?",
		dayStart.UnixNano(),
	).Scan(&requests); err != nil {
		t.Fatalf("query day bucket after restart: %v", err)
	}
	if requests != 2 {
		t.Fatalf("day requests after restart = %d, want 2", requests)
	}
}

func TestRollupImportedDays_RebuildsDaysBeforeWatermark(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	seed := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 100},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &seed, nil); err != nil {
		t.Fatalf("persist seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if err := store.rollupPendingDays(ctx); err != nil {
		t.Fatalf("initial rollup: %v", err)
	}

	legacy := FlatDetail{
		Timestamp: time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 50},
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin legacy: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &legacy, nil); err != nil {
		t.Fatalf("persist legacy: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit legacy: %v", err)
	}

	earlyDayNs := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC).UnixNano()
	if err := store.rollupImportedDays(ctx, earlyDayNs, earlyDayNs); err != nil {
		t.Fatalf("rollupImportedDays: %v", err)
	}

	var requests int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT requests FROM day_bucket WHERE bucket_ts_ns = ?",
		earlyDayNs,
	).Scan(&requests); err != nil {
		t.Fatalf("query rebuilt day_bucket: %v", err)
	}
	if requests != 1 {
		t.Fatalf("rebuilt day_bucket requests=%d, want 1", requests)
	}

	watermarkNs, hasWatermark, err := store.getDayRollupWatermark(ctx)
	if err != nil {
		t.Fatalf("get watermark: %v", err)
	}
	wantWatermark := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC).UnixNano()
	if !hasWatermark || watermarkNs != wantWatermark {
		t.Fatalf("watermark = (%d, %t), want (%d, true)", watermarkNs, hasWatermark, wantWatermark)
	}
}

// TestQueryTimeSeriesByCredential_PopulatesCost is the regression test for the
// "credential cost line shows as flat zero" bug: queryTimeSeriesByCredential
// was the only timeSeries* path missing SUM(cost_micro). With pricing
// configured the bucket gets a non-zero cost_micro, and the chart switcher
// expects time_series_by_credential[].cost to reflect it.
func TestQueryTimeSeriesByCredential_PopulatesCost(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// $1 per million prompt tokens, $2 per million completion. With 1000+2000
	// tokens this produces a non-zero cost_micro that round-trips through the
	// chart payload.
	priceFn := func(model string) (prompt, completion, cache float64, found bool) {
		return 1.0, 2.0, 0.0, true
	}

	d := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Model:     "m",
		APIKey:    "k",
		Source:    "cred-a",
		Tokens:    tokenStats{InputTokens: 1000, OutputTokens: 2000, TotalTokens: 3000},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &d, priceFn); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	qr, err := store.querySummary(ctx, from, to, EventFilters{}, "hourly", true)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	credSeries, ok := qr.TimeSeriesByCredential["cred-a"]
	if !ok || len(credSeries) == 0 {
		t.Fatalf("expected time series for cred-a, got %#v", qr.TimeSeriesByCredential)
	}
	var totalCost float64
	for _, tp := range credSeries {
		totalCost += tp.Cost
	}
	if totalCost <= 0 {
		t.Fatalf(
			"expected non-zero cost in time_series_by_credential, got %v across %d points", totalCost, len(credSeries),
		)
	}
}

// BenchmarkStoreQuerySummaryAndEvents records a reproducible read-path baseline
// for the SQLite summary/events queries across a 180-day dataset, so storage
// changes can be judged against a concrete latency budget instead of hand-wavy
// claims.
func BenchmarkStoreQuerySummaryAndEvents(b *testing.B) {
	dir := b.TempDir()
	store, err := openStore(filepath.Join(dir, "events.db"))
	if err != nil {
		b.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	priceFn := func(model string) (prompt, completion, cache float64, found bool) {
		return 1.0, 2.0, 0.5, true
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for day := range 180 {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		for i := range 48 {
			detail := FlatDetail{
				Timestamp: base.Add(time.Duration(day*24+i/2) * time.Hour),
				Model:     []string{"claude-opus-4-6", "gpt-5.4", "claude-sonnet-4-6"}[i%3],
				APIKey:    []string{"k0", "k1", "k2", "k3"}[i%4],
				Source:    []string{"cred-a", "cred-b", "cred-c", "cred-d", "cred-e"}[i%5],
				Provider:  []string{"claude", "codex"}[i%2],
				RequestID: fmt.Sprintf("benchmark-%03d-%02d", day, i),
				UserAgent: strings.Repeat("metadata-heavy-agent", 32),
				Completed: true,
				Tokens: tokenStats{
					InputTokens:     int64(100 + i),
					OutputTokens:    int64(50 + i),
					CachedTokens:    int64(i % 7),
					ReasoningTokens: int64(i % 5),
					TotalTokens:     int64(150 + 2*i),
				},
				Failed: i%11 == 0,
			}
			if _, err := persistEvent(ctx, tx, store, &detail, priceFn); err != nil {
				b.Fatalf("persist: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
	if err := store.rollupPendingDays(ctx); err != nil {
		b.Fatalf("rollup: %v", err)
	}

	from := base
	to := base.Add(180 * 24 * time.Hour)

	b.Run(
		"summary_180d_filtered", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := store.querySummary(
					ctx, from, to, EventFilters{Model: "claude-opus-4-6", Source: "claude:cred-a", APIKey: "k0"},
					"daily", true,
				); err != nil {
					b.Fatalf("Query: %v", err)
				}
			}
		},
	)

	b.Run(
		"events_30d_filtered", func(b *testing.B) {
			b.ReportAllocs()
			thirtyDaysAgo := to.Add(-30 * 24 * time.Hour)
			for i := 0; i < b.N; i++ {
				if _, _, err := store.queryEventDetails(
					ctx, thirtyDaysAgo, to, EventFilters{Model: "claude-opus-4-6", APIKey: "k0"}, 1, 100, "timestamp",
					true,
				); err != nil {
					b.Fatalf("QueryEvents: %v", err)
				}
			}
		},
	)

	b.Run(
		"events_30d_compact_filtered", func(b *testing.B) {
			b.ReportAllocs()
			thirtyDaysAgo := to.Add(-30 * 24 * time.Hour)
			for i := 0; i < b.N; i++ {
				if _, _, err := store.queryCompactEventDetails(
					ctx, thirtyDaysAgo, to, EventFilters{Model: "claude-opus-4-6", APIKey: "k0"}, 1, 100, "timestamp",
					true,
				); err != nil {
					b.Fatalf("QueryCompactEvents: %v", err)
				}
			}
		},
	)

	b.Run(
		"summary_365d_large_bucketized", func(b *testing.B) {
			largeStore, err := openStore(filepath.Join(b.TempDir(), "events.db"))
			if err != nil {
				b.Fatalf("OpenStore large: %v", err)
			}
			defer func() { _ = largeStore.Close() }()
			largeCtx := context.Background()
			largeBase := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			tx, err := largeStore.db.BeginTx(largeCtx, nil)
			if err != nil {
				b.Fatalf("begin large preload: %v", err)
			}
			modelIDs := make([]int64, 16)
			for modelIdx := range modelIDs {
				modelIDs[modelIdx], err = largeStore.resolveModelID(largeCtx, tx, fmt.Sprintf("model-%02d", modelIdx))
				if err != nil {
					b.Fatalf("ResolveModelID: %v", err)
				}
			}
			apiKeyIDs := make([]sql.NullInt64, 16)
			for apiIdx := range apiKeyIDs {
				apiKeyIDs[apiIdx], err = largeStore.resolveAPIKeyID(largeCtx, tx, fmt.Sprintf("key-%02d", apiIdx))
				if err != nil {
					b.Fatalf("ResolveAPIKeyID: %v", err)
				}
			}
			credentialIDs := make([]sql.NullInt64, 2048)
			for credIdx := range credentialIDs {
				credentialIDs[credIdx], err = largeStore.resolveCredentialID(
					largeCtx, tx, fmt.Sprintf("cred-%04d", credIdx), "",
				)
				if err != nil {
					b.Fatalf("ResolveCredentialID: %v", err)
				}
			}
			providerIDs := make([]sql.NullInt64, 4)
			for providerIdx, provider := range []string{"claude", "codex", "openai", "gemini"} {
				providerIDs[providerIdx], err = largeStore.resolveProviderID(largeCtx, tx, provider)
				if err != nil {
					b.Fatalf("ResolveProviderID: %v", err)
				}
			}
			for i := range 365 {
				bucketDay := largeBase.AddDate(0, 0, i)
				for bucketIdx := range 8192 {
					credIdx := bucketIdx % len(credentialIDs)
					modelID := modelIDs[bucketIdx%len(modelIDs)]
					apiKeyID := apiKeyIDs[(bucketIdx/len(credentialIDs))%len(apiKeyIDs)]
					credentialID := credentialIDs[credIdx]
					providerID := providerIDs[credIdx%len(providerIDs)]
					if _, err := tx.ExecContext(
						largeCtx, `INSERT INTO day_bucket (
				    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
				    requests, success, failure,
				    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_micro
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
						bucketDay.UnixNano(),
						modelID,
						nullableID(credentialID),
						nullableID(apiKeyID),
						nullableID(providerID),
						200,
						196,
						4,
						200000,
						120000,
						10000,
						30000,
						360000,
						540000,
					); err != nil {
						b.Fatalf("insert day_bucket: %v", err)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatalf("commit large preload: %v", err)
			}
			largeFrom := largeBase
			largeTo := largeBase.AddDate(1, 0, 0)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := largeStore.querySummary(
					largeCtx, largeFrom, largeTo,
					EventFilters{Model: "model-03", Source: "cred-0007", APIKey: "key-05"}, "daily", true,
				); err != nil {
					b.Fatalf("large Query: %v", err)
				}
			}
		},
	)
}

func TestQuery_HourBucketFloorsFrom(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Event at 10:30 lives in the 10:00 bucket.
	d := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC),
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 100},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// from is mid-bucket (10:15). Without floor, bucket_ts_ns(10:00) < from
	// would exclude the 10:00 bucket entirely.
	from := time.Date(2026, 4, 27, 10, 15, 0, 0, time.UTC)
	to := time.Date(2026, 4, 27, 11, 0, 0, 0, time.UTC)
	qr, err := store.querySummary(ctx, from, to, EventFilters{}, "hourly", true)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if qr.Totals.Requests != 1 {
		t.Fatalf(
			"expected 1 request when from is mid-bucket, got %d (10:00 bucket likely excluded)", qr.Totals.Requests,
		)
	}
}

// TestQuery_DayBucketFloorsFrom is the day_bucket counterpart of the floor
// invariant. With a span beyond the hourly bucket threshold the route flips to day_bucket whose rows are
// keyed by day-start, so a from offset within the day must floor to that day.
func TestQuery_DayBucketFloorsFrom(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Event at 15:00 of a target day — well within the day bucket.
	target := time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC)
	d := FlatDetail{
		Timestamp: target,
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 100},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Roll up so day_bucket has the row.
	stop := store.startRollup(ctx)
	defer stop()

	// from offset 10h within the same day; span beyond the hourly bucket threshold to force day_bucket route.
	from := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	qr, err := store.querySummary(ctx, from, to, EventFilters{}, "daily", true)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if qr.Totals.Requests != 1 {
		t.Fatalf("expected 1 request when from is mid-day, got %d (day bucket likely excluded)", qr.Totals.Requests)
	}
}

// TestNoneSentinel_FilterMatchesNullAPIKey verifies the (none) sentinel maps
// to "api_key_id IS NULL" so the FilterBar can include OAuth-style requests
// in by_api_key filters. Pre-fix the same selection silently dropped them
// because `col IN (...)` cannot match NULL.
func TestNoneSentinel_FilterMatchesNullAPIKey(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, key := range []string{"sk-real", ""} {
		d := FlatDetail{
			Timestamp: base,
			Model:     "m",
			APIKey:    key,
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
			t.Fatalf("persist key=%q: %v", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	qrNone, err := store.querySummary(
		ctx, from, to,
		EventFilters{APIKey: "(none)"}, "hourly", true,
	)
	if err != nil {
		t.Fatalf("Query (none): %v", err)
	}
	if got := qrNone.Totals.Requests; got != 1 {
		t.Fatalf("(none) totals = %d, want 1", got)
	}
	if _, ok := qrNone.ByAPIKey["(none)"]; !ok {
		var keys []string
		for k := range qrNone.ByAPIKey {
			keys = append(keys, k)
		}
		t.Fatalf("expected (none) bucket in ByAPIKey, got %v", keys)
	}

	qrBoth, err := store.querySummary(
		ctx, from, to,
		EventFilters{APIKey: "(none),sk-real"}, "hourly", true,
	)
	if err != nil {
		t.Fatalf("Query (none,sk-real): %v", err)
	}
	if got := qrBoth.Totals.Requests; got != 2 {
		t.Fatalf("(none,sk-real) totals = %d, want 2", got)
	}
}

func TestQueryBackupEventsRejectsOversizedExportBeforeLoadingAllRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := range 4 {
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Model:     "oversized-model-" + strings.Repeat("x", 256),
			APIKey:    fmt.Sprintf("sk-%d", i),
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
			t.Fatalf("persist[%d]: %v", i, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err = store.queryBackupEvents(ctx, time.Time{}, time.Time{}, EventFilters{}, 1024)
	if !errors.Is(err, ErrPortableBackupTooLarge) {
		t.Fatalf("queryBackupEvents() error = %v, want ErrPortableBackupTooLarge", err)
	}
}

func TestAPIKeyFilterHandlesDisplayCollisionsAndLiteralSentinels(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	keys := []string{
		"abcd...wxyz",
		"abcd-secret-wxyz",
		"(none)",
		"",
		"ab,c-first-zzzz",
		"ab,c-second-zzzz",
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, key := range keys {
		detail := FlatDetail{
			Timestamp: base,
			Model:     "m",
			APIKey:    key,
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
			t.Fatalf("persist key=%q: %v", key, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	queries := []struct {
		name       string
		filters    EventFilters
		wantTotals int64
	}{
		{name: "exact raw key", filters: EventFilters{APIKey: "abcd...wxyz"}, wantTotals: 1},
		{name: "literal none and null sentinel", filters: EventFilters{APIKey: "(none)"}, wantTotals: 2},
		{
			name: "expanded masked display collision",
			filters: EventFilters{
				APIKeys: []string{
					coreusage.HashAPIKeyIdentity("abcd...wxyz"),
					coreusage.HashAPIKeyIdentity("abcd-secret-wxyz"),
				},
			},
			wantTotals: 2,
		},
		{
			name: "expanded display containing comma",
			filters: EventFilters{
				APIKeys: []string{
					coreusage.HashAPIKeyIdentity("ab,c-first-zzzz"),
					coreusage.HashAPIKeyIdentity("ab,c-second-zzzz"),
				},
			},
			wantTotals: 2,
		},
	}
	for _, tc := range queries {
		t.Run(
			tc.name, func(t *testing.T) {
				result, errQuery := store.querySummary(ctx, from, to, tc.filters, "hourly", true)
				if errQuery != nil {
					t.Fatalf("Query(%q): %v", tc.name, errQuery)
				}
				if result.Totals.Requests != tc.wantTotals {
					t.Fatalf("Query(%q) totals = %d, want %d", tc.name, result.Totals.Requests, tc.wantTotals)
				}
			},
		)
	}

	repeated := []string{
		coreusage.HashAPIKeyIdentity("ab,c-first-zzzz"),
		coreusage.HashAPIKeyIdentity("ab,c-second-zzzz"),
		coreusage.HashAPIKeyIdentity("abcd...wxyz"),
		coreusage.HashAPIKeyIdentity("abcd-secret-wxyz"),
	}
	result, err := store.querySummary(
		ctx,
		from,
		to,
		EventFilters{APIKeys: repeated},
		"hourly",
		true,
	)
	if err != nil {
		t.Fatalf("Query(repeated API keys): %v", err)
	}
	if result.Totals.Requests != 4 {
		t.Fatalf("Query(repeated API keys) totals = %d, want 4", result.Totals.Requests)
	}

	singleRepeated := []string{
		coreusage.HashAPIKeyIdentity("ab,c-first-zzzz"),
		coreusage.HashAPIKeyIdentity("ab,c-second-zzzz"),
	}
	result, err = store.querySummary(
		ctx,
		from,
		to,
		EventFilters{APIKeys: singleRepeated},
		"hourly",
		true,
	)
	if err != nil {
		t.Fatalf("Query(single repeated API key): %v", err)
	}
	if result.Totals.Requests != 2 {
		t.Fatalf("Query(single repeated API key) totals = %d, want 2", result.Totals.Requests)
	}

	result, err = store.querySummary(
		ctx,
		from,
		to,
		EventFilters{APIKeys: []string{"", "  "}},
		"hourly",
		true,
	)
	if err != nil {
		t.Fatalf("Query(empty repeated API keys): %v", err)
	}
	if result.Totals.Requests != int64(len(keys)) {
		t.Fatalf("Query(empty repeated API keys) totals = %d, want %d", result.Totals.Requests, len(keys))
	}
}
