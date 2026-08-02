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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

// TestStore_QueryStaysWithinMemoryBudget is the regression test for the
// 2026-04-26 LA OOM. The pre-SQLite path read 60MB+ of detail JSONs into
// memory (5-10x amplified by Go's JSON unmarshal) when any dim filter was
// active. With SQLite, every read is a SQL aggregate and the working set
// never depends on dataset size — this test asserts that even a sizeable
// query keeps HeapAlloc below 50MB above the baseline.
//
// 50 events is small but the test serves a different purpose: it exercises
// every read path (Query, QueryEvents, SnapshotFiltered) to catch the case
// where someone reintroduces "read-then-slice" by accident. A larger event
// count is left for benchmarks; this is a guard, not a load test.
func TestStore_QueryStaysWithinMemoryBudget(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "events.db"))
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
	for i := 0; i < 50; i++ {
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
	if _, err := store.Query(ctx, from, to, EventFilters{Model: "m1,m2", APIKey: "k0"}, "hourly", true); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, _, err := store.QueryEvents(ctx, from, to, EventFilters{}, 1, 100, "timestamp", false); err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	// QuerySnapshotFiltered rebuilds the detail-heavy v1 shape used by legacy /usage.
	p := &Persister{store: store}
	if _, err := p.QuerySnapshotFiltered(from, to, EventFilters{}); err != nil {
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

// TestStartRollup_DayBucketReadyOnReturn locks in the post-incident invariant
// that StartRollup must finish the initial rollupAllDays pass synchronously.
// If the initial pass were async (the original implementation), a /usage/summary
// request hitting the >90d branch right after server start would read an empty
// day_bucket and the user would see a blank dashboard until the goroutine
// caught up.
//
// The test asserts day_bucket transitions from 0 → ≥1 rows during the
// StartRollup call. Async rollup would leave the count at 0 in nearly all
// scheduling timelines and fail the test deterministically.
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
	snap, err := p.QuerySnapshotFiltered(ts.Add(-time.Minute), ts.Add(time.Minute), EventFilters{})
	if err != nil {
		t.Fatalf("QuerySnapshotFiltered: %v", err)
	}
	detail := snap.APIs["k"].Models["m"].Details[0]
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
	snap, err := p.QuerySnapshotFiltered(
		base.Add(20*time.Minute),
		base.Add(40*time.Minute),
		EventFilters{},
	)
	if err != nil {
		t.Fatalf("QuerySnapshotFiltered: %v", err)
	}
	if snap.TotalRequests != 1 || snap.SuccessCount != 0 || snap.FailureCount != 1 || snap.TotalTokens != 2 {
		t.Fatalf("totals = requests:%d success:%d failure:%d tokens:%d, want 1/0/1/2", snap.TotalRequests, snap.SuccessCount, snap.FailureCount, snap.TotalTokens)
	}
	if snap.RequestsByDay["2026-05-20"] != 1 || snap.RequestsByHour["10"] != 1 || snap.TokensByDay["2026-05-20"] != 2 || snap.TokensByHour["10"] != 2 {
		t.Fatalf("time maps = %#v %#v %#v %#v", snap.RequestsByDay, snap.RequestsByHour, snap.TokensByDay, snap.TokensByHour)
	}
	api := snap.APIs["k"]
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

	if _, err = store.db.ExecContext(ctx, "UPDATE events SET metadata = '{}' WHERE ts_ns = ?", timestamp.UnixNano()); err != nil {
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
	if _, err = p.QuerySnapshot(); !errors.Is(err, ErrLegacySnapshotTooLarge) {
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
		TimeSeries: []timePoint{{
			Time:     "1969-12-31T23:00:00Z",
			Requests: 1,
		}},
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
	if _, err := store.db.ExecContext(ctx, "UPDATE events SET metadata = '{' WHERE ts_ns = ?", ts.UnixNano()); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}

	p := &Persister{store: store}
	_, err = p.QuerySnapshotFiltered(ts.Add(-time.Minute), ts.Add(time.Minute), EventFilters{})
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
	if _, err := p.QuerySnapshotFiltered(time.Time{}, time.Time{}, EventFilters{}); err == nil {
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
	_, err = p.QuerySnapshotFiltered(time.Time{}, time.Time{}, EventFilters{})
	if err == nil || !strings.Contains(err.Error(), "snapshot token aggregate overflows int64") {
		t.Fatalf("QuerySnapshotFiltered overflow error = %v", err)
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

	stop := store.StartRollup(ctx)
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
		t.Run(raw, func(t *testing.T) {
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
		})
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
		t.Fatalf("next day = (%d, %t), want (%d, true)", next, hasNext, int64(nsPerDay))
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
	err := store.RollupDay(context.Background(), math.MaxInt64-nsPerDay+1)
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
	if err := store.RollupDay(ctx, dayStart.UnixNano()); err != nil {
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
	store, err := OpenStore(dbPath)
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
	if err := store.RollupDay(ctx, dayStart.UnixNano()); err != nil {
		t.Fatalf("initial RollupDay: %v", err)
	}
	persist(dayStart.Add(11 * time.Hour))
	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	store, err = OpenStore(dbPath)
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
	qr, err := store.Query(ctx, from, to, EventFilters{}, "hourly", true)
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
	store, err := OpenStore(filepath.Join(dir, "events.db"))
	if err != nil {
		b.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	priceFn := func(model string) (prompt, completion, cache float64, found bool) {
		return 1.0, 2.0, 0.5, true
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for day := 0; day < 180; day++ {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		for i := 0; i < 48; i++ {
			detail := FlatDetail{
				Timestamp: base.Add(time.Duration(day*24+i/2) * time.Hour),
				Model:     []string{"claude-opus-4-6", "gpt-5.4", "claude-sonnet-4-6"}[i%3],
				APIKey:    []string{"k0", "k1", "k2", "k3"}[i%4],
				Source:    []string{"cred-a", "cred-b", "cred-c", "cred-d", "cred-e"}[i%5],
				Provider:  []string{"claude", "codex"}[i%2],
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
				if _, err := store.Query(
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
				if _, _, err := store.QueryEvents(
					ctx, thirtyDaysAgo, to, EventFilters{Model: "claude-opus-4-6", APIKey: "k0"}, 1, 100, "timestamp",
					true,
				); err != nil {
					b.Fatalf("QueryEvents: %v", err)
				}
			}
		},
	)

	b.Run(
		"summary_365d_large_bucketized", func(b *testing.B) {
			largeStore, err := OpenStore(filepath.Join(b.TempDir(), "events.db"))
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
			for modelIdx := 0; modelIdx < len(modelIDs); modelIdx++ {
				modelIDs[modelIdx], err = largeStore.ResolveModelID(largeCtx, tx, fmt.Sprintf("model-%02d", modelIdx))
				if err != nil {
					b.Fatalf("ResolveModelID: %v", err)
				}
			}
			apiKeyIDs := make([]sql.NullInt64, 16)
			for apiIdx := 0; apiIdx < len(apiKeyIDs); apiIdx++ {
				apiKeyIDs[apiIdx], err = largeStore.ResolveAPIKeyID(largeCtx, tx, fmt.Sprintf("key-%02d", apiIdx))
				if err != nil {
					b.Fatalf("ResolveAPIKeyID: %v", err)
				}
			}
			credentialIDs := make([]sql.NullInt64, 2048)
			for credIdx := 0; credIdx < len(credentialIDs); credIdx++ {
				credentialIDs[credIdx], err = largeStore.ResolveCredentialID(
					largeCtx, tx, fmt.Sprintf("cred-%04d", credIdx), "",
				)
				if err != nil {
					b.Fatalf("ResolveCredentialID: %v", err)
				}
			}
			providerIDs := make([]sql.NullInt64, 4)
			for providerIdx, provider := range []string{"claude", "codex", "openai", "gemini"} {
				providerIDs[providerIdx], err = largeStore.ResolveProviderID(largeCtx, tx, provider)
				if err != nil {
					b.Fatalf("ResolveProviderID: %v", err)
				}
			}
			for i := 0; i < 365; i++ {
				bucketDay := largeBase.AddDate(0, 0, i)
				for bucketIdx := 0; bucketIdx < 8192; bucketIdx++ {
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
				if _, err := largeStore.Query(
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
	qr, err := store.Query(ctx, from, to, EventFilters{}, "hourly", true)
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
// invariant. With a span > 90d the route flips to day_bucket whose rows are
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
	stop := store.StartRollup(ctx)
	defer stop()

	// from offset 10h within the same day; span > 90d to force day_bucket route.
	from := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	qr, err := store.Query(ctx, from, to, EventFilters{}, "daily", true)
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

	qrNone, err := store.Query(
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

	qrBoth, err := store.Query(
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
	for i := 0; i < 4; i++ {
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
		filter     string
		wantTotals int64
	}{
		{name: "exact raw and masked display", filter: "abcd...wxyz", wantTotals: 2},
		{name: "literal none and null sentinel", filter: "(none)", wantTotals: 2},
		{name: "masked display containing comma", filter: util.HideAPIKey("ab,c-first-zzzz"), wantTotals: 2},
	}
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			result, errQuery := store.Query(
				ctx,
				from,
				to,
				EventFilters{APIKey: tc.filter},
				"hourly",
				true,
			)
			if errQuery != nil {
				t.Fatalf("Query(%q): %v", tc.filter, errQuery)
			}
			if result.Totals.Requests != tc.wantTotals {
				t.Fatalf("Query(%q) totals = %d, want %d", tc.filter, result.Totals.Requests, tc.wantTotals)
			}
		})
	}

	repeated := []string{util.HideAPIKey("ab,c-first-zzzz"), "abcd...wxyz"}
	result, err := store.Query(
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

	singleRepeated := []string{util.HideAPIKey("ab,c-first-zzzz")}
	result, err = store.Query(
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

	result, err = store.Query(
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
