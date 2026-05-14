// Last compiled: 2026-04-28
// Author: pyro

package usage

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"fmt"
	"path/filepath"
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
	defer store2.Close()

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
func TestResolveDimIDs(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

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

// TestQueryFiltered_ZeroMatchNoFallback verifies the critical post-incident
// invariant: an active filter that matches zero dim ids returns an empty
// result, NEVER falls back to all rows.
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
func TestComputeFingerprint_NULLProviderEquivalent(t *testing.T) {
	d := FlatDetail{
		Timestamp: time.Date(2026, 4, 27, 10, 30, 0, 123_456_789, time.UTC),
		Model:     "claude-opus-4-7",
		Source:    "user@example.com",
		AuthIndex: "auth-idx-1",
		APIKey:    "sk-test",
		Failed:    false,
		Tokens:    tokenStats{InputTokens: 100, OutputTokens: 200, TotalTokens: 300},
	}
	want := sha1.Sum(
		[]byte(fmt.Sprintf(
			"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
			d.APIKey,
			d.Model,
			d.Timestamp.UTC().Format(time.RFC3339Nano),
			d.Source,
			d.AuthIndex,
			d.Failed,
			d.Tokens.InputTokens,
			d.Tokens.OutputTokens,
			d.Tokens.ReasoningTokens,
			d.Tokens.CachedTokens,
			d.Tokens.TotalTokens,
		)),
	)
	got := ComputeFingerprint(d)
	if got != want {
		t.Fatalf("v1-equivalent fingerprint mismatch:\n got %x\nwant %x", got, want)
	}

	// Same logical event with a non-empty Provider must produce a *different*
	// fingerprint — otherwise the same email under claude vs codex would still
	// collapse onto one row.
	d.Provider = "claude"
	got2 := ComputeFingerprint(d)
	if got2 == want {
		t.Fatalf("expected provider-tagged fingerprint to differ from null-provider one, both = %x", got2)
	}

	// And a different provider on the same payload should also produce a
	// distinct fingerprint.
	d.Provider = "codex"
	got3 := ComputeFingerprint(d)
	if got3 == got2 {
		t.Fatalf("expected distinct fingerprints for claude vs codex, both = %x", got2)
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
//   - schema_version is bumped to 2
//   - provider_id column appears on events / hour_bucket / day_bucket
//   - the legacy event row survives with provider_id NULL
//   - re-opening the migrated db is a no-op (idempotency)
//
// This reproduces the LA upgrade path exactly — without it, a regression in
// migrateV1ToV2 would only surface in production, where rollback is hard
// because ALTER TABLE ADD COLUMN is unidirectional.
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
INSERT INTO events(id, fingerprint, ts_ns, model_id, failed, total_tokens)
       VALUES (1, x'0102030405060708090a0b0c0d0e0f1011121314', 1000000000, 1, 0, 42);
INSERT INTO hour_bucket(id, bucket_ts_ns, model_id, requests, success, failure, total_tokens)
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

	// schema_version must now be 2.
	var ver string
	if err := store.db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&ver); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if ver != "2" {
		t.Fatalf("schema_version = %q, want \"2\"", ver)
	}

	// provider_id column must exist on every fact table.
	for _, table := range []string{"events", "hour_bucket", "day_bucket"} {
		if !columnPresent(t, store.db, table, "provider_id") {
			t.Fatalf("provider_id missing on %s after migration", table)
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
	defer store2.Close()
	if err := store2.db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&ver); err != nil {
		t.Fatalf("re-read schema_version: %v", err)
	}
	if ver != "2" {
		t.Fatalf("schema_version after re-open = %q, want \"2\"", ver)
	}
}

// columnPresent answers "does table have column?" via PRAGMA table_info.
func columnPresent(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
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
