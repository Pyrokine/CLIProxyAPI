package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"modernc.org/sqlite" // pure-Go SQLite driver — required by .goreleaser CGO_ENABLED=0
)

//go:embed schema.sql
var schemaSQL string

const (
	// CurrentSchemaVersion mirrors the 'schema_version' row inserted by schema.sql.
	// OpenStore refuses to run when the persisted value disagrees, forcing an
	// explicit migration step rather than silent corruption.
	CurrentSchemaVersion = "9"

	// dbFileName is the SQLite database file inside the usage data dir.
	dbFileName = "events.db"

	eventFingerprintBytes = 20

	// Production-size databases can require more than five minutes for forward
	// migrations. Keep one bounded deadline across setup and migration.
	storeOpenTimeout = 15 * time.Minute

	maxSQLitePageCount      = int64(2147483646)
	storeMaxOpenConnections = 8
)

type sqliteConnector struct {
	driver *sqlite.Driver
	dsn    string
}

func (c *sqliteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c *sqliteConnector) Driver() driver.Driver {
	return c.driver
}

func inspectExistingSchemaVersion(dbPath string) (string, bool, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat database: %w", err)
	}

	probeDSN := dbPath +
		"?mode=ro" +
		"&_pragma=query_only(1)" +
		"&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", probeDSN)
	if err != nil {
		return "", false, fmt.Errorf("open read-only schema probe: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var metaExists int
	if err := db.QueryRowContext(
		ctx,
		"SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='meta')",
	).Scan(&metaExists); err != nil {
		return "", false, fmt.Errorf("inspect meta table: %w", err)
	}
	if metaExists == 0 {
		return "", false, nil
	}

	var version string
	if err := db.QueryRowContext(
		ctx,
		"SELECT value FROM meta WHERE key='schema_version'",
	).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read schema_version: %w", err)
	}
	return version, true, nil
}

func isSupportedSchemaVersion(version string) bool {
	switch version {
	case "1", "2", "3", "4", "5", "6", "7", "8", CurrentSchemaVersion:
		return true
	default:
		return false
	}
}

// Store owns the *sql.DB plus dimension-name → id caches.
//
// Cache strategy: every dim is INSERT-OR-IGNORE on first write and SELECT-d
// back to learn the assigned id, so id stability is guaranteed even when two
// goroutines race on the same name. Once seen, the (name, id) pair is pinned
// in the local sync.Map so steady-state writes only need a single SQL roundtrip
// (the events INSERT). The cache never invalidates because dim ids never change
// — rows are never deleted (FK ON DELETE RESTRICT) and ROWIDs are stable.
type Store struct {
	db           *sql.DB
	maxPageCount atomic.Int64

	modelCache      sync.Map // string → int64
	apiKeyCache     sync.Map
	sourceCache     sync.Map
	authIndexCache  sync.Map
	credentialCache sync.Map
	providerCache   sync.Map
}

// openStore opens (or creates) the SQLite database, applies PRAGMAs, runs the
// schema (idempotent thanks to IF NOT EXISTS), and validates schema_version.
//
// Connection limits: SQLite serializes writes regardless of pool size, so a
// large pool just inflates context overhead. Eight connections gives readers
// breathing room without wasting memory.
func openStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("usage: empty SQLite db path")
	}
	preflightVersion, hasVersion, err := inspectExistingSchemaVersion(dbPath)
	if err != nil {
		return nil, fmt.Errorf("usage: inspect SQLite schema %s: %w", dbPath, err)
	}
	if hasVersion && !isSupportedSchemaVersion(preflightVersion) {
		return nil, fmt.Errorf(
			"usage: unexpected schema_version=%s (want %s)",
			preflightVersion,
			CurrentSchemaVersion,
		)
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
		"&_pragma=temp_store(FILE)"

	store := &Store{}
	store.maxPageCount.Store(maxSQLitePageCount)
	sqliteDriver := &sqlite.Driver{}
	sqliteDriver.RegisterConnectionHook(
		func(conn sqlite.ExecQuerierContext, _ string) error {
			query := fmt.Sprintf("PRAGMA max_page_count = %d", store.maxPageCount.Load())
			_, errHook := conn.ExecContext(context.Background(), query, nil)
			return errHook
		},
	)
	db := sql.OpenDB(&sqliteConnector{driver: sqliteDriver, dsn: dsn})
	store.db = db
	db.SetMaxOpenConns(storeMaxOpenConnections)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), storeOpenTimeout)
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
	// were absent and let the schema apply step seed it. migrations must run
	// BEFORE schema.sql applies, otherwise current queries would reference columns
	// the pre-migration tables don't have yet.
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
	if version == "2" {
		if err := migrateV2ToV3(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v2→v3: %w", err)
		}
		version = "3"
	}
	if version == "3" {
		if err := migrateV3ToV4(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v3→v4: %w", err)
		}
		version = "4"
	}
	if version == "4" {
		if err := migrateV4ToV5(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v4→v5: %w", err)
		}
		version = "5"
	}
	if version == "5" {
		if err := migrateV5ToV6(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v5→v6: %w", err)
		}
		version = "6"
	}
	if version == "6" {
		if err := migrateV6ToV7(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v6→v7: %w", err)
		}
		version = "7"
	}
	if version == "7" {
		if err := migrateV7ToV8(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v7→v8: %w", err)
		}
		version = "8"
	}
	if version == "8" {
		if err := migrateV8ToV9(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("usage: migrate v8→v9: %w", err)
		}
		version = "9"
	}

	schemaTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: begin schema transaction: %w", err)
	}
	if _, err := schemaTx.ExecContext(ctx, schemaSQL); err != nil {
		_ = schemaTx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("usage: apply schema: %w", err)
	}
	if err := schemaTx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: commit schema transaction: %w", err)
	}

	// Re-read in case schema.sql seeded the version on a fresh database.
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: read schema_version (post-apply): %w", err)
	}

	if version != CurrentSchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("usage: unexpected schema_version=%s (want %s)", version, CurrentSchemaVersion)
	}
	if err := ensurePricingReceiptImmutabilityTrigger(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: enforce immutable pricing receipts: %w", err)
	}
	if err := ensureIntegerStorageTriggers(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: enforce integer storage: %w", err)
	}
	var maxRequestLogRevision sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MAX(revision) FROM request_log_artifact").Scan(
		&maxRequestLogRevision,
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: read maximum request log artifact revision: %w", err)
	}
	if maxRequestLogRevision.Valid {
		internallogging.AdvanceRequestLogArtifactRevision(maxRequestLogRevision.Int64)
	}

	return store, nil
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
func columnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid          int
			name         string
			colType      string
			notnull      int
			defaultValue sql.NullString
			pk           int
		)
		if err := rows.Scan(&cid, &name, &colType, &notnull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

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

	if _, err := tx.ExecContext(
		ctx,
		`CREATE TABLE IF NOT EXISTS dim_provider (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE CHECK(name = trim(name)) CHECK(length(name) > 0)
		)`,
	); err != nil {
		return fmt.Errorf("create dim_provider: %w", err)
	}

	// Add provider_id column to each fact table when missing. ALTER TABLE on
	// a table that already has the column would fail; the columnExists guard
	// keeps the migration idempotent for partial-completion replays.
	for _, table := range []string{"events", "hour_bucket", "day_bucket"} {
		exists, err := columnExists(ctx, tx, table, "provider_id")
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

func migrateV2ToV3(ctx context.Context, db *sql.DB) error {
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

	for _, table := range []string{"hour_bucket", "day_bucket"} {
		for _, column := range []string{"latency_ms_total", "latency_samples"} {
			exists, err := columnExists(ctx, tx, table, column)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			stmt := fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN %s INTEGER NOT NULL DEFAULT 0 CHECK(%s >= 0)", table, column, column,
			)
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("alter %s add %s: %w", table, column, err)
			}
		}
	}

	if err := backfillLatencyBuckets(ctx, tx, true); err != nil {
		return err
	}

	if _, err := tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '3' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	log.Infof("usage: migrated events.db schema v2 → v3 (latency aggregates)")
	return nil
}

func backfillLatencyBuckets(ctx context.Context, tx *sql.Tx, includeDay bool) error {
	var invalidMetadata int64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*)
		   FROM events
		  WHERE metadata IS NOT NULL
		    AND metadata <> ''
		    AND CASE
		        WHEN json_valid(metadata) = 0 THEN 1
		        ELSE
		            json_type(metadata, '$.latency_ms') IS NOT NULL AND
		            json_type(metadata, '$.latency_ms') <> 'null' AND
		            (
		                json_type(metadata, '$.latency_ms') <> 'integer' OR
		                typeof(json_extract(metadata, '$.latency_ms')) <> 'integer'
		            )
		    END`,
	).Scan(&invalidMetadata); err != nil {
		return fmt.Errorf("validate latency metadata: %w", err)
	}
	if invalidMetadata > 0 {
		log.Warnf("usage: skip latency backfill for %d invalid metadata records", invalidMetadata)
	}

	if err := backfillLatencyBucket(ctx, tx, "hour_bucket", nsPerHour); err != nil {
		return err
	}
	if includeDay {
		if err := backfillLatencyBucket(ctx, tx, "day_bucket", nsPerDay); err != nil {
			return err
		}
	}
	return nil
}

func backfillLatencyBucket(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	bucketSize int64,
) error {
	bucketExpr := sqliteFloorBucketExpr("ts_ns", bucketSize)
	statement := fmt.Sprintf(
		`
		WITH event_latency AS (
		    SELECT
		        ts_ns, model_id, credential_id, api_key_id, provider_id,
		        CASE WHEN json_valid(metadata)
		             THEN json_extract(metadata, '$.latency_ms')
		        END AS latency_ms,
		        CASE WHEN json_valid(metadata)
		             THEN json_type(metadata, '$.latency_ms')
		        END AS latency_type
		      FROM events
		     WHERE metadata IS NOT NULL AND metadata <> ''
		), latency AS (
		    SELECT
		        %s AS bucket_ts_ns,
		        model_id, credential_id, api_key_id, provider_id,
		        SUM(latency_ms) AS latency_ms_total,
		        COUNT(*) AS latency_samples
		      FROM event_latency
		     WHERE latency_type = 'integer'
		       AND typeof(latency_ms) = 'integer'
		       AND latency_ms > 0
		     GROUP BY %s, model_id, credential_id, api_key_id, provider_id
		)
		UPDATE %s AS bucket
		   SET latency_ms_total = latency.latency_ms_total,
		       latency_samples = latency.latency_samples
		  FROM latency
		 WHERE bucket.bucket_ts_ns = latency.bucket_ts_ns
		   AND bucket.model_id = latency.model_id
		   AND bucket.credential_id IS latency.credential_id
		   AND bucket.api_key_id IS latency.api_key_id
		   AND bucket.provider_id IS latency.provider_id
	`, bucketExpr, bucketExpr, table,
	)
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("backfill %s latency: %w", table, err)
	}
	return nil
}

func migrateV5ToV6(ctx context.Context, db *sql.DB) error {
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

	columns := map[string][]struct {
		name       string
		definition string
	}{
		"events": {
			{name: "cache_read_tokens", definition: "INTEGER NOT NULL DEFAULT 0 CHECK(cache_read_tokens >= 0)"},
			{name: "cache_write_tokens", definition: "INTEGER NOT NULL DEFAULT 0 CHECK(cache_write_tokens >= 0)"},
			{
				name:       "cost_status",
				definition: "TEXT NOT NULL DEFAULT 'unpriced' CHECK(cost_status IN ('complete','partial','unpriced'))",
			},
			{name: "cost_reason", definition: "TEXT NOT NULL DEFAULT ''"},
		},
		"hour_bucket": {
			{name: "cache_read_tokens", definition: "INTEGER NOT NULL DEFAULT 0 CHECK(cache_read_tokens >= 0)"},
			{name: "cache_write_tokens", definition: "INTEGER NOT NULL DEFAULT 0 CHECK(cache_write_tokens >= 0)"},
		},
		"day_bucket": {
			{name: "cache_read_tokens", definition: "INTEGER NOT NULL DEFAULT 0 CHECK(cache_read_tokens >= 0)"},
			{name: "cache_write_tokens", definition: "INTEGER NOT NULL DEFAULT 0 CHECK(cache_write_tokens >= 0)"},
		},
	}
	for table, definitions := range columns {
		for _, column := range definitions {
			exists, errColumn := columnExists(ctx, tx, table, column.name)
			if errColumn != nil {
				return errColumn
			}
			if exists {
				continue
			}
			statement := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column.name, column.definition)
			if _, errColumn = tx.ExecContext(ctx, statement); errColumn != nil {
				return fmt.Errorf("alter %s add %s: %w", table, column.name, errColumn)
			}
		}
	}
	if _, err = tx.ExecContext(
		ctx,
		"CREATE TABLE IF NOT EXISTS dirty_day (day_ts_ns INTEGER PRIMARY KEY) WITHOUT ROWID",
	); err != nil {
		return fmt.Errorf("create dirty_day: %w", err)
	}

	if _, err = tx.ExecContext(
		ctx,
		`UPDATE events
		    SET cache_read_tokens = cached_tokens,
		        cache_write_tokens = 0,
		        cost_status = CASE WHEN cost_micro > 0 THEN 'partial' ELSE 'unpriced' END,
		        cost_reason = CASE
		            WHEN cost_micro > 0 THEN 'legacy pricing did not preserve component status'
		            ELSE 'legacy event has no applied price'
		        END`,
	); err != nil {
		return fmt.Errorf("backfill legacy cost state: %w", err)
	}

	if _, err = tx.ExecContext(
		ctx,
		`UPDATE events
		    SET input_tokens = json_extract(metadata, '$.token_breakdown.input.total_tokens'),
		        output_tokens = json_extract(metadata, '$.token_breakdown.output.total_tokens'),
		        reasoning_tokens = json_extract(metadata, '$.token_breakdown.output.reasoning_tokens'),
		        cache_read_tokens = json_extract(metadata, '$.token_breakdown.input.cache_read_tokens'),
		        cache_write_tokens = json_extract(metadata, '$.token_breakdown.input.cache_write_tokens'),
		        cached_tokens = json_extract(metadata, '$.token_breakdown.input.cache_read_tokens') +
		            json_extract(metadata, '$.token_breakdown.input.cache_write_tokens'),
		        total_tokens = json_extract(metadata, '$.token_breakdown.total_tokens')
		  WHERE json_valid(metadata)
		    AND json_extract(metadata, '$.accounting_version') = 2
		    AND json_extract(metadata, '$.token_breakdown.schema_version') = 2
		    AND json_extract(metadata, '$.token_breakdown.quality') = 'complete'
		    AND json_type(metadata, '$.token_breakdown.input.total_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.input.uncached_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.input.cache_read_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.input.cache_write_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.output.total_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.output.non_reasoning_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.output.reasoning_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.unclassified_tokens') = 'integer'
		    AND json_type(metadata, '$.token_breakdown.total_tokens') = 'integer'
		    AND json_extract(metadata, '$.token_breakdown.input.total_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.input.uncached_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.input.cache_read_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.input.cache_write_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.output.total_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.output.non_reasoning_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.output.reasoning_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.unclassified_tokens') = 0
		    AND json_extract(metadata, '$.token_breakdown.total_tokens') >= 0
		    AND json_extract(metadata, '$.token_breakdown.input.total_tokens') =
		        json_extract(metadata, '$.token_breakdown.input.uncached_tokens') +
		        json_extract(metadata, '$.token_breakdown.input.cache_read_tokens') +
		        json_extract(metadata, '$.token_breakdown.input.cache_write_tokens')
		    AND json_extract(metadata, '$.token_breakdown.output.total_tokens') =
		        json_extract(metadata, '$.token_breakdown.output.non_reasoning_tokens') +
		        json_extract(metadata, '$.token_breakdown.output.reasoning_tokens')
		    AND json_extract(metadata, '$.token_breakdown.total_tokens') =
		        json_extract(metadata, '$.token_breakdown.input.total_tokens') +
		        json_extract(metadata, '$.token_breakdown.output.total_tokens')`,
	); err != nil {
		return fmt.Errorf("backfill canonical token buckets: %w", err)
	}

	// The immediately following v6→v7 migration rebuilds both bucket tables
	// after adding cost-coverage columns.
	if _, err = tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '6' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	log.Infof("usage: migrated events.db schema v5 → v6 (canonical cache buckets and cost state)")
	return nil
}

func migrateV6ToV7(ctx context.Context, db *sql.DB) error {
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

	columns := []string{
		"complete_requests",
		"partial_requests",
		"unpriced_requests",
		"complete_tokens",
		"partial_tokens",
		"unpriced_tokens",
	}
	for _, table := range []string{"hour_bucket", "day_bucket"} {
		for _, column := range columns {
			exists, errColumn := columnExists(ctx, tx, table, column)
			if errColumn != nil {
				return errColumn
			}
			if exists {
				continue
			}
			statement := fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN %s INTEGER NOT NULL DEFAULT 0 CHECK(%s >= 0)",
				table,
				column,
				column,
			)
			if _, errColumn = tx.ExecContext(ctx, statement); errColumn != nil {
				return fmt.Errorf("alter %s add %s: %w", table, column, errColumn)
			}
		}
		for _, operation := range []string{"insert", "update"} {
			if _, err = tx.ExecContext(
				ctx,
				"DROP TRIGGER IF EXISTS usage_"+table+"_integer_"+operation,
			); err != nil {
				return fmt.Errorf("drop %s integer %s trigger: %w", table, operation, err)
			}
		}
	}
	if err = rebuildUsageBucketsTx(ctx, tx); err != nil {
		return fmt.Errorf("rebuild usage buckets for schema v7: %w", err)
	}
	if _, err = tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '7' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	log.Infof("usage: migrated events.db schema v6 → v7 (cost coverage aggregates)")
	return nil
}

type apiKeyIdentityMigration struct {
	id        int64
	temporary string
	target    string
}

func migrateAPIKeyIdentitiesV8(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, name FROM dim_api_key ORDER BY id")
	if err != nil {
		return fmt.Errorf("query API key identities: %w", err)
	}
	migrations := make([]apiKeyIdentityMigration, 0)
	occupied := make(map[string]struct{})
	targetOwner := make(map[string]int64)
	for rows.Next() {
		var id int64
		var name string
		if err = rows.Scan(&id, &name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan API key identity: %w", err)
		}
		name = strings.TrimSpace(name)
		occupied[name] = struct{}{}
		target := coreusage.EnsureAPIKeyIdentity(name)
		if owner, duplicate := targetOwner[target]; duplicate {
			_ = rows.Close()
			return fmt.Errorf("API key identities %d and %d resolve to the same persistent identity", owner, id)
		}
		targetOwner[target] = id
		if target != name {
			migrations = append(migrations, apiKeyIdentityMigration{id: id, target: target})
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate API key identities: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close API key identity rows: %w", err)
	}
	if len(migrations) == 0 {
		return nil
	}
	for index := range migrations {
		candidate := fmt.Sprintf("__cpa_v8_api_key_migration__:%d", migrations[index].id)
		for {
			if _, exists := occupied[candidate]; !exists {
				break
			}
			candidate += ":"
		}
		migrations[index].temporary = candidate
		occupied[candidate] = struct{}{}
		if _, err = tx.ExecContext(
			ctx,
			"UPDATE dim_api_key SET name = ? WHERE id = ?",
			candidate,
			migrations[index].id,
		); err != nil {
			return fmt.Errorf("stage API key identity %d migration: %w", migrations[index].id, err)
		}
	}
	for _, migration := range migrations {
		if _, err = tx.ExecContext(
			ctx,
			"UPDATE dim_api_key SET name = ? WHERE id = ? AND name = ?",
			migration.target,
			migration.id,
			migration.temporary,
		); err != nil {
			return fmt.Errorf("persist API key identity %d: %w", migration.id, err)
		}
	}
	if err = migrateEventFingerprintsV2(ctx, tx); err != nil {
		return fmt.Errorf("recompute API key event fingerprints: %w", err)
	}
	return nil
}

func migrateV7ToV8(ctx context.Context, db *sql.DB) error {
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
	if _, err = tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create API key reservation schema: %w", err)
	}
	if err = migrateAPIKeyIdentitiesV8(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '8' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	log.Infof("usage: migrated events.db schema v7 → v8 (API key reservations)")
	return nil
}

func migrateV8ToV9(ctx context.Context, db *sql.DB) error {
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
	hasPricingReceipt, err := columnExists(ctx, tx, "events", "pricing_receipt")
	if err != nil {
		return err
	}
	if !hasPricingReceipt {
		if _, err = tx.ExecContext(
			ctx, `ALTER TABLE events ADD COLUMN pricing_receipt TEXT CHECK (
			pricing_receipt IS NULL OR (
				typeof(pricing_receipt) = 'text' AND
				length(CAST(pricing_receipt AS BLOB)) <= 16384 AND
				json_valid(pricing_receipt)
			)
		)`,
		); err != nil {
			return fmt.Errorf("add events.pricing_receipt: %w", err)
		}
	}
	if err = ensurePricingReceiptImmutabilityTrigger(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '9' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	log.Infof("usage: migrated events.db schema v8 → v9 (immutable pricing receipts)")
	return nil
}

func ensurePricingReceiptImmutabilityTrigger(ctx context.Context, exec dbExec) error {
	if exec == nil {
		return errors.New("usage: create pricing receipt trigger on nil database")
	}
	_, err := exec.ExecContext(
		ctx, `CREATE TRIGGER IF NOT EXISTS usage_events_pricing_receipt_immutable
		BEFORE UPDATE OF pricing_receipt ON events
		WHEN OLD.pricing_receipt IS NOT NEW.pricing_receipt
	BEGIN
		SELECT RAISE(ABORT, 'events.pricing_receipt is immutable');
	END`,
	)
	if err != nil {
		return fmt.Errorf("create immutable pricing receipt trigger: %w", err)
	}
	return nil
}

// Close releases the underlying database. Safe to call on nil.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// database exposes the underlying *sql.DB for writer / query / import packages.
func (s *Store) database() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) setMaxSizeBytes(ctx context.Context, maxBytes int64) error {
	if s == nil || s.db == nil {
		return errors.New("usage: set max size on nil store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var pageSize int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return fmt.Errorf("usage: query SQLite page size: %w", err)
	}
	if pageSize <= 0 {
		return errors.New("usage: SQLite returned an invalid page size")
	}
	maxPages := maxSQLitePageCount
	if maxBytes > 0 {
		maxPages = max(maxBytes/pageSize, 1)
	}
	s.maxPageCount.Store(maxPages)
	appliedPages, err := s.applyMaxPageCountToConnections(ctx, maxPages)
	if err != nil {
		return err
	}
	if maxBytes > 0 && appliedPages > maxPages {
		log.Warnf(
			"usage: SQLite database already uses %d pages, above configured maximum %d; future growth is blocked",
			appliedPages,
			maxPages,
		)
	}
	return nil
}

func (s *Store) applyMaxPageCountToConnections(ctx context.Context, maxPages int64) (int64, error) {
	connections := make([]*sql.Conn, 0, storeMaxOpenConnections)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()

	query := fmt.Sprintf("PRAGMA max_page_count = %d", maxPages)
	appliedPages := maxPages
	for range storeMaxOpenConnections {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return 0, fmt.Errorf("usage: acquire SQLite connection for page limit: %w", err)
		}
		connections = append(connections, conn)
		var applied int64
		if err = conn.QueryRowContext(ctx, query).Scan(&applied); err != nil {
			return 0, fmt.Errorf("usage: set SQLite max page count: %w", err)
		}
		if applied > appliedPages {
			appliedPages = applied
		}
	}
	return appliedPages, nil
}

// dbExec is the minimal interface satisfied by both *sql.DB and *sql.Tx so
// dim resolution works inside or outside a transaction without duplication.
type dbExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// resolveDimID is the shared get-or-create path for every dim_* table.
// An empty (or whitespace-only) name returns ok=false without touching the database
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
	if _, inTransaction := exec.(*sql.Tx); !inTransaction {
		cache.Store(name, id)
	}
	return id, true, nil
}

// ResolveModelID returns the dim_model id, falling back to "unknown" when the
// caller passes an empty model name. Model is the only dim that is NOT NULL on
// the events table, so we never return NullInt64 here.
func (s *Store) warmDimensionCaches(ctx context.Context, details []FlatDetail) error {
	if s == nil || s.db == nil || len(details) == 0 {
		return nil
	}
	type candidate struct {
		cache *sync.Map
		table string
		name  string
	}
	seen := make(map[string]struct{})
	for i := range details {
		detail := &details[i]
		model := strings.TrimSpace(detail.Model)
		if model == "" {
			model = "unknown"
		}
		candidates := []candidate{
			{cache: &s.modelCache, table: "dim_model", name: model},
			{cache: &s.apiKeyCache, table: "dim_api_key", name: strings.TrimSpace(detail.APIKey)},
			{cache: &s.sourceCache, table: "dim_source", name: strings.TrimSpace(detail.Source)},
			{cache: &s.authIndexCache, table: "dim_auth_index", name: strings.TrimSpace(detail.AuthIndex)},
			{cache: &s.credentialCache, table: "dim_credential", name: credentialName(detail.Source, detail.AuthIndex)},
			{cache: &s.providerCache, table: "dim_provider", name: strings.TrimSpace(detail.Provider)},
		}
		for _, item := range candidates {
			if item.name == "" {
				continue
			}
			key := item.table + "\x00" + item.name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if _, ok := item.cache.Load(item.name); ok {
				continue
			}
			var id int64
			if err := s.db.QueryRowContext(
				ctx,
				"SELECT id FROM "+item.table+" WHERE name = ?",
				item.name,
			).Scan(&id); err != nil {
				return fmt.Errorf("usage: warm %s cache for %q: %w", item.table, item.name, err)
			}
			item.cache.Store(item.name, id)
		}
	}
	return nil
}

func (s *Store) resolveModelID(ctx context.Context, exec dbExec, name string) (int64, error) {
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

// resolveAPIKeyID returns the dim_api_key id, or a NULL when name is blank.
func (s *Store) resolveAPIKeyID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.apiKeyCache, "dim_api_key", name)
}

// resolveSourceID returns the dim_source id, or a NULL when name is blank.
func (s *Store) resolveSourceID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.sourceCache, "dim_source", name)
}

// resolveAuthIndexID returns the dim_auth_index id, or a NULL when name is blank.
func (s *Store) resolveAuthIndexID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.authIndexCache, "dim_auth_index", name)
}

// resolveCredentialID returns the dim_credential id for COALESCE(source, auth_index).
// Both inputs blank → NULL credential, matching summary.go::credKey semantics.
func (s *Store) resolveCredentialID(ctx context.Context, exec dbExec, source, authIndex string) (sql.NullInt64, error) {
	return s.resolveNullableDim(ctx, exec, &s.credentialCache, "dim_credential", credentialName(source, authIndex))
}

// resolveProviderID returns the dim_provider id, or a NULL when name is blank.
// Old v1/v2 JSON imports never carried provider, so a blank string is the
// expected steady-state for backfilled rows — the NULL surfaces in /usage
// queries as the "(unknown)" bucket so the dimension chart doesn't drop them.
func (s *Store) resolveProviderID(ctx context.Context, exec dbExec, name string) (sql.NullInt64, error) {
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

// computeFingerprint produces a 20-byte digest over an unambiguous, versioned payload
func computeFingerprint(d FlatDetail) [eventFingerprintBytes]byte {
	tokens := normaliseTokenStats(d.Tokens)
	var payload bytes.Buffer
	payload.WriteString("usage-fingerprint-v2")
	writeFingerprintString(&payload, d.RequestID)
	writeFingerprintString(&payload, d.APIKey)
	writeFingerprintString(&payload, d.Model)
	writeFingerprintString(&payload, d.Timestamp.UTC().Format(time.RFC3339Nano))
	writeFingerprintString(&payload, d.Source)
	writeFingerprintString(&payload, d.AuthIndex)
	writeFingerprintString(&payload, d.Provider)
	if d.Failed {
		payload.WriteByte(1)
	} else {
		payload.WriteByte(0)
	}
	writeFingerprintInt64(&payload, tokens.InputTokens)
	writeFingerprintInt64(&payload, tokens.OutputTokens)
	writeFingerprintInt64(&payload, tokens.ReasoningTokens)
	writeFingerprintInt64(&payload, tokens.CachedTokens)
	writeFingerprintInt64(&payload, tokens.TotalTokens)
	digest := sha256.Sum256(payload.Bytes())
	var fingerprint [eventFingerprintBytes]byte
	copy(fingerprint[:], digest[:len(fingerprint)])
	return fingerprint
}

func writeFingerprintString(payload *bytes.Buffer, value string) {
	writeFingerprintInt64(payload, int64(len(value)))
	payload.WriteString(value)
}

func writeFingerprintInt64(payload *bytes.Buffer, value int64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], uint64(value))
	payload.Write(encoded[:])
}
