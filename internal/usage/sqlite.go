// Last compiled: 2026-04-28
// Author: pyro

package usage

import (
	"context"
	"crypto/sha1"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite" // pure-Go SQLite driver — required by .goreleaser CGO_ENABLED=0
)

//go:embed schema.sql
var schemaSQL string

const (
	// schemaVersion mirrors the 'schema_version' row inserted by schema.sql.
	// OpenStore refuses to run when the persisted value disagrees, forcing an
	// explicit migration step rather than silent corruption.
	schemaVersion = "2"

	// dbFileName is the SQLite database file inside the usage data dir.
	dbFileName = "events.db"
)

// Store owns the *sql.DB plus dimension-name → id caches.
//
// Cache strategy: every dim is INSERT-OR-IGNORE on first write and SELECT-d
// back to learn the assigned id, so id stability is guaranteed even when two
// goroutines race on the same name. Once seen, the (name, id) pair is pinned
// in the local sync.Map so steady-state writes only need a single SQL roundtrip
// (the events INSERT). The cache never invalidates because dim ids never change
// — rows are never deleted (FK ON DELETE RESTRICT) and ROWIDs are stable.
type Store struct {
	db *sql.DB

	modelCache      sync.Map // string → int64
	apiKeyCache     sync.Map
	sourceCache     sync.Map
	authIndexCache  sync.Map
	credentialCache sync.Map
	providerCache   sync.Map
}

// OpenStore opens (or creates) the SQLite database, applies PRAGMAs, runs the
// schema (idempotent thanks to IF NOT EXISTS), and validates schema_version.
//
// Connection limits: SQLite serialises writes regardless of pool size, so a
// large pool just inflates context overhead. Eight connections gives readers
// breathing room without wasting memory.
func OpenStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("usage: empty SQLite db path")
	}

	// Per-connection pragmas via DSN: modernc.org/sqlite re-applies these on
	// every new pooled connection, which matters for foreign_keys (per-conn)
	// and busy_timeout (per-conn). journal_mode + synchronous are db-level
	// but harmless to repeat.
	dsn := dbPath +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=cache_size(-64000)" +
		"&_pragma=temp_store(MEMORY)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("usage: open sqlite %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: ping sqlite %s: %w", dbPath, err)
	}

	// Database-level pragmas only need to run once per file.
	for _, stmt := range []string{
		"PRAGMA mmap_size=268435456",
		"PRAGMA wal_autocheckpoint=1000",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: pragma %q: %w", stmt, err)
		}
	}

	if _, err := db.ExecContext(
		ctx, "CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: create meta: %w", err)
	}

	// Read whatever schema_version is currently persisted. Empty string means
	// "fresh database, no version yet" — treat the same as if a version row
	// were absent and let the schema apply step seed it. v1 means "needs
	// upgrade"; v2 means "current". migrations must run BEFORE schema.sql
	// applies, otherwise schema.sql's v2 indexes would reference columns the
	// pre-migration tables don't have yet.
	var version string
	err = db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		return nil, fmt.Errorf("usage: read schema_version: %w", err)
	}

	// Apply forward migrations as needed. Each migration is idempotent on a
	// per-version basis and bumps meta.schema_version atomically with the
	// structural changes so a crash mid-migration leaves a consistent state.
	if version == "1" {
		if err := migrateV1ToV2(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v1→v2: %w", err)
		}
		version = "2"
	}

	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: apply schema: %w", err)
	}

	// Re-read in case schema.sql seeded the version on a fresh database.
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: read schema_version (post-apply): %w", err)
	}

	if version != schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("usage: unexpected schema_version=%s (want %s)", version, schemaVersion)
	}

	return &Store{db: db}, nil
}

// migrateV1ToV2 rewrites the old (v1) events / hour_bucket / day_bucket tables
// to add the provider_id column and rebuilds the UNIQUE indexes so the
// (bucket, model, credential, api_key) tuple now also includes provider.
//
// The schema.sql script has already created dim_provider (idempotent). What
// this function adds is the missing provider_id COLUMN on the existing fact
// tables — IF NOT EXISTS on CREATE TABLE skipped that branch on upgrade so we
// must ALTER TABLE explicitly.
//
// Why a full transaction: the column add and the UNIQUE-index rebuild must
// either both land or neither. A partial state where provider_id exists but
// the unique index still excludes it would silently merge (claude, codex)
// rows on the next UPSERT.
//
// modernc.org/sqlite supports ALTER TABLE ADD COLUMN, DROP INDEX, and
// CREATE UNIQUE INDEX inside a transaction.
func migrateV1ToV2(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// columnExists is cheaper than a try-catch on ALTER TABLE — modernc.org
	// returns a generic "duplicate column" error which we'd then have to
	// string-match against. PRAGMA table_info is the canonical SQLite check.
	columnExists := func(table, column string) (bool, error) {
		rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
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
				return false, fmt.Errorf("scan table_info(%s): %w", table, err)
			}
			if name == column {
				return true, nil
			}
		}
		return false, rows.Err()
	}

	// Add provider_id column to each fact table when missing. ALTER TABLE on
	// a table that already has the column would fail; the columnExists guard
	// keeps the migration idempotent for partial-completion replays.
	for _, table := range []string{"events", "hour_bucket", "day_bucket"} {
		exists, err := columnExists(table, "provider_id")
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		stmt := fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN provider_id INTEGER REFERENCES dim_provider(id) ON DELETE RESTRICT",
			table,
		)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("alter %s add provider_id: %w", table, err)
		}
	}

	// Rebuild bucket UNIQUE indexes so provider_id participates in the dedup
	// key. Drop-then-create is safe inside the transaction; if anything below
	// fails we roll back and the original index is restored.
	rebuildIndex := func(table, indexName, indexBody string) error {
		if _, err := tx.ExecContext(ctx, "DROP INDEX IF EXISTS "+indexName); err != nil {
			return fmt.Errorf("drop %s: %w", indexName, err)
		}
		create := "CREATE UNIQUE INDEX " + indexName + " ON " + table + "(" + indexBody + ")"
		if _, err := tx.ExecContext(ctx, create); err != nil {
			return fmt.Errorf("create %s: %w", indexName, err)
		}
		return nil
	}
	const bucketKeyBody = "bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0), ifnull(provider_id, 0)"
	if err := rebuildIndex("hour_bucket", "uq_hour_bucket_key", bucketKeyBody); err != nil {
		return err
	}
	if err := rebuildIndex("day_bucket", "uq_day_bucket_key", bucketKeyBody); err != nil {
		return err
	}

	// Per-dimension index on the new column so future "by provider" queries
	// can use it. Non-unique, so re-running is idempotent via IF NOT EXISTS.
	if _, err := tx.ExecContext(
		ctx,
		"CREATE INDEX IF NOT EXISTS idx_events_provider_ts_ns ON events(provider_id, ts_ns)",
	); err != nil {
		return fmt.Errorf("create idx_events_provider_ts_ns: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		"CREATE INDEX IF NOT EXISTS idx_hour_bucket_prov_ts_ns ON hour_bucket(provider_id, bucket_ts_ns)",
	); err != nil {
		return fmt.Errorf("create idx_hour_bucket_prov_ts_ns: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		"CREATE INDEX IF NOT EXISTS idx_day_bucket_prov_ts_ns ON day_bucket(provider_id, bucket_ts_ns)",
	); err != nil {
		return fmt.Errorf("create idx_day_bucket_prov_ts_ns: %w", err)
	}

	// Bump schema_version in the same transaction as the structural changes
	// so a crashed migration leaves the value untouched and the next startup
	// re-runs the whole block.
	if _, err := tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '2' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	log.Infof("usage: migrated events.db schema v1 → v2 (provider dimension)")
	return nil
}

// Close releases the underlying database. Safe to call on nil.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying *sql.DB for writer / query / import packages.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// dbExec is the minimal interface satisfied by both *sql.DB and *sql.Tx so
// dim resolution works inside or outside a transaction without duplication.
type dbExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// resolveDimID is the shared get-or-create path for every dim_* table.
// An empty (or whitespace-only) name returns ok=false without touching the DB
// so the caller can map it to NULL in the events row.
func (s *Store) resolveDimID(
	ctx context.Context,
	exec dbExec,
	cache *sync.Map,
	table, name string,
) (int64, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, nil
	}
	if v, ok := cache.Load(name); ok {
		return v.(int64), true, nil
	}

	// Table name comes from constants in this package; safe to interpolate.
	if _, err := exec.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+"(name) VALUES (?)", name); err != nil {
		return 0, false, fmt.Errorf("usage: insert into %s: %w", table, err)
	}
	var id int64
	if err := exec.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE name = ?", name).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("usage: select from %s: %w", table, err)
	}
	cache.Store(name, id)
	return id, true, nil
}

// ResolveModelID returns the dim_model id, falling back to "unknown" when the
// caller passes an empty model name. Model is the only dim that is NOT NULL on
// the events table, so we never return NullInt64 here.
func (s *Store) ResolveModelID(ctx context.Context, exec dbExec, name string) (int64, error) {
	if strings.TrimSpace(name) == "" {
		name = "unknown"
	}
	id, ok, err := s.resolveDimID(ctx, exec, &s.modelCache, "dim_model", name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("usage: empty model name after fallback")
	}
	return id, nil
}

// ResolveAPIKeyID returns the dim_api_key id, or a NULL when name is blank.
func (s *Store) ResolveAPIKeyID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.apiKeyCache, "dim_api_key", name)
}

// ResolveSourceID returns the dim_source id, or a NULL when name is blank.
func (s *Store) ResolveSourceID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.sourceCache, "dim_source", name)
}

// ResolveAuthIndexID returns the dim_auth_index id, or a NULL when name is blank.
func (s *Store) ResolveAuthIndexID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.authIndexCache, "dim_auth_index", name)
}

// ResolveCredentialID returns the dim_credential id for COALESCE(source, auth_index).
// Both inputs blank → NULL credential, matching summary.go::credKey semantics.
func (s *Store) ResolveCredentialID(ctx context.Context, exec dbExec, source, authIndex string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.credentialCache, "dim_credential", credentialName(source, authIndex))
}

// ResolveProviderID returns the dim_provider id, or a NULL when name is blank.
// Old v1/v2 JSON imports never carried provider, so a blank string is the
// expected steady-state for backfilled rows — the NULL surfaces in /usage
// queries as the "(unknown)" bucket so the dimension chart doesn't drop them.
func (s *Store) ResolveProviderID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.providerCache, "dim_provider", name)
}

func (s *Store) resolveNullableDim(
	ctx context.Context,
	exec dbExec,
	cache *sync.Map,
	table, name string,
) (sql.NullInt64, error) {
	id, ok, err := s.resolveDimID(ctx, exec, cache, table, name)
	if err != nil {
		return sql.NullInt64{}, err
	}
	if !ok {
		return sql.NullInt64{}, nil
	}
	return sql.NullInt64{Int64: id, Valid: true}, nil
}

// credentialName mirrors summary.go::credKey: source wins, auth_index is the
// fallback. Both blank means "no credential dimension" (NULL on disk).
func credentialName(source, authIndex string) string {
	if s := strings.TrimSpace(source); s != "" {
		return s
	}
	return strings.TrimSpace(authIndex)
}

// ComputeFingerprint produces a 20-byte SHA-1 over the canonical dedupKey
// payload. Field order MUST stay aligned with dedupKey() in logger_plugin.go
// so the same logical event arriving via v2 detail / v1 nested / frontend
// CSV / frontend JSON / /usage/export collapses onto one row through the
// events.fingerprint UNIQUE constraint.
//
// Provider compatibility: when Provider is empty (legacy JSON imports, where
// the field never existed on disk) the payload format reverts to the v1 11-
// field layout so historical rows reproduce the same SHA-1 the JSON era
// produced. When Provider is set (live writes from the v2 LoggerPlugin) the
// 12-field layout is used so the same email speaking to two providers no
// longer dedupes to a single row.
//
// SHA-1 is used as a fast 160-bit hash, not for security; collision risk for a
// well-formed dedup payload is negligible (birthday bound ≫ realistic event
// counts) and any duplicate would harmlessly produce INSERT OR IGNORE no-ops.
func ComputeFingerprint(d FlatDetail) [20]byte {
	tokens := normaliseTokenStats(d.Tokens)
	var payload string
	if d.Provider == "" {
		payload = fmt.Sprintf(
			"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
			d.APIKey,
			d.Model,
			d.Timestamp.UTC().Format(time.RFC3339Nano),
			d.Source,
			d.AuthIndex,
			d.Failed,
			tokens.InputTokens,
			tokens.OutputTokens,
			tokens.ReasoningTokens,
			tokens.CachedTokens,
			tokens.TotalTokens,
		)
	} else {
		payload = fmt.Sprintf(
			"%s|%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
			d.APIKey,
			d.Model,
			d.Timestamp.UTC().Format(time.RFC3339Nano),
			d.Source,
			d.AuthIndex,
			d.Provider,
			d.Failed,
			tokens.InputTokens,
			tokens.OutputTokens,
			tokens.ReasoningTokens,
			tokens.CachedTokens,
			tokens.TotalTokens,
		)
	}
	return sha1.Sum([]byte(payload))
}
