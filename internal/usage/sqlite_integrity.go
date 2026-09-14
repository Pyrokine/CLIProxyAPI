package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type integerStorageColumn struct {
	name     string
	nullable bool
}

type integerStorageTable struct {
	name    string
	columns []integerStorageColumn
}

var integerStorageTables = []integerStorageTable{
	{
		name: "events",
		columns: []integerStorageColumn{
			{name: "ts_ns"},
			{name: "model_id"},
			{name: "api_key_id", nullable: true},
			{name: "source_id", nullable: true},
			{name: "auth_index_id", nullable: true},
			{name: "credential_id", nullable: true},
			{name: "provider_id", nullable: true},
			{name: "failed"},
			{name: "input_tokens"},
			{name: "output_tokens"},
			{name: "reasoning_tokens"},
			{name: "cached_tokens"},
			{name: "cache_read_tokens"},
			{name: "cache_write_tokens"},
			{name: "total_tokens"},
			{name: "cost_micro"},
		},
	},
	{
		name: "hour_bucket",
		columns: []integerStorageColumn{
			{name: "bucket_ts_ns"},
			{name: "model_id"},
			{name: "credential_id", nullable: true},
			{name: "api_key_id", nullable: true},
			{name: "provider_id", nullable: true},
			{name: "requests"},
			{name: "success"},
			{name: "failure"},
			{name: "input_tokens"},
			{name: "output_tokens"},
			{name: "reasoning_tokens"},
			{name: "cached_tokens"},
			{name: "cache_read_tokens"},
			{name: "cache_write_tokens"},
			{name: "total_tokens"},
			{name: "cost_micro"},
			{name: "complete_requests"},
			{name: "partial_requests"},
			{name: "unpriced_requests"},
			{name: "complete_tokens"},
			{name: "partial_tokens"},
			{name: "unpriced_tokens"},
			{name: "latency_ms_total"},
			{name: "latency_samples"},
		},
	},
	{
		name: "day_bucket",
		columns: []integerStorageColumn{
			{name: "bucket_ts_ns"},
			{name: "model_id"},
			{name: "credential_id", nullable: true},
			{name: "api_key_id", nullable: true},
			{name: "provider_id", nullable: true},
			{name: "requests"},
			{name: "success"},
			{name: "failure"},
			{name: "input_tokens"},
			{name: "output_tokens"},
			{name: "reasoning_tokens"},
			{name: "cached_tokens"},
			{name: "cache_read_tokens"},
			{name: "cache_write_tokens"},
			{name: "total_tokens"},
			{name: "cost_micro"},
			{name: "complete_requests"},
			{name: "partial_requests"},
			{name: "unpriced_requests"},
			{name: "complete_tokens"},
			{name: "partial_tokens"},
			{name: "unpriced_tokens"},
			{name: "latency_ms_total"},
			{name: "latency_samples"},
		},
	},
	{
		name: "api_key_reservation",
		columns: []integerStorageColumn{
			{name: "created_at_ns"},
			{name: "expires_at_ns"},
			{name: "updated_at_ns"},
			{name: "terminal_at_ns", nullable: true},
		},
	},
	{
		name: "api_key_reservation_window",
		columns: []integerStorageColumn{
			{name: "window_ns"},
			{name: "window_start_ns"},
			{name: "window_end_ns"},
			{name: "max_requests"},
			{name: "max_tokens"},
			{name: "max_cost_micro"},
			{name: "reserved_requests"},
			{name: "reserved_tokens"},
			{name: "reserved_cost_micro"},
			{name: "actual_requests"},
			{name: "actual_tokens"},
			{name: "actual_cost_micro"},
		},
	},
	{
		name: "api_key_reservation_event",
		columns: []integerStorageColumn{
			{name: "total_tokens"},
			{name: "cost_micro"},
			{name: "created_at_ns"},
		},
	},
}

func migrateV3ToV4(ctx context.Context, db *sql.DB) error {
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

	for _, table := range integerStorageTables {
		existing := integerStorageTable{name: table.name}
		for _, column := range table.columns {
			exists, errColumn := columnExists(ctx, tx, table.name, column.name)
			if errColumn != nil {
				return errColumn
			}
			if exists {
				existing.columns = append(existing.columns, column)
			}
		}
		if len(existing.columns) == 0 {
			continue
		}
		predicate := integerStoragePredicate(existing, "")
		var count int64
		if err := tx.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM "+table.name+" WHERE "+predicate,
		).Scan(&count); err != nil {
			return fmt.Errorf("validate %s integer storage: %w", table.name, err)
		}
		if count != 0 {
			return fmt.Errorf("%s contains %d rows with non-integer storage classes", table.name, count)
		}
	}
	if err := migrateEventFingerprintsV2(ctx, tx); err != nil {
		return err
	}

	if _, err := tx.ExecContext(
		ctx,
		"UPDATE meta SET value = '4' WHERE key = 'schema_version'",
	); err != nil {
		return fmt.Errorf("update schema_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	return nil
}

const fingerprintMigrationPageSize = 512

type eventFingerprintMigration struct {
	id     int64
	detail FlatDetail
}

func migrateEventFingerprintsV2(ctx context.Context, tx *sql.Tx) error {
	update, err := tx.PrepareContext(ctx, "UPDATE events SET fingerprint = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare event fingerprint migration update: %w", err)
	}
	defer func() { _ = update.Close() }()

	var afterID int64
	for {
		rows, errQuery := tx.QueryContext(
			ctx,
			`SELECT
    e.id,
    e.ts_ns,
    m.name,
    COALESCE(k.name, ''),
    COALESCE(src.name, ''),
    COALESCE(ai.name, ''),
    COALESCE(p.name, ''),
    e.failed,
    e.input_tokens,
    e.output_tokens,
    e.reasoning_tokens,
    e.cached_tokens,
    e.total_tokens,
    e.metadata
FROM events e
JOIN dim_model m ON m.id = e.model_id
LEFT JOIN dim_api_key k ON k.id = e.api_key_id
LEFT JOIN dim_source src ON src.id = e.source_id
LEFT JOIN dim_auth_index ai ON ai.id = e.auth_index_id
LEFT JOIN dim_provider p ON p.id = e.provider_id
WHERE e.id > ?
ORDER BY e.id
LIMIT ?`,
			afterID,
			fingerprintMigrationPageSize,
		)
		if errQuery != nil {
			return fmt.Errorf("query event fingerprints for migration: %w", errQuery)
		}
		batch := make([]eventFingerprintMigration, 0, fingerprintMigrationPageSize)
		for rows.Next() {
			var (
				item      eventFingerprintMigration
				timestamp int64
				failed    int64
				metadata  sql.NullString
			)
			if errScan := rows.Scan(
				&item.id,
				&timestamp,
				&item.detail.Model,
				&item.detail.APIKey,
				&item.detail.Source,
				&item.detail.AuthIndex,
				&item.detail.Provider,
				&failed,
				&item.detail.Tokens.InputTokens,
				&item.detail.Tokens.OutputTokens,
				&item.detail.Tokens.ReasoningTokens,
				&item.detail.Tokens.CachedTokens,
				&item.detail.Tokens.TotalTokens,
				&metadata,
			); errScan != nil {
				_ = rows.Close()
				return fmt.Errorf("scan event fingerprint migration row: %w", errScan)
			}
			item.detail.Timestamp = time.Unix(0, timestamp).UTC()
			item.detail.Failed = failed != 0
			if errMetadata := decodeEventMetadata(&item.detail, metadata); errMetadata != nil {
				_ = rows.Close()
				return fmt.Errorf("decode event %d metadata for fingerprint migration: %w", item.id, errMetadata)
			}
			batch = append(batch, item)
			afterID = item.id
		}
		if errRows := rows.Err(); errRows != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate event fingerprints for migration: %w", errRows)
		}
		if errClose := rows.Close(); errClose != nil {
			return fmt.Errorf("close event fingerprint migration rows: %w", errClose)
		}
		for i := range batch {
			fingerprint := computeFingerprint(batch[i].detail)
			if _, errUpdate := update.ExecContext(ctx, fingerprint[:], batch[i].id); errUpdate != nil {
				return fmt.Errorf("update event %d fingerprint: %w", batch[i].id, errUpdate)
			}
		}
		if len(batch) < fingerprintMigrationPageSize {
			return nil
		}
	}
}

func ensureIntegerStorageTriggers(ctx context.Context, db *sql.DB) error {
	for _, table := range integerStorageTables {
		predicate := integerStoragePredicate(table, "NEW.")
		for _, operation := range []string{"INSERT", "UPDATE"} {
			name := fmt.Sprintf("usage_%s_integer_%s", table.name, strings.ToLower(operation))
			statement := fmt.Sprintf(
				`CREATE TRIGGER IF NOT EXISTS %s
BEFORE %s ON %s
WHEN %s
BEGIN
    SELECT RAISE(ABORT, '%s requires integer storage classes');
END`,
				name,
				operation,
				table.name,
				predicate,
				table.name,
			)
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		}
	}
	return nil
}

func integerStoragePredicate(table integerStorageTable, prefix string) string {
	predicates := make([]string, 0, len(table.columns))
	for _, column := range table.columns {
		name := prefix + column.name
		if column.nullable {
			predicates = append(
				predicates,
				fmt.Sprintf("(%s IS NOT NULL AND typeof(%s) <> 'integer')", name, name),
			)
			continue
		}
		predicates = append(predicates, fmt.Sprintf("typeof(%s) <> 'integer'", name))
	}
	return strings.Join(predicates, " OR ")
}
