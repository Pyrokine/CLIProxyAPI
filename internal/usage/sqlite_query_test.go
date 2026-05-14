// Last compiled: 2026-05-07
// Author: pyro

package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
	defer store.Close()

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
	// SnapshotFiltered rebuilds v1 nested — the path /usage/export uses.
	p := &Persister{store: store}
	_ = p.SnapshotFiltered(from, to, EventFilters{})

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

	watermarkNs, err := store.getDayRollupWatermark(ctx)
	if err != nil {
		t.Fatalf("get watermark: %v", err)
	}
	wantWatermark := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC).UnixNano()
	if watermarkNs != wantWatermark {
		t.Fatalf("watermark moved backward to %d, want %d", watermarkNs, wantWatermark)
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
			for i := 0; i < 365; i++ {
				bucketDay := largeBase.AddDate(0, 0, i)
				for modelIdx := 0; modelIdx < 16; modelIdx++ {
					modelID, err := largeStore.ResolveModelID(largeCtx, tx, fmt.Sprintf("model-%02d", modelIdx))
					if err != nil {
						b.Fatalf("ResolveModelID: %v", err)
					}
					for apiIdx := 0; apiIdx < 16; apiIdx++ {
						apiKeyID, err := largeStore.ResolveAPIKeyID(largeCtx, tx, fmt.Sprintf("key-%02d", apiIdx))
						if err != nil {
							b.Fatalf("ResolveAPIKeyID: %v", err)
						}
						for credIdx := 0; credIdx < 32; credIdx++ {
							credID, err := largeStore.ResolveCredentialID(
								largeCtx, tx, fmt.Sprintf("cred-%03d", credIdx), "",
							)
							if err != nil {
								b.Fatalf("ResolveCredentialID: %v", err)
							}
							providerID, err := largeStore.ResolveProviderID(
								largeCtx, tx, []string{"claude", "codex", "openai", "gemini"}[credIdx%4],
							)
							if err != nil {
								b.Fatalf("ResolveProviderID: %v", err)
							}
							if _, err := tx.ExecContext(
								largeCtx, `INSERT INTO day_bucket (
						    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
						    requests, success, failure,
						    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_micro
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
								bucketDay.UnixNano(),
								modelID,
								nullableID(credID),
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
					largeCtx, largeFrom, largeTo, EventFilters{Model: "model-03", Source: "cred-007", APIKey: "key-05"},
					"daily", true,
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
