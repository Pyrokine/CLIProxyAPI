package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	_ "modernc.org/sqlite"
)

const (
	managementStateSchemaVersion = "4"
	managementStateFileMode      = 0o600
)

type managementStateStore struct {
	db   *sql.DB
	path string
}

type accountInspectionPersistedState struct {
	Schedule   accountInspectionSchedule   `json:"schedule"`
	Status     string                      `json:"status"`
	CurrentRun *accountInspectionRun       `json:"current_run,omitempty"`
	LastRun    *accountInspectionRun       `json:"last_run,omitempty"`
	Runs       []accountInspectionRun      `json:"runs"`
	Results    []accountInspectionResult   `json:"results"`
	Logs       []accountInspectionLogEntry `json:"logs"`
	UpdatedAt  time.Time                   `json:"updated_at"`
}

func openManagementStateStore(configFilePath string) (*managementStateStore, error) {
	dbPath := resolveManagementStateDBPath(configFilePath)
	if dbPath == "" {
		return nil, nil
	}
	return openManagementStateStorePath(dbPath)
}

func openManagementStateStorePath(dbPath string) (*managementStateStore, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, fmt.Errorf("management state: sqlite path is empty")
	}
	var err error
	dbPath, err = filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("management state: resolve sqlite path: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("management state: create dir: %w", err)
	}
	dbFile, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, managementStateFileMode)
	if err != nil {
		return nil, fmt.Errorf("management state: create sqlite file %s: %w", dbPath, err)
	}
	if err = dbFile.Chmod(managementStateFileMode); err != nil {
		_ = dbFile.Close()
		return nil, fmt.Errorf("management state: secure sqlite file %s: %w", dbPath, err)
	}
	if err = dbFile.Close(); err != nil {
		return nil, fmt.Errorf("management state: close sqlite file %s: %w", dbPath, err)
	}
	dsn := dbPath +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("management state: open sqlite %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("management state: ping sqlite %s: %w", dbPath, err)
	}
	store := &managementStateStore{db: db, path: dbPath}
	if err := store.applySchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := secureManagementStateFiles(dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func secureManagementStateFiles(dbPath string) error {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(path, managementStateFileMode); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("management state: secure sqlite file %s: %w", path, err)
		}
	}
	return nil
}

func resolveManagementStateDBPath(configFilePath string) string {
	if wp := util.WritablePath(); wp != "" {
		return filepath.Join(wp, "management-state", "management-state.db")
	}
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configFilePath), "management-state.db")
}

func (s *managementStateStore) applySchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS state_items (key TEXT PRIMARY KEY, value BLOB NOT NULL, updated_at TEXT NOT NULL) WITHOUT ROWID",
		`CREATE TABLE IF NOT EXISTS quota_entries_v2 (
			entry_key TEXT PRIMARY KEY,
			file_name TEXT NOT NULL,
			type TEXT NOT NULL,
			auth_index TEXT NOT NULL,
			credential_revision TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL DEFAULT 1,
			state_revision INTEGER NOT NULL DEFAULT 1,
			tombstone INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			freshness TEXT NOT NULL DEFAULT 'outdated',
			source TEXT NOT NULL DEFAULT '',
			observed_at TEXT,
			fetched_at TEXT,
			last_success_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			last_error_at TEXT,
			retry_at TEXT,
			successful_empty INTEGER NOT NULL DEFAULT 0,
			last_refresh TEXT,
			next_refresh TEXT,
			error TEXT NOT NULL DEFAULT '',
			failure_count INTEGER NOT NULL DEFAULT 0,
			data BLOB,
			disabled INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			CHECK(generation > 0),
			CHECK(state_revision > 0),
			CHECK(tombstone IN (0, 1)),
			CHECK(successful_empty IN (0, 1))
		) WITHOUT ROWID`,
		"INSERT INTO meta(key, value) VALUES('schema_version', '4') ON CONFLICT(key) DO NOTHING",
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("management state: apply schema: %w", err)
		}
	}
	var version string
	if err := s.db.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key='schema_version'",
	).Scan(&version); err != nil {
		return fmt.Errorf("management state: read schema version: %w", err)
	}
	if version == "1" {
		if err := s.migrateQuotaEntriesV2(ctx); err != nil {
			return err
		}
		version = "2"
	}
	if version == "2" {
		if err := s.migrateManagementMutationAuditV3(ctx); err != nil {
			return err
		}
		version = "3"
	}
	if version == "3" {
		if err := s.migrateQuotaFreshnessV4(ctx); err != nil {
			return err
		}
		version = managementStateSchemaVersion
	}
	if version != managementStateSchemaVersion {
		return fmt.Errorf("management state: unexpected schema_version=%s", version)
	}
	if err := s.applyManagementMutationAuditSchema(ctx); err != nil {
		return err
	}
	return nil
}

func (s *managementStateStore) migrateQuotaEntriesV2(ctx context.Context) error {
	var tableName string
	err := s.db.QueryRowContext(
		ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='quota_entries'",
	).Scan(&tableName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("management state: inspect quota_entries: %w", err)
	}
	if tableName == "quota_entries" {
		_, err = s.db.ExecContext(
			ctx,
			`INSERT INTO quota_entries_v2(
				entry_key, file_name, type, auth_index, status, last_refresh, next_refresh, error, failure_count, data, disabled, updated_at
			 )
			 SELECT type || '::' || file_name, file_name, type, auth_index, status, last_refresh, next_refresh, error, failure_count, data, disabled, updated_at
			 FROM quota_entries
			 WHERE type <> '' AND file_name <> ''
			 ON CONFLICT(entry_key) DO NOTHING`,
		)
		if err != nil {
			return fmt.Errorf("management state: migrate quota entries: %w", err)
		}
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE meta SET value='2' WHERE key='schema_version'"); err != nil {
		return fmt.Errorf("management state: update schema version: %w", err)
	}
	return nil
}

func (s *managementStateStore) migrateQuotaFreshnessV4(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("management state: begin quota freshness migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statements := []string{
		"DROP TABLE IF EXISTS quota_entries_v4",
		`CREATE TABLE quota_entries_v4 (
			entry_key TEXT PRIMARY KEY,
			file_name TEXT NOT NULL,
			type TEXT NOT NULL,
			auth_index TEXT NOT NULL,
			credential_revision TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL DEFAULT 1,
			state_revision INTEGER NOT NULL DEFAULT 1,
			tombstone INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			freshness TEXT NOT NULL DEFAULT 'outdated',
			source TEXT NOT NULL DEFAULT '',
			observed_at TEXT,
			fetched_at TEXT,
			last_success_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			last_error_at TEXT,
			retry_at TEXT,
			successful_empty INTEGER NOT NULL DEFAULT 0,
			last_refresh TEXT,
			next_refresh TEXT,
			error TEXT NOT NULL DEFAULT '',
			failure_count INTEGER NOT NULL DEFAULT 0,
			data BLOB,
			disabled INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			CHECK(generation > 0),
			CHECK(state_revision > 0),
			CHECK(tombstone IN (0, 1)),
			CHECK(successful_empty IN (0, 1))
		) WITHOUT ROWID`,
		`INSERT INTO quota_entries_v4(
			entry_key, file_name, type, auth_index, credential_revision,
			generation, state_revision, tombstone, status, freshness, source,
			observed_at, fetched_at, last_success_at, last_error, last_error_at,
			retry_at, successful_empty, last_refresh, next_refresh, error,
			failure_count, data, disabled, updated_at
		 )
		 SELECT entry_key, file_name, type, auth_index, '',
			1, 1, 0, status,
			CASE
				WHEN status = 'success' AND length(data) > 0 THEN 'fresh'
				WHEN length(data) > 0 THEN 'stale'
				ELSE 'outdated'
			END,
			'',
			CASE WHEN status = 'success' AND length(data) > 0 THEN last_refresh END,
			last_refresh,
			CASE WHEN status = 'success' AND length(data) > 0 THEN last_refresh END,
			error,
			CASE WHEN error <> '' THEN last_refresh END,
			CASE WHEN status <> 'success' THEN next_refresh END,
			0, last_refresh, next_refresh, error, failure_count, data, disabled, updated_at
		 FROM quota_entries_v2`,
		"DROP TABLE quota_entries_v2",
		"ALTER TABLE quota_entries_v4 RENAME TO quota_entries_v2",
		"UPDATE meta SET value='4' WHERE key='schema_version'",
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("management state: migrate quota freshness: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("management state: commit quota freshness migration: %w", err)
	}
	return nil
}

func (s *managementStateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *managementStateStore) loadAccountInspection(ctx context.Context) (*accountInspectionPersistedState, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key='account_inspection'").Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("management state: load account inspection: %w", err)
	}
	var state accountInspectionPersistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("management state: decode account inspection: %w", err)
	}
	return &state, nil
}

func (s *managementStateStore) saveAccountInspection(ctx context.Context, state accountInspectionPersistedState) error {
	if s == nil || s.db == nil {
		return nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("management state: encode account inspection: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO state_items(key, value, updated_at) VALUES('account_inspection', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		raw,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("management state: save account inspection: %w", err)
	}
	return nil
}

func (s *managementStateStore) loadQuotaRefreshTasks(ctx context.Context) (*quotaRefreshTasksPersistedState, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key='quota_refresh_tasks'").Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("management state: load quota refresh tasks: %w", err)
	}
	if len(raw) > maxQuotaRefreshTaskStateBytes {
		return nil, fmt.Errorf(
			"management state: quota refresh task state is %d bytes, maximum is %d",
			len(raw),
			maxQuotaRefreshTaskStateBytes,
		)
	}
	var state quotaRefreshTasksPersistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("management state: decode quota refresh tasks: %w", err)
	}
	return &state, nil
}

func (s *managementStateStore) saveQuotaRefreshTasks(
	ctx context.Context,
	state quotaRefreshTasksPersistedState,
) error {
	if s == nil || s.db == nil {
		return errQuotaRefreshTaskPersistenceUnavailable
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("management state: encode quota refresh tasks: %w", err)
	}
	if len(raw) > maxQuotaRefreshTaskStateBytes {
		return fmt.Errorf(
			"management state: encoded quota refresh task state is %d bytes, maximum is %d",
			len(raw),
			maxQuotaRefreshTaskStateBytes,
		)
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO state_items(key, value, updated_at) VALUES('quota_refresh_tasks', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		raw,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("management state: save quota refresh tasks: %w", err)
	}
	return nil
}

func (s *managementStateStore) loadRateLimitState(ctx context.Context) (*rateLimitPersistedState, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state_items WHERE key='management_rate_limit'").Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("management state: load rate limit state: %w", err)
	}
	if len(raw) > maxManagementRateLimitStateBytes {
		return nil, fmt.Errorf(
			"management state: rate limit state is %d bytes, maximum is %d",
			len(raw),
			maxManagementRateLimitStateBytes,
		)
	}
	var state rateLimitPersistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("management state: decode rate limit state: %w", err)
	}
	return &state, nil
}

func (s *managementStateStore) saveRateLimitState(ctx context.Context, state rateLimitPersistedState) error {
	if s == nil || s.db == nil {
		return nil
	}
	if state.Revision <= 0 {
		return fmt.Errorf("management state: rate limit revision must be positive")
	}
	if len(state.Attempts) > maxManagementRateLimitClients ||
		len(state.ActiveBans) > maxManagementRateLimitClients {
		return fmt.Errorf(
			"management state: rate limit client count exceeds maximum %d",
			maxManagementRateLimitClients,
		)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("management state: encode rate limit state: %w", err)
	}
	if len(raw) > maxManagementRateLimitStateBytes {
		return fmt.Errorf(
			"management state: encoded rate limit state is %d bytes, maximum is %d",
			len(raw),
			maxManagementRateLimitStateBytes,
		)
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO state_items(key, value, updated_at) VALUES('management_rate_limit', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
		 WHERE COALESCE(
		     CASE WHEN json_valid(state_items.value)
		         THEN CAST(json_extract(state_items.value, '$.revision') AS INTEGER)
		     END,
		     0
		 ) < CAST(json_extract(excluded.value, '$.revision') AS INTEGER)`,
		raw,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("management state: save rate limit state: %w", err)
	}
	return nil
}

func (s *managementStateStore) loadQuotaEntries(ctx context.Context) (map[string]quota.EntryState, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT entry_key, file_name, type, auth_index, credential_revision,
			generation, state_revision, tombstone, status, freshness, source,
			observed_at, fetched_at, last_success_at, last_error, last_error_at,
			retry_at, successful_empty, last_refresh, next_refresh, error,
			failure_count, data, disabled
		 FROM quota_entries_v2`,
	)
	if err != nil {
		return nil, fmt.Errorf("management state: load quota entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make(map[string]quota.EntryState)
	for rows.Next() {
		var (
			entry           quota.EntryState
			entryKey        string
			generation      int64
			stateRevision   int64
			tombstone       int
			observedAt      sql.NullString
			fetchedAt       sql.NullString
			lastSuccessAt   sql.NullString
			lastErrorAt     sql.NullString
			retryAt         sql.NullString
			successfulEmpty int
			lastRefresh     sql.NullString
			nextRefresh     sql.NullString
			data            []byte
			disabled        int
		)
		if err := rows.Scan(
			&entryKey,
			&entry.FileName,
			&entry.Type,
			&entry.AuthIndex,
			&entry.CredentialRevision,
			&generation,
			&stateRevision,
			&tombstone,
			&entry.Status,
			&entry.Freshness,
			&entry.Source,
			&observedAt,
			&fetchedAt,
			&lastSuccessAt,
			&entry.LastError,
			&lastErrorAt,
			&retryAt,
			&successfulEmpty,
			&lastRefresh,
			&nextRefresh,
			&entry.Error,
			&entry.FailureCount,
			&data,
			&disabled,
		); err != nil {
			return nil, fmt.Errorf("management state: scan quota entry: %w", err)
		}
		if generation <= 0 || stateRevision <= 0 {
			return nil, fmt.Errorf("management state: invalid quota generation %d/%d", generation, stateRevision)
		}
		entry.Generation = uint64(generation)
		entry.StateRevision = uint64(stateRevision)
		entry.Tombstone = tombstone != 0
		entry.SuccessfulEmpty = successfulEmpty != 0
		entry.Disabled = disabled != 0
		entry.Data = append(entry.Data[:0], data...)
		if entry.ObservedAt, err = parseOptionalQuotaTime("observed_at", observedAt); err != nil {
			return nil, err
		}
		if entry.FetchedAt, err = parseOptionalQuotaTime("fetched_at", fetchedAt); err != nil {
			return nil, err
		}
		if entry.LastSuccessAt, err = parseOptionalQuotaTime("last_success_at", lastSuccessAt); err != nil {
			return nil, err
		}
		if entry.LastErrorAt, err = parseOptionalQuotaTime("last_error_at", lastErrorAt); err != nil {
			return nil, err
		}
		if entry.RetryAt, err = parseOptionalQuotaTime("retry_at", retryAt); err != nil {
			return nil, err
		}
		if entry.LastRefresh, err = parseOptionalQuotaTime("last_refresh", lastRefresh); err != nil {
			return nil, err
		}
		if entry.NextRefresh, err = parseOptionalQuotaTime("next_refresh", nextRefresh); err != nil {
			return nil, err
		}
		if entryKey == "" {
			entryKey = quota.EntryKey(entry.Type, entry.FileName)
		}
		if entryKey != "" {
			entries[entryKey] = entry
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("management state: iterate quota entries: %w", err)
	}
	return entries, nil
}

func (s *managementStateStore) saveQuotaEntry(ctx context.Context, entry quota.EntryState) error {
	entryKey := quota.EntryKey(entry.Type, entry.FileName)
	if s == nil || s.db == nil || entryKey == "" {
		return nil
	}
	if entry.Generation == 0 || entry.StateRevision == 0 {
		return fmt.Errorf(
			"management state: quota generation must be positive, got %d/%d",
			entry.Generation,
			entry.StateRevision,
		)
	}
	if entry.Tombstone {
		entry.Status = quota.StatusIdle
		entry.Freshness = quota.FreshnessOutdated
		entry.Source = ""
		entry.ObservedAt = nil
		entry.FetchedAt = nil
		entry.LastSuccessAt = nil
		entry.LastError = ""
		entry.LastErrorAt = nil
		entry.RetryAt = nil
		entry.SuccessfulEmpty = false
		entry.LastRefresh = nil
		entry.NextRefresh = nil
		entry.Error = ""
		entry.FailureCount = 0
		entry.Data = nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO quota_entries_v2(
			entry_key, file_name, type, auth_index, credential_revision,
			generation, state_revision, tombstone, status, freshness, source,
			observed_at, fetched_at, last_success_at, last_error, last_error_at,
			retry_at, successful_empty, last_refresh, next_refresh, error,
			failure_count, data, disabled, updated_at
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(entry_key) DO UPDATE SET
			file_name=excluded.file_name,
			type=excluded.type,
			auth_index=excluded.auth_index,
			credential_revision=excluded.credential_revision,
			generation=excluded.generation,
			state_revision=excluded.state_revision,
			tombstone=excluded.tombstone,
			status=excluded.status,
			freshness=excluded.freshness,
			source=excluded.source,
			observed_at=excluded.observed_at,
			fetched_at=excluded.fetched_at,
			last_success_at=excluded.last_success_at,
			last_error=excluded.last_error,
			last_error_at=excluded.last_error_at,
			retry_at=excluded.retry_at,
			successful_empty=excluded.successful_empty,
			last_refresh=excluded.last_refresh,
			next_refresh=excluded.next_refresh,
			error=excluded.error,
			failure_count=excluded.failure_count,
			data=excluded.data,
			disabled=excluded.disabled,
			updated_at=excluded.updated_at
		 WHERE quota_entries_v2.generation < excluded.generation
			OR (quota_entries_v2.generation = excluded.generation
				AND quota_entries_v2.state_revision < excluded.state_revision)`,
		entryKey,
		entry.FileName,
		string(entry.Type),
		entry.AuthIndex,
		entry.CredentialRevision,
		entry.Generation,
		entry.StateRevision,
		boolToInt(entry.Tombstone),
		string(entry.Status),
		string(entry.Freshness),
		entry.Source,
		formatOptionalTime(entry.ObservedAt),
		formatOptionalTime(entry.FetchedAt),
		formatOptionalTime(entry.LastSuccessAt),
		entry.LastError,
		formatOptionalTime(entry.LastErrorAt),
		formatOptionalTime(entry.RetryAt),
		boolToInt(entry.SuccessfulEmpty),
		formatOptionalTime(entry.LastRefresh),
		formatOptionalTime(entry.NextRefresh),
		entry.Error,
		entry.FailureCount,
		[]byte(entry.Data),
		boolToInt(entry.Disabled),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("management state: save quota entry: %w", err)
	}
	return nil
}

func (s *managementStateStore) deleteQuotaEntry(ctx context.Context, entry quota.EntryState) error {
	if !entry.Tombstone {
		return fmt.Errorf("management state: quota deletion requires a tombstone")
	}
	if err := s.saveQuotaEntry(ctx, entry); err != nil {
		return fmt.Errorf("management state: save quota tombstone: %w", err)
	}
	return nil
}

func parseOptionalQuotaTime(name string, value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, fmt.Errorf("management state: parse quota %s: %w", name, err)
	}
	return &parsed, nil
}

func formatOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
