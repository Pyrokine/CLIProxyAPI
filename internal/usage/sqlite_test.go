package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openTestStore opens a Store rooted at t.TempDir() so each test gets a clean
// SQLite database and the WAL files clean up automatically.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestOpenStore_Idempotent verifies that opening an already-existing database
// works and the schema_version meta row is preserved across opens.
func TestOpenStore_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")

	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("first OpenStore: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("second OpenStore: %v", err)
	}
	defer func() {
		if errClose := store2.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()

	var v string
	if err := store2.db.QueryRow("SELECT value FROM meta WHERE key = 'schema_version'").Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v == "" {
		t.Fatalf("expected non-empty schema_version after re-open")
	}
}

// TestResolveDimIDs verifies dim_* tables INSERT-OR-IGNORE plus return-on-
// existing-id semantics for both required (model) and nullable (api_key,
// source, auth_index, credential) dimensions.
func TestOpenStoreRejectsFutureSchemaWithoutMutatingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := raw.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatalf("set journal mode: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
		INSERT INTO meta(key, value) VALUES ('schema_version', '5');
		CREATE TABLE future_only (id INTEGER PRIMARY KEY, payload TEXT NOT NULL);
	`); err != nil {
		t.Fatalf("create future schema: %v", err)
	}

	snapshotSchema := func(db *sql.DB) string {
		t.Helper()
		rows, err := db.Query(`
			SELECT type, name, tbl_name, ifnull(sql, '')
			  FROM sqlite_master
			 ORDER BY type, name
		`)
		if err != nil {
			t.Fatalf("query sqlite_master: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var entries []string
		for rows.Next() {
			var entryType, name, table, statement string
			if err := rows.Scan(&entryType, &name, &table, &statement); err != nil {
				t.Fatalf("scan sqlite_master: %v", err)
			}
			entries = append(entries, strings.Join([]string{entryType, name, table, statement}, "\x00"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate sqlite_master: %v", err)
		}
		return strings.Join(entries, "\n")
	}
	readJournalMode := func(db *sql.DB) string {
		t.Helper()
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("read journal mode: %v", err)
		}
		return mode
	}

	beforeSchema := snapshotSchema(raw)
	beforeJournalMode := readJournalMode(raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	store, err := OpenStore(dbPath)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenStore returned a store for future schema")
	}
	if err == nil || !strings.Contains(err.Error(), "unexpected schema_version=5") {
		t.Fatalf("OpenStore error = %v, want future schema rejection", err)
	}

	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen raw database: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if after := snapshotSchema(raw); after != beforeSchema {
		t.Fatalf("sqlite_master changed after rejection\nbefore:\n%s\nafter:\n%s", beforeSchema, after)
	}
	if after := readJournalMode(raw); after != beforeJournalMode {
		t.Fatalf("journal_mode = %q after rejection, want %q", after, beforeJournalMode)
	}
}

func TestResolveDimIDs(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Required dim: empty string is rejected by callers, so we only test the
	// happy path here.
	id1, err := store.ResolveModelID(ctx, tx, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("resolve model 1: %v", err)
	}
	id2, err := store.ResolveModelID(ctx, tx, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("resolve model 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id on repeat resolve: %d vs %d", id1, id2)
	}
	if id1 == 0 {
		t.Fatalf("expected non-zero id")
	}

	// Nullable dim: empty string returns NullInt64{Valid: false} without
	// inserting a row.
	for _, fn := range []func(context.Context, dbExec, string) (sql.NullInt64, error){
		store.ResolveAPIKeyID,
		store.ResolveSourceID,
		store.ResolveAuthIndexID,
	} {
		got, err := fn(ctx, tx, "")
		if err != nil {
			t.Fatalf("resolve nullable empty: %v", err)
		}
		if got.Valid {
			t.Fatalf("expected NullInt64 invalid for empty input, got valid id %d", got.Int64)
		}
	}

	// ResolveCredentialID: takes (source, authIndex). Both empty -> NULL.
	got, err := store.ResolveCredentialID(ctx, tx, "", "")
	if err != nil {
		t.Fatalf("resolve credential empty: %v", err)
	}
	if got.Valid {
		t.Fatalf("expected NullInt64 invalid for empty credential, got valid id %d", got.Int64)
	}
}

func TestResolveDimIDsRollbackDoesNotPopulateCache(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const model = "rolled-back-model"

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.ResolveModelID(ctx, tx, model); err != nil {
		_ = tx.Rollback()
		t.Fatalf("resolve model in transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, ok := store.modelCache.Load(model); ok {
		t.Fatalf("model cache contains %q after rollback", model)
	}

	id, err := store.ResolveModelID(ctx, store.db, model)
	if err != nil {
		t.Fatalf("resolve model after rollback: %v", err)
	}
	if id == 0 {
		t.Fatal("resolved model id is zero")
	}
	cached, ok := store.modelCache.Load(model)
	if !ok || cached != id {
		t.Fatalf("model cache = (%v, %t), want (%d, true)", cached, ok, id)
	}
}

// TestPersistEvent_Idempotent verifies INSERT OR IGNORE on events.fingerprint
// dedupes a second insert of the same logical event, and the hour_bucket is
// only incremented on the first insert.
func TestPersistEvent_Idempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	d := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC),
		Model:     "claude-sonnet-4-6",
		APIKey:    "fb",
		Source:    "claude_oauth",
		Failed:    false,
		Tokens:    tokenStats{InputTokens: 100, OutputTokens: 200, TotalTokens: 300},
	}

	for round, expectAdded := range []bool{true, false} {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("round %d begin: %v", round, err)
		}
		added, err := persistEvent(ctx, tx, store, &d, nil)
		if err != nil {
			t.Fatalf("round %d persist: %v", round, err)
		}
		if added != expectAdded {
			t.Fatalf("round %d added=%v want %v", round, added, expectAdded)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("round %d commit: %v", round, err)
		}
	}

	var count int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT requests FROM hour_bucket WHERE model_id = (SELECT id FROM dim_model WHERE name = ?)",
		d.Model,
	).Scan(&count); err != nil {
		t.Fatalf("read hour_bucket: %v", err)
	}
	if count != 1 {
		t.Fatalf("hour_bucket.requests=%d after dup, want 1", count)
	}
}

func TestPersistEventBucketOverflowRollsBackEventAndDimensions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	seed := FlatDetail{
		Timestamp: base,
		Model:     "overflow-model",
		APIKey:    "overflow-key",
		Source:    "shared-source",
		Provider:  "overflow-provider",
		Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &seed, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		"UPDATE hour_bucket SET total_tokens = ?",
		int64(math.MaxInt64),
	); err != nil {
		t.Fatalf("set bucket boundary: %v", err)
	}

	bad := seed
	bad.Timestamp = base.Add(time.Minute)
	bad.AuthIndex = "rolled-back-auth-index"
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin overflow event: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &bad, nil); err == nil ||
		!strings.Contains(err.Error(), "integer overflow") {
		_ = tx.Rollback()
		t.Fatalf("persist overflow error = %v, want integer overflow", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback overflow event: %v", err)
	}

	var eventCount, authIndexCount int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM dim_auth_index WHERE name = ?",
		bad.AuthIndex,
	).Scan(&authIndexCount); err != nil {
		t.Fatalf("count rolled-back auth index: %v", err)
	}
	if eventCount != 1 || authIndexCount != 0 {
		t.Fatalf("after rollback events=%d auth index dimensions=%d, want 1/0", eventCount, authIndexCount)
	}
}

func TestComputeCostMicroRejectsInvalidAndOutOfRangeValues(t *testing.T) {
	tests := []struct {
		name       string
		tokens     tokenStats
		prompt     float64
		completion float64
		cache      float64
	}{
		{name: "prompt NaN", prompt: math.NaN()},
		{name: "completion positive infinity", completion: math.Inf(1)},
		{name: "cache negative infinity", cache: math.Inf(-1)},
		{name: "negative price", prompt: -1},
		{name: "exactly 2^63", tokens: tokenStats{InputTokens: 1}, prompt: math.Exp2(63)},
		{name: "multiplication overflow", tokens: tokenStats{InputTokens: math.MaxInt64}, prompt: math.MaxFloat64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			priceFn := func(string) (float64, float64, float64, bool) {
				return test.prompt, test.completion, test.cache, true
			}
			if _, err := computeCostMicro(test.tokens, priceFn, "m"); err == nil {
				t.Fatal("computeCostMicro returned nil error")
			}
		})
	}

	belowLimit := math.Nextafter(math.Exp2(63), 0)
	priceFn := func(string) (float64, float64, float64, bool) {
		return belowLimit, 0, 0, true
	}
	cost, err := computeCostMicro(tokenStats{InputTokens: 1}, priceFn, "m")
	if err != nil {
		t.Fatalf("computeCostMicro below limit: %v", err)
	}
	if cost <= 0 || cost >= math.MaxInt64 {
		t.Fatalf("cost below limit = %d", cost)
	}
}

func TestWriterFlushUsesOnePriceSnapshotPerBatch(t *testing.T) {
	store := openTestStore(t)
	priceFor := func(prompt float64) PriceFunc {
		return func(string) (float64, float64, float64, bool) {
			return prompt, 0, 0, true
		}
	}
	provider := &scriptedPriceProvider{snapshots: []PriceFunc{priceFor(1), priceFor(2)}}
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	base := time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC)

	if err := writer.flush([]FlatDetail{
		{Timestamp: base, Model: "m1", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1}},
		{Timestamp: base.Add(time.Minute), Model: "m2", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1}},
	}); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if got := provider.CallCount(); got != 1 {
		t.Fatalf("Snapshot calls after first batch = %d, want 1", got)
	}
	if err := writer.flush([]FlatDetail{
		{Timestamp: base.Add(time.Hour), Model: "m1", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1}},
	}); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if got := provider.CallCount(); got != 2 {
		t.Fatalf("Snapshot calls after second batch = %d, want 2", got)
	}

	rows, err := store.db.Query("SELECT cost_micro FROM events ORDER BY ts_ns")
	if err != nil {
		t.Fatalf("query event costs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var costs []int64
	for rows.Next() {
		var cost int64
		if err := rows.Scan(&cost); err != nil {
			t.Fatalf("scan event cost: %v", err)
		}
		costs = append(costs, cost)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event costs: %v", err)
	}
	want := []int64{1, 1, 2}
	if len(costs) != len(want) {
		t.Fatalf("event costs = %v, want %v", costs, want)
	}
	for index := range want {
		if costs[index] != want[index] {
			t.Fatalf("event costs = %v, want %v", costs, want)
		}
	}
}

func TestWriterFlushIsolatesBadRecord(t *testing.T) {
	store := openTestStore(t)
	priceFn := func(model string) (float64, float64, float64, bool) {
		if model == "bad-model" {
			return math.NaN(), 0, 0, true
		}
		return 1, 0, 0, true
	}
	provider := &scriptedPriceProvider{snapshots: []PriceFunc{priceFn}}
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	batch := []FlatDetail{
		{Timestamp: base, Model: "good-model-1", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1}},
		{Timestamp: base.Add(time.Minute), Model: "bad-model", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1}},
		{Timestamp: base.Add(2 * time.Minute), Model: "good-model-2", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1}},
	}

	err := writer.flush(batch)
	if err == nil || !strings.Contains(err.Error(), "1 of 3 records were rejected") {
		t.Fatalf("flush error = %v", err)
	}
	if got := provider.CallCount(); got != 1 {
		t.Fatalf("Snapshot calls during batch fallback = %d, want 1", got)
	}
	var eventCount, bucketRequests int64
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := store.db.QueryRow("SELECT COALESCE(SUM(requests), 0) FROM hour_bucket").Scan(&bucketRequests); err != nil {
		t.Fatalf("sum bucket requests: %v", err)
	}
	if eventCount != 2 || bucketRequests != 2 {
		t.Fatalf("persisted events=%d bucket requests=%d, want 2/2", eventCount, bucketRequests)
	}
	var badDimensions int64
	if err := store.db.QueryRow(
		"SELECT COUNT(*) FROM dim_model WHERE name = 'bad-model'",
	).Scan(&badDimensions); err != nil {
		t.Fatalf("count bad model dimensions: %v", err)
	}
	if badDimensions != 0 {
		t.Fatalf("bad model dimensions=%d, want 0", badDimensions)
	}
}

func TestWriterRetriesBatchAfterTransientFlushFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	initial, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := initial.Close(); err != nil {
		t.Fatalf("close initial store: %v", err)
	}

	writerDB, err := sql.Open(
		"sqlite",
		dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(20)",
	)
	if err != nil {
		t.Fatalf("open writer database: %v", err)
	}
	defer func() { _ = writerDB.Close() }()
	writerDB.SetMaxOpenConns(1)
	store := &Store{db: writerDB}

	lockerDB, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(20)")
	if err != nil {
		t.Fatalf("open locker database: %v", err)
	}
	defer func() { _ = lockerDB.Close() }()
	lockerDB.SetMaxOpenConns(1)
	ctx := context.Background()
	locker, err := lockerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("open locker connection: %v", err)
	}
	defer func() { _ = locker.Close() }()
	if _, err := locker.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = locker.ExecContext(ctx, "ROLLBACK")
		}
	}()

	provider := &scriptedPriceProvider{snapshots: []PriceFunc{
		func(string) (float64, float64, float64, bool) {
			return 1, 0, 0, true
		},
	}}
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	writer.Start(ctx)
	writer.Submit(FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Model:     "m",
		APIKey:    "k",
		Tokens:    tokenStats{TotalTokens: 1},
	})

	time.Sleep(3 * writerFlushInterval)
	snapshotDeadline := time.Now().Add(time.Second)
	for provider.CallCount() < 2 && time.Now().Before(snapshotDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := provider.CallCount(); got < 2 {
		t.Fatalf("Snapshot calls during transient retries = %d, want at least 2", got)
	}
	var count int64
	if err := writerDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		t.Fatalf("count while locked: %v", err)
	}
	if count != 0 {
		t.Fatalf("events while locked = %d, want 0", count)
	}
	if _, err := locker.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("release writer lock: %v", err)
	}
	locked = false

	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := writerDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&count); err != nil {
			t.Fatalf("count after unlock: %v", err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted record was not persisted after transient lock cleared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	writer.Stop()
	if got := provider.CallCount(); got < 2 {
		t.Fatalf("Snapshot calls after transient retry succeeded = %d, want at least 2", got)
	}

	var requests int64
	if err := writerDB.QueryRowContext(
		ctx,
		"SELECT COALESCE(SUM(requests), 0) FROM hour_bucket",
	).Scan(&requests); err != nil {
		t.Fatalf("sum bucket requests: %v", err)
	}
	if requests != 1 {
		t.Fatalf("bucket requests = %d, want 1", requests)
	}
}

// TestQueryFiltered_ZeroMatchNoFallback verifies the critical post-incident
// invariant: an active filter that matches zero dim ids returns an empty
// result, NEVER falls back to all rows.
func TestQueryLatencyStatsFromBuckets(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i, latencyMs := range []int64{100, 300, 0} {
		d := FlatDetail{
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Model:     "m",
			APIKey:    "k",
			LatencyMs: latencyMs,
			Tokens:    tokenStats{TotalTokens: int64(i + 1)},
		}
		if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
			t.Fatalf("persist[%d]: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	qr, err := store.Query(ctx, base.Add(-time.Hour), base.Add(time.Hour), EventFilters{}, "hourly", true)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if qr.Totals.Latency.TotalMs != 400 || qr.Totals.Latency.SampleCount != 2 {
		t.Fatalf("totals latency = %+v, want total 400 sample 2", qr.Totals.Latency)
	}
	model := qr.ByModel["m"]
	if model == nil || model.Latency.TotalMs != 400 || model.Latency.SampleCount != 2 {
		t.Fatalf("model latency = %+v, want total 400 sample 2", model)
	}
	apiKey := qr.ByAPIKey["k"]
	if apiKey == nil || apiKey.Latency.TotalMs != 400 || apiKey.Latency.SampleCount != 2 {
		t.Fatalf("api key latency = %+v, want total 400 sample 2", apiKey)
	}

	dayStart := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC).UnixNano()
	if err := store.RollupDay(ctx, dayStart); err != nil {
		t.Fatalf("RollupDay: %v", err)
	}
	var dayLatency, daySamples int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT latency_ms_total, latency_samples FROM day_bucket WHERE bucket_ts_ns = ?",
		dayStart,
	).Scan(&dayLatency, &daySamples); err != nil {
		t.Fatalf("query day latency: %v", err)
	}
	if dayLatency != 400 || daySamples != 2 {
		t.Fatalf("day latency = (%d, %d), want (400, 2)", dayLatency, daySamples)
	}
}

func TestQueryFiltered_ZeroMatchNoFallback(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert one event so a no-filter query would return non-zero.
	d := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Model:     "real-model",
		APIKey:    "real-key",
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

	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)

	// Filter with a model that doesn't exist -> totals must be zero.
	qr, err := store.Query(ctx, from, to, EventFilters{Model: "ghost-model"}, "hourly", true)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if qr.Totals.Requests != 0 {
		t.Fatalf("expected 0 requests for ghost filter, got %d", qr.Totals.Requests)
	}

	// Sanity check: no filter should still see the inserted event.
	qrAll, err := store.Query(ctx, from, to, EventFilters{}, "hourly", true)
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if qrAll.Totals.Requests != 1 {
		t.Fatalf("expected 1 request without filter, got %d", qrAll.Totals.Requests)
	}
}

// TestQueryEvents_Pagination verifies SQL LIMIT/OFFSET pagination returns the
// correct slice and total count for a known dataset.
func TestQueryEvents_Pagination(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert 25 events, one per minute.
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < 25; i++ {
		d := FlatDetail{
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Model:     "m",
			Tokens:    tokenStats{TotalTokens: int64(i)},
		}
		if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
			t.Fatalf("persist[%d]: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	// Page 1, size 10.
	events, total, err := store.QueryEvents(ctx, from, to, EventFilters{}, 1, 10, "timestamp", false)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 25 {
		t.Fatalf("total=%d want 25", total)
	}
	if len(events) != 10 {
		t.Fatalf("page 1 len=%d want 10", len(events))
	}

	// Page 3 should have 5 leftovers.
	events3, _, err := store.QueryEvents(ctx, from, to, EventFilters{}, 3, 10, "timestamp", false)
	if err != nil {
		t.Fatalf("QueryEvents page 3: %v", err)
	}
	if len(events3) != 5 {
		t.Fatalf("page 3 len=%d want 5", len(events3))
	}
}

// TestComputeFingerprint_NULLProviderEquivalent locks in the v2 compatibility
// guarantee: for legacy rows where Provider == "", ComputeFingerprint must
// produce the exact SHA-1 the v1 11-field algorithm did. Otherwise a
// re-import of pre-v2 JSON would explode events.fingerprint UNIQUE collisions
// against rows already present from previous imports.
func TestPersistEvent_ResponseHeadersRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	budget := new(int)
	*budget = 8192
	d := FlatDetail{
		Timestamp:           time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC),
		Model:               "m",
		LatencyMs:           1234,
		ReasoningEffort:     "medium",
		ServiceTier:         "flex",
		RequestServiceTier:  "flex",
		ResponseServiceTier: "default",
		ExecutorType:        "OpenAICompatExecutor",
		Thinking:            &Thinking{Mode: "budget", Budget: budget},
		ResponseHeaders: http.Header{
			"X-Request-Id":         []string{"req-1"},
			"Openai-Processing-Ms": []string{"42"},
		},
		Tokens: tokenStats{TotalTokens: 1},
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

	events, total, err := store.QueryEvents(
		ctx, d.Timestamp.Add(-time.Minute), d.Timestamp.Add(time.Minute), EventFilters{}, 1, 10, "timestamp", false,
	)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("events len=%d total=%d, want 1/1", len(events), total)
	}
	if got := events[0].ResponseHeaders.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("X-Request-Id = %q, want req-1", got)
	}
	if got := events[0].ResponseHeaders.Get("Openai-Processing-Ms"); got != "42" {
		t.Fatalf("Openai-Processing-Ms = %q, want 42", got)
	}
	if got := events[0].LatencyMs; got != 1234 {
		t.Fatalf("LatencyMs = %d, want 1234", got)
	}
	if got := events[0].ReasoningEffort; got != "medium" {
		t.Fatalf("ReasoningEffort = %q, want medium", got)
	}
	if got := events[0].ServiceTier; got != "flex" {
		t.Fatalf("ServiceTier = %q, want flex", got)
	}
	if got := events[0].RequestServiceTier; got != "flex" {
		t.Fatalf("RequestServiceTier = %q, want flex", got)
	}
	if got := events[0].ResponseServiceTier; got != "default" {
		t.Fatalf("ResponseServiceTier = %q, want default", got)
	}
	if got := events[0].ExecutorType; got != "OpenAICompatExecutor" {
		t.Fatalf("ExecutorType = %q, want OpenAICompatExecutor", got)
	}
	if events[0].Thinking == nil || events[0].Thinking.Budget == nil || *events[0].Thinking.Budget != 8192 {
		t.Fatalf("Thinking = %#v, want budget 8192", events[0].Thinking)
	}
}

func TestDecodeEventMetadataLegacyServiceTierFallback(t *testing.T) {
	var detail FlatDetail
	if err := decodeEventMetadata(
		&detail,
		sql.NullString{String: `{"service_tier":"priority"}`, Valid: true},
	); err != nil {
		t.Fatalf("decodeEventMetadata: %v", err)
	}
	if detail.ServiceTier != "priority" || detail.RequestServiceTier != "priority" {
		t.Fatalf("request tiers = (%q, %q), want priority", detail.ServiceTier, detail.RequestServiceTier)
	}
	if detail.ResponseServiceTier != "" {
		t.Fatalf("response tier = %q, want empty", detail.ResponseServiceTier)
	}
}

func TestWriterSubmitBlocksInsteadOfDropping(t *testing.T) {
	writer := NewWriter(nil, nil)
	writer.stopped.Store(false)
	for i := 0; i < writerChanCap; i++ {
		writer.ch <- FlatDetail{Timestamp: time.Now(), Model: "model"}
	}

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		writer.Submit(FlatDetail{Timestamp: time.Now(), Model: "model"})
		close(done)
	}()
	<-started

	select {
	case <-done:
		t.Fatal("Submit returned while writer queue was full")
	case <-time.After(20 * time.Millisecond):
	}

	<-writer.ch
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit did not resume after queue capacity became available")
	}
}

func TestWriterStopUnblocksSubmitWaitingOnFullQueue(t *testing.T) {
	writer := NewWriter(nil, nil)
	for i := 0; i < writerChanCap; i++ {
		writer.ch <- FlatDetail{Timestamp: time.Now(), Model: "model"}
	}

	submitDone := make(chan struct{})
	go func() {
		writer.Submit(FlatDetail{Timestamp: time.Now(), Model: "blocked"})
		close(submitDone)
	}()
	select {
	case <-submitDone:
		t.Fatal("Submit returned while writer queue was full")
	case <-time.After(20 * time.Millisecond):
	}

	stopDone := make(chan struct{})
	go func() {
		writer.Stop()
		close(stopDone)
	}()
	select {
	case <-submitDone:
	case <-time.After(time.Second):
		t.Fatal("Submit did not return when writer stopped")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after waking blocked Submit")
	}
}

func TestWriterSubmitDuringStopDoesNotPanic(t *testing.T) {
	writer := NewWriter(nil, nil)
	writer.Start(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			writer.Submit(FlatDetail{Timestamp: time.Now(), Model: "model"})
		}()
	}
	writer.Stop()
	wg.Wait()
}

func TestWriterContextCancellationStopsAcceptanceAndDrains(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	writer := NewWriter(store, nil)
	writer.Start(ctx)
	writer.Submit(FlatDetail{
		Timestamp: time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC),
		Model:     "accepted",
		Tokens:    tokenStats{TotalTokens: 1},
	})
	cancel()
	select {
	case <-writer.done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not stop after context cancellation")
	}
	if !writer.stopped.Load() {
		t.Fatal("writer stopped flag = false after context cancellation")
	}

	writer.Submit(FlatDetail{
		Timestamp: time.Date(2026, 5, 20, 11, 0, 0, 0, time.UTC),
		Model:     "rejected",
		Tokens:    tokenStats{TotalTokens: 1},
	})
	var events int64
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("events = %d, want one accepted record", events)
	}
	writer.Stop()
}

func TestPersistEventRejectsInvalidTokensAndTimestamp(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		detail FlatDetail
		want   string
	}{
		{
			name: "negative token",
			detail: FlatDetail{
				Timestamp: time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC),
				Model:     "m",
				Tokens:    tokenStats{InputTokens: -1},
			},
			want: "input_tokens must be non-negative",
		},
		{
			name: "timestamp outside UnixNano range",
			detail: FlatDetail{
				Timestamp: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
				Model:     "m",
				Tokens:    tokenStats{TotalTokens: 1},
			},
			want: "outside the UnixNano range",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			_, err = persistEvent(ctx, tx, store, &test.detail, nil)
			_ = tx.Rollback()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("persistEvent error = %v, want contain %q", err, test.want)
			}
		})
	}
}

func TestPersistEventUsesFloorBucketsBeforeUnixEpoch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	detail := FlatDetail{
		Timestamp: time.Date(1969, 12, 31, 23, 30, 0, 0, time.UTC),
		Model:     "m",
		Tokens:    tokenStats{TotalTokens: 1},
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persistEvent: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	wantHour := time.Date(1969, 12, 31, 23, 0, 0, 0, time.UTC).UnixNano()
	wantDay := time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC).UnixNano()
	var hourNs, dayNs int64
	if err := store.db.QueryRow("SELECT bucket_ts_ns FROM hour_bucket").Scan(&hourNs); err != nil {
		t.Fatalf("query hour bucket: %v", err)
	}
	if err := store.db.QueryRow("SELECT day_ts_ns FROM dirty_day").Scan(&dayNs); err != nil {
		t.Fatalf("query dirty day: %v", err)
	}
	if hourNs != wantHour || dayNs != wantDay {
		t.Fatalf("bucket starts = (%d, %d), want (%d, %d)", hourNs, dayNs, wantHour, wantDay)
	}
}

func TestComputeFingerprintUsesUnambiguousIdentity(t *testing.T) {
	base := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 30, 0, 123_456_789, time.UTC),
		Model:     "claude-opus-4-7",
		Source:    "user@example.com",
		AuthIndex: "auth-idx-1",
		APIKey:    "sk-test",
		Provider:  "claude",
		Tokens:    tokenStats{InputTokens: 100, OutputTokens: 200, TotalTokens: 300},
	}
	withRequestID := base
	withRequestID.RequestID = "request-1"
	otherRequestID := base
	otherRequestID.RequestID = "request-2"
	if ComputeFingerprint(withRequestID) == ComputeFingerprint(otherRequestID) {
		t.Fatal("different request IDs produced the same fingerprint")
	}

	left := base
	left.APIKey = "a|b"
	left.Model = "c"
	right := base
	right.APIKey = "a"
	right.Model = "b|c"
	if ComputeFingerprint(left) == ComputeFingerprint(right) {
		t.Fatal("delimiter-ambiguous fields produced the same fingerprint")
	}

	otherProvider := base
	otherProvider.Provider = "codex"
	if ComputeFingerprint(base) == ComputeFingerprint(otherProvider) {
		t.Fatal("different providers produced the same fingerprint")
	}
}

// TestQueryByCredential_DistinctProviders verifies that two events with the
// same Source but different Providers land in separate by_credential buckets,
// keyed as "provider:source" — the whole point of the v2 schema upgrade.
func TestQueryByCredential_DistinctProviders(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, prov := range []string{"claude", "codex"} {
		d := FlatDetail{
			Timestamp: base,
			Model:     "shared-model",
			Source:    "shared@example.com",
			Provider:  prov,
			Tokens:    tokenStats{TotalTokens: 1},
		}
		if _, err := persistEvent(ctx, tx, store, &d, nil); err != nil {
			t.Fatalf("persist %s: %v", prov, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	qr, err := store.Query(
		ctx,
		base.Add(-time.Hour), base.Add(time.Hour),
		EventFilters{}, "hourly", true,
	)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	wantClaude := "claude:shared@example.com"
	wantCodex := "codex:shared@example.com"
	if _, ok := qr.ByCredential[wantClaude]; !ok {
		t.Fatalf("missing %q in ByCredential: got keys %v", wantClaude, mapKeys(qr.ByCredential))
	}
	if _, ok := qr.ByCredential[wantCodex]; !ok {
		t.Fatalf("missing %q in ByCredential: got keys %v", wantCodex, mapKeys(qr.ByCredential))
	}

	// Filter "claude:shared@..." must isolate the claude row only.
	qrClaude, err := store.Query(
		ctx,
		base.Add(-time.Hour), base.Add(time.Hour),
		EventFilters{Source: wantClaude}, "hourly", true,
	)
	if err != nil {
		t.Fatalf("Query filtered: %v", err)
	}
	if got := qrClaude.Totals.Requests; got != 1 {
		t.Fatalf("filtered totals = %d, want 1", got)
	}
	if _, ok := qrClaude.ByCredential[wantCodex]; ok {
		t.Fatalf("codex bucket leaked into claude-only filter")
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMigrateV1ToV2 builds a synthetic v1 events.db on disk (via raw SQL using
// the pre-v2 schema literally), pre-populates it with one event, then opens
// it through OpenStore so the migration runs and verifies:
//
//   - schema_version is advanced through every migration to 4
//   - provider_id column appears on events / hour_bucket / day_bucket
//   - latency aggregate columns appear on hour_bucket / day_bucket
//   - the legacy event row survives with provider_id NULL
//   - re-opening the migrated db is a no-op (idempotency)
//
// This reproduces the LA upgrade path exactly — without it, a regression in
// migrateV1ToV2 would only surface in production, where rollback is hard
// because ALTER TABLE ADD COLUMN is unidirectional.
func TestMigrateV3ToV4RewritesFingerprintsBeforeReimport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	detail := FlatDetail{
		Timestamp: time.Date(2026, 7, 30, 4, 0, 0, 123, time.UTC),
		RequestID: "request-v3",
		Model:     "model-v3",
		APIKey:    "key-v3",
		Source:    "source-v3",
		AuthIndex: "auth-v3",
		Provider:  "provider-v3",
		Tokens:    tokenStats{InputTokens: 2, OutputTokens: 3, TotalTokens: 5},
		Completed: true,
	}

	store, errOpen := OpenStore(dbPath)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	writer := NewWriter(store, nil)
	if errFlush := writer.flush([]FlatDetail{detail}); errFlush != nil {
		_ = store.Close()
		t.Fatalf("initial flush: %v", errFlush)
	}
	if errClose := store.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	rawDB, errRaw := sql.Open("sqlite", dbPath)
	if errRaw != nil {
		t.Fatal(errRaw)
	}
	legacyFingerprint := []byte("01234567890123456789")
	if len(legacyFingerprint) != 20 {
		t.Fatalf("legacy fingerprint length = %d", len(legacyFingerprint))
	}
	if _, errExec := rawDB.Exec(
		"UPDATE events SET fingerprint = ?; UPDATE meta SET value = '3' WHERE key = 'schema_version'",
		legacyFingerprint,
	); errExec != nil {
		_ = rawDB.Close()
		t.Fatal(errExec)
	}
	if errClose := rawDB.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	migrated, errMigrate := OpenStore(dbPath)
	if errMigrate != nil {
		t.Fatalf("OpenStore v3 database: %v", errMigrate)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	var fingerprint []byte
	if errQuery := migrated.db.QueryRow("SELECT fingerprint FROM events").Scan(&fingerprint); errQuery != nil {
		t.Fatal(errQuery)
	}
	wantFingerprint := ComputeFingerprint(detail)
	if string(fingerprint) != string(wantFingerprint[:]) {
		t.Fatalf("migrated fingerprint = %x, want %x", fingerprint, wantFingerprint)
	}

	migratedWriter := NewWriter(migrated, nil)
	if errFlush := migratedWriter.flush([]FlatDetail{detail}); errFlush != nil {
		t.Fatalf("reimport migrated event: %v", errFlush)
	}
	var events int64
	if errQuery := migrated.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); errQuery != nil {
		t.Fatal(errQuery)
	}
	if events != 1 {
		t.Fatalf("events after reimport = %d, want 1", events)
	}
	var requests int64
	if errQuery := migrated.db.QueryRow("SELECT COALESCE(SUM(requests), 0) FROM hour_bucket").Scan(&requests); errQuery != nil {
		t.Fatal(errQuery)
	}
	if requests != 1 {
		t.Fatalf("hour bucket requests after reimport = %d, want 1", requests)
	}
}

func TestMigrateV1ToV2(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")

	// Step 1: build a v1 db using the schema as it stood before this commit.
	// Anything beyond what migrateV1ToV2 needs to find can be omitted; we only
	// want enough structure to exercise the column add and index rebuild.
	v1Schema := `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
INSERT INTO meta(key, value) VALUES ('schema_version', '1');

CREATE TABLE dim_model (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dim_api_key (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dim_source (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dim_auth_index (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dim_credential (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);

CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    fingerprint BLOB NOT NULL UNIQUE,
    ts_ns INTEGER NOT NULL,
    model_id INTEGER NOT NULL REFERENCES dim_model(id),
    api_key_id INTEGER REFERENCES dim_api_key(id),
    source_id INTEGER REFERENCES dim_source(id),
    auth_index_id INTEGER REFERENCES dim_auth_index(id),
    credential_id INTEGER REFERENCES dim_credential(id),
    failed INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro INTEGER NOT NULL DEFAULT 0,
    metadata TEXT
);

CREATE TABLE hour_bucket (
    id INTEGER PRIMARY KEY,
    bucket_ts_ns INTEGER NOT NULL,
    model_id INTEGER NOT NULL REFERENCES dim_model(id),
    credential_id INTEGER REFERENCES dim_credential(id),
    api_key_id INTEGER REFERENCES dim_api_key(id),
    requests INTEGER NOT NULL DEFAULT 0,
    success INTEGER NOT NULL DEFAULT 0,
    failure INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX uq_hour_bucket_key ON hour_bucket(
    bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0)
);

CREATE TABLE day_bucket (
    id INTEGER PRIMARY KEY,
    bucket_ts_ns INTEGER NOT NULL,
    model_id INTEGER NOT NULL REFERENCES dim_model(id),
    credential_id INTEGER REFERENCES dim_credential(id),
    api_key_id INTEGER REFERENCES dim_api_key(id),
    requests INTEGER NOT NULL DEFAULT 0,
    success INTEGER NOT NULL DEFAULT 0,
    failure INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX uq_day_bucket_key ON day_bucket(
    bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0)
);

INSERT INTO dim_model(id, name) VALUES (1, 'legacy-model');
INSERT INTO events(id, fingerprint, ts_ns, model_id, failed, total_tokens, metadata)
       VALUES (1, x'0102030405060708090a0b0c0d0e0f1011121314', 1000000000, 1, 0, 42, '{"latency_ms":25}');
INSERT INTO hour_bucket(id, bucket_ts_ns, model_id, requests, success, failure, total_tokens)
       VALUES (1, 0, 1, 1, 1, 0, 42);
INSERT INTO day_bucket(id, bucket_ts_ns, model_id, requests, success, failure, total_tokens)
       VALUES (1, 0, 1, 1, 1, 0, 42);
`
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw v1 db: %v", err)
	}
	if _, err := rawDB.Exec(v1Schema); err != nil {
		_ = rawDB.Close()
		t.Fatalf("apply v1 schema: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close v1 db: %v", err)
	}

	// Step 2: open through OpenStore — this triggers migrateV1ToV2.
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore (v1→v2): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// schema_version must now match the current schema.
	var ver string
	if err := store.db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&ver); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if ver != "4" {
		t.Fatalf("schema_version = %q, want \"4\"", ver)
	}

	// provider_id column must exist on every fact table.
	for _, table := range []string{"events", "hour_bucket", "day_bucket"} {
		if !columnPresent(t, store.db, table, "provider_id") {
			t.Fatalf("provider_id missing on %s after migration", table)
		}
	}
	for _, table := range []string{"hour_bucket", "day_bucket"} {
		for _, column := range []string{"latency_ms_total", "latency_samples"} {
			if !columnPresent(t, store.db, table, column) {
				t.Fatalf("%s missing on %s after migration", column, table)
			}
		}
		var latencyTotal, latencySamples int64
		if err := store.db.QueryRow(
			"SELECT latency_ms_total, latency_samples FROM "+table,
		).Scan(&latencyTotal, &latencySamples); err != nil {
			t.Fatalf("read %s latency after migration: %v", table, err)
		}
		if latencyTotal != 25 || latencySamples != 1 {
			t.Fatalf("%s latency after migration = %d/%d, want 25/1", table, latencyTotal, latencySamples)
		}
	}

	// Legacy event row must survive with provider_id NULL.
	var legacyTokens int64
	var legacyProvider sql.NullInt64
	if err := store.db.QueryRow(
		"SELECT total_tokens, provider_id FROM events WHERE id = 1",
	).Scan(&legacyTokens, &legacyProvider); err != nil {
		t.Fatalf("read legacy event: %v", err)
	}
	if legacyTokens != 42 {
		t.Fatalf("legacy event total_tokens = %d, want 42", legacyTokens)
	}
	if legacyProvider.Valid {
		t.Fatalf("expected provider_id NULL on legacy event, got %d", legacyProvider.Int64)
	}

	// Step 3: re-open. migration must be idempotent (already-v2 path).
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("re-OpenStore: %v", err)
	}
	defer func() {
		if errClose := store2.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()
	if err := store2.db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&ver); err != nil {
		t.Fatalf("re-read schema_version: %v", err)
	}
	if ver != "4" {
		t.Fatalf("schema_version after re-open = %q, want \"4\"", ver)
	}
}

func TestMigrateV3ToV4RewritesMultipleFingerprintPages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const eventCount = fingerprintMigrationPageSize + 1
	base := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
	details := make([]FlatDetail, 0, eventCount)
	for i := 0; i < eventCount; i++ {
		details = append(details, FlatDetail{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			RequestID: fmt.Sprintf("request-%d", i),
			Model:     "model-v3-pages",
			APIKey:    "key-v3",
			Source:    "source-v3",
			AuthIndex: "auth-v3",
			Provider:  "provider-v3",
			Tokens:    tokenStats{InputTokens: int64(i + 1), TotalTokens: int64(i + 1)},
			Completed: true,
		})
	}
	writer := NewWriter(store, nil)
	if err := writer.flush(details); err != nil {
		_ = store.Close()
		t.Fatalf("initial flush: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`
		UPDATE events SET fingerprint = CAST(printf('%020d', id) AS BLOB);
		UPDATE meta SET value = '3' WHERE key = 'schema_version';
	`); err != nil {
		_ = rawDB.Close()
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore v3 database: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	rows, err := migrated.db.Query("SELECT id, fingerprint FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var id int64
		var fingerprint []byte
		if err := rows.Scan(&id, &fingerprint); err != nil {
			t.Fatal(err)
		}
		if id <= 0 || id > int64(len(details)) {
			t.Fatalf("event id = %d, want 1..%d", id, len(details))
		}
		want := ComputeFingerprint(details[id-1])
		if string(fingerprint) != string(want[:]) {
			t.Fatalf("event %d fingerprint = %x, want %x", id, fingerprint, want)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != eventCount {
		t.Fatalf("migrated events = %d, want %d", count, eventCount)
	}
}

func TestMigrateV3ToV4RejectsNonIntegerStorage(t *testing.T) {
	dbPath := prepareV3TestDB(t)
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw v3 db: %v", err)
	}
	if _, err := rawDB.Exec(`
		INSERT INTO dim_model(id, name) VALUES (1, 'legacy-real-model');
		INSERT INTO events(
			fingerprint, ts_ns, model_id, failed,
			input_tokens, output_tokens, reasoning_tokens,
			cached_tokens, total_tokens, cost_micro
		) VALUES (
			zeroblob(20), 1, 1, 0,
			1.5, 0, 0, 0, 1, 0
		)`); err != nil {
		_ = rawDB.Close()
		t.Fatalf("insert real storage class: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw v3 db: %v", err)
	}

	store, err := OpenStore(dbPath)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "non-integer storage classes") {
		t.Fatalf("OpenStore error = %v, want non-integer storage rejection", err)
	}

	rawDB, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen rejected v3 db: %v", err)
	}
	defer func() { _ = rawDB.Close() }()
	var version, storageClass string
	if err := rawDB.QueryRow(
		"SELECT value FROM meta WHERE key = 'schema_version'",
	).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if err := rawDB.QueryRow(
		"SELECT typeof(input_tokens) FROM events LIMIT 1",
	).Scan(&storageClass); err != nil {
		t.Fatalf("read input token storage class: %v", err)
	}
	if version != "3" || storageClass != "real" {
		t.Fatalf("after failed migration version=%q storage=%q, want 3/real", version, storageClass)
	}
}

func TestMigrateV3ToV4InstallsIntegerStorageTriggers(t *testing.T) {
	dbPath := prepareV3TestDB(t)
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore v3 database: %v", err)
	}
	defer func() { _ = store.Close() }()

	var version string
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key = 'schema_version'",
	).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != "4" {
		t.Fatalf("schema version=%q, want 4", version)
	}
	for _, name := range []string{
		"usage_events_integer_insert",
		"usage_events_integer_update",
		"usage_hour_bucket_integer_insert",
		"usage_hour_bucket_integer_update",
		"usage_day_bucket_integer_insert",
		"usage_day_bucket_integer_update",
	} {
		var count int
		if err := store.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?",
			name,
		).Scan(&count); err != nil {
			t.Fatalf("query trigger %s: %v", name, err)
		}
		if count != 1 {
			t.Fatalf("trigger %s count=%d, want 1", name, count)
		}
	}

	if _, err := store.db.Exec(
		"INSERT INTO dim_model(id, name) VALUES (1, 'trigger-model')",
	); err != nil {
		t.Fatalf("insert trigger model: %v", err)
	}
	_, err = store.db.Exec(
		`INSERT INTO events(
			fingerprint, ts_ns, model_id, failed,
			input_tokens, output_tokens, reasoning_tokens,
			cached_tokens, total_tokens, cost_micro
		) VALUES (zeroblob(20), ?, 1, 0, 0, 0, 0, 0, 0, 0)`,
		1.5,
	)
	if err == nil || !strings.Contains(err.Error(), "requires integer storage classes") {
		t.Fatalf("real-valued insert error = %v, want integer storage rejection", err)
	}
}

func prepareV3TestDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("create current database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close current database: %v", err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open current database for downgrade: %v", err)
	}
	for _, table := range []string{"events", "hour_bucket", "day_bucket"} {
		for _, operation := range []string{"insert", "update"} {
			name := fmt.Sprintf("usage_%s_integer_%s", table, operation)
			if _, err := rawDB.Exec("DROP TRIGGER IF EXISTS " + name); err != nil {
				_ = rawDB.Close()
				t.Fatalf("drop trigger %s: %v", name, err)
			}
		}
	}
	if _, err := rawDB.Exec(
		"UPDATE meta SET value = '3' WHERE key = 'schema_version'",
	); err != nil {
		_ = rawDB.Close()
		t.Fatalf("set schema version 3: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close downgraded database: %v", err)
	}
	return dbPath
}

// columnPresent answers "does table have column?" via PRAGMA table_info.
func columnPresent(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var query string
	switch table {
	case "events":
		query = "PRAGMA table_info(events)"
	case "hour_bucket":
		query = "PRAGMA table_info(hour_bucket)"
	case "day_bucket":
		query = "PRAGMA table_info(day_bucket)"
	default:
		t.Fatalf("unexpected table for column check: %s", table)
	}
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			colType string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}
