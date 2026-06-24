package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	_ "modernc.org/sqlite"
)

const managementStateSchemaVersion = "1"

type managementStateStore struct {
	db *sql.DB
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
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("management state: create dir: %w", err)
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
	store := &managementStateStore{db: db}
	if err := store.applySchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
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
		`CREATE TABLE IF NOT EXISTS quota_entries (
			file_name TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			auth_index TEXT NOT NULL,
			status TEXT NOT NULL,
			last_refresh TEXT,
			next_refresh TEXT,
			error TEXT NOT NULL DEFAULT '',
			failure_count INTEGER NOT NULL DEFAULT 0,
			data BLOB,
			disabled INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		) WITHOUT ROWID`,
		"INSERT INTO meta(key, value) VALUES('schema_version', '1') ON CONFLICT(key) DO NOTHING",
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
	if version != managementStateSchemaVersion {
		return fmt.Errorf("management state: unexpected schema_version=%s", version)
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
		if err == sql.ErrNoRows {
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

func (s *managementStateStore) loadQuotaEntries(ctx context.Context) (map[string]quota.EntryState, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT file_name, type, auth_index, status, last_refresh, next_refresh, error, failure_count, data, disabled
		 FROM quota_entries`,
	)
	if err != nil {
		return nil, fmt.Errorf("management state: load quota entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make(map[string]quota.EntryState)
	for rows.Next() {
		var (
			entry        quota.EntryState
			lastRefresh  sql.NullString
			nextRefresh  sql.NullString
			data         []byte
			disabled     int
			failureCount int
		)
		if err := rows.Scan(
			&entry.FileName,
			&entry.Type,
			&entry.AuthIndex,
			&entry.Status,
			&lastRefresh,
			&nextRefresh,
			&entry.Error,
			&failureCount,
			&data,
			&disabled,
		); err != nil {
			return nil, fmt.Errorf("management state: scan quota entry: %w", err)
		}
		entry.FailureCount = failureCount
		entry.Disabled = disabled != 0
		entry.Data = append(entry.Data[:0], data...)
		if lastRefresh.Valid && lastRefresh.String != "" {
			parsed, err := time.Parse(time.RFC3339Nano, lastRefresh.String)
			if err != nil {
				return nil, fmt.Errorf("management state: parse quota last_refresh: %w", err)
			}
			entry.LastRefresh = &parsed
		}
		if nextRefresh.Valid && nextRefresh.String != "" {
			parsed, err := time.Parse(time.RFC3339Nano, nextRefresh.String)
			if err != nil {
				return nil, fmt.Errorf("management state: parse quota next_refresh: %w", err)
			}
			entry.NextRefresh = &parsed
		}
		entries[entry.FileName] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("management state: iterate quota entries: %w", err)
	}
	return entries, nil
}

func (s *managementStateStore) saveQuotaEntry(ctx context.Context, entry quota.EntryState) error {
	if s == nil || s.db == nil || strings.TrimSpace(entry.FileName) == "" {
		return nil
	}
	lastRefresh := formatOptionalTime(entry.LastRefresh)
	nextRefresh := formatOptionalTime(entry.NextRefresh)
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO quota_entries(
			file_name, type, auth_index, status, last_refresh, next_refresh, error, failure_count, data, disabled, updated_at
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(file_name) DO UPDATE SET
			type=excluded.type,
			auth_index=excluded.auth_index,
			status=excluded.status,
			last_refresh=excluded.last_refresh,
			next_refresh=excluded.next_refresh,
			error=excluded.error,
			failure_count=excluded.failure_count,
			data=excluded.data,
			disabled=excluded.disabled,
			updated_at=excluded.updated_at`,
		entry.FileName,
		string(entry.Type),
		entry.AuthIndex,
		string(entry.Status),
		lastRefresh,
		nextRefresh,
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

func (s *managementStateStore) deleteQuotaEntry(ctx context.Context, fileName string) error {
	if s == nil || s.db == nil || strings.TrimSpace(fileName) == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM quota_entries WHERE file_name=?", fileName); err != nil {
		return fmt.Errorf("management state: delete quota entry: %w", err)
	}
	return nil
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
