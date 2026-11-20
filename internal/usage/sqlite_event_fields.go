package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	eventFieldsVersion      = "1"
	eventFieldsVersionKey   = "event_fields_version"
	eventFieldsCursorKey    = "event_fields_cursor"
	eventFieldsTaskKey      = "event_fields_task_v1"
	eventFieldsPageSize     = 128
	storageUpgradeStatusSQL = `SELECT
		(SELECT value FROM meta WHERE key=?),
		(SELECT value FROM meta WHERE key=?),
		(SELECT COUNT(*) FROM event_fields),
		(SELECT COUNT(*) FROM events)`
)

// StorageUpgradeStatus reports current row coverage and recorded maintenance times.
type StorageUpgradeStatus struct {
	Status          string   `json:"status"`
	Running         bool     `json:"running"`
	ProcessedEvents *int64   `json:"processed_events,omitempty"`
	TotalEvents     *int64   `json:"total_events,omitempty"`
	Percent         *float64 `json:"percent,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	UpdatedAt       string   `json:"updated_at,omitempty"`
	CompletedAt     string   `json:"completed_at,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// StorageUpgradeStatusContext reads both counts and the cursor in one SQLite snapshot.
func (p *Persister) StorageUpgradeStatusContext(parent context.Context) (StorageUpgradeStatus, error) {
	if p == nil {
		return StorageUpgradeStatus{Status: "idle"}, nil
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return StorageUpgradeStatus{}, errors.New("usage: store not initialized")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	return s.queryStorageUpgradeStatus(ctx)
}

func (s *sqliteStore) queryStorageUpgradeStatus(ctx context.Context) (StorageUpgradeStatus, error) {
	var cursor, raw sql.NullString
	var processed, total int64
	if err := s.db.QueryRowContext(ctx, storageUpgradeStatusSQL, eventFieldsCursorKey, eventFieldsTaskKey).
		Scan(&cursor, &raw, &processed, &total); err != nil {
		return StorageUpgradeStatus{}, fmt.Errorf("usage: read storage upgrade status: %w", err)
	}
	if !cursor.Valid {
		return StorageUpgradeStatus{}, errors.New("usage: storage upgrade cursor missing")
	}
	status := StorageUpgradeStatus{Status: "running", Running: true}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &status); err != nil {
			return StorageUpgradeStatus{}, fmt.Errorf("usage: decode storage upgrade status: %w", err)
		}
	}
	if cursor.String == "complete" {
		status.Status, status.Running, status.Error = "complete", false, ""
	} else {
		if cursor.String != "" {
			if _, err := strconv.ParseInt(cursor.String, 10, 64); err != nil {
				return StorageUpgradeStatus{}, fmt.Errorf("usage: invalid storage upgrade cursor: %w", err)
			}
		}
		if s.eventFieldsRunning.Load() {
			status.Status, status.Running, status.Error = "running", true, ""
		}
		if failure := s.eventFieldsFailure.Load(); failure != nil {
			status.Status, status.Running = "error", false
			status.Error, status.UpdatedAt = failure.Error, failure.UpdatedAt
		}
	}
	percent := float64(0)
	if total > 0 {
		percent = float64(processed) * 100 / float64(total)
	} else if status.Status == "complete" {
		percent = 100
	}
	status.ProcessedEvents, status.TotalEvents, status.Percent = &processed, &total, &percent
	return status, nil
}

func readEventFieldsUpgradeTask(ctx context.Context, q costTaskQueryer) (StorageUpgradeStatus, bool, error) {
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", eventFieldsTaskKey).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StorageUpgradeStatus{}, false, nil
		}
		return StorageUpgradeStatus{}, false, err
	}
	var status StorageUpgradeStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return StorageUpgradeStatus{}, false, fmt.Errorf("decode event fields task: %w", err)
	}
	return status, true, nil
}

func writeEventFieldsUpgradeTask(ctx context.Context, q costTaskExecer, status StorageUpgradeStatus) error {
	encoded, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(
		ctx, "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		eventFieldsTaskKey, string(encoded),
	)
	return err
}

func (p *Persister) checkEventFieldsUpgradeCapacity() error {
	maximum := p.dbSizeCapLimit()
	if maximum == 0 {
		return nil
	}
	size, err := p.DBSizeWithError()
	if err != nil {
		return fmt.Errorf("measure storage upgrade capacity: %w", err)
	}
	reserve := min(maximum/100, int64(64<<20))
	if size >= maximum-reserve {
		return fmt.Errorf(
			"structured storage upgrade paused to reserve capacity for live usage: database and sidecars use %d bytes of %d bytes, reserve %d bytes; increase the size limit or free capacity, then restart to resume",
			size, maximum, reserve,
		)
	}
	return nil
}

func (s *sqliteStore) recordEventFieldsUpgradeError(failure error) {
	status := StorageUpgradeStatus{Status: "error", Error: failure.Error(), UpdatedAt: costTaskNow()}
	// Retain the observed failure even when a full or locked database rejects its meta write.
	s.eventFieldsFailure.Store(&status)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	previous, _, err := readEventFieldsUpgradeTask(ctx, s.db)
	if err == nil {
		if previous.Status == "complete" {
			s.eventFieldsFailure.Store(nil)
			return
		}
		status.StartedAt = previous.StartedAt
		err = writeEventFieldsUpgradeTask(ctx, s.db, status)
	}
	if err != nil {
		log.Errorf("usage: persist structured event upgrade failure: %v; observed failure: %v", err, failure)
	}
}

var eventCoreColumns = []string{
	"id", "ts_ns", "model_id", "api_key_id", "source_id", "auth_index_id", "credential_id", "provider_id",
	"failed", "input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens",
	"cache_write_tokens", "total_tokens", "cost_micro", "cost_status", "cost_reason",
}

var eventMetadataColumns = []string{
	"latency_ms", "request_id", "time_to_first_byte_ms", "total_duration_ms", "completed",
	"reasoning_effort", "thinking", "service_tier", "request_service_tier", "response_service_tier",
	"client_model", "upstream_url", "upstream_model", "response_model", "response_provider",
	"response_provider_source", "request_speed", "response_speed", "executor_type",
}

func eventFieldsColumns() []string {
	columns := append([]string{}, eventCoreColumns...)
	columns = append(columns, eventMetadataColumns...)
	return append(columns, "metadata_state", "observed_speed", "pricing_metadata")
}

func eventMetadataValueSQL(prefix, field string) string {
	return "CASE WHEN json_valid(" + prefix + "metadata) THEN json_extract(" + prefix + "metadata,'$." + field + "') END"
}

func eventFieldsSelectSQL(prefix string) string {
	values := make([]string, 0, len(eventFieldsColumns()))
	for _, field := range eventCoreColumns {
		values = append(values, prefix+field)
	}
	for _, field := range eventMetadataColumns {
		values = append(values, eventMetadataValueSQL(prefix, field)+" AS "+field)
	}
	values = append(
		values,
		"CASE WHEN "+prefix+"metadata IS NULL OR "+prefix+"metadata='' THEN 0 WHEN json_valid("+prefix+"metadata) THEN 1 ELSE -1 END AS metadata_state",
		strings.ReplaceAll(observedRequestSpeedSQL(), "events.", prefix)+" AS observed_speed",
		eventPricingMetadataSQL(prefix)+" AS pricing_metadata",
	)
	return strings.Join(values, ",")
}

const pricingMetadataProjectionPrefix = `{"_pricing_projection":2`

// Retain duplicate and case-variant accounting keys for encoding/json's existing decoding rules.
func eventPricingMetadataSQL(prefix string) string {
	metadata := prefix + "metadata"
	return "CASE WHEN json_valid(" + metadata + ") THEN CASE WHEN json_type(" + metadata + ")='object' THEN " +
		"(SELECT '" + pricingMetadataProjectionPrefix + "' || CASE WHEN projected='{}' THEN '}' ELSE ',' || substr(projected,2) END " +
		"FROM (SELECT json_group_object(key,json(CASE type WHEN 'text' THEN json_quote(value) " +
		"WHEN 'true' THEN 'true' WHEN 'false' THEN 'false' WHEN 'null' THEN 'null' " +
		"ELSE value END)) AS projected FROM (SELECT key,type,value FROM json_each(" + metadata + ") " +
		"WHERE lower(key) IN ('accounting_version','token_breakdown','request_speed','response_speed'," +
		"'response_service_tier','executor_type','billing_metadata','cache_read_reported','cache_write_reported'," +
		"'pricing_receipt_v2') OR key GLOB '*[^ -~]*' ORDER BY id))) ELSE " + metadata + " END ELSE " + metadata + " END"
}

// Old projections lack billing evidence; read their canonical metadata only within the selected page.
func eventPricingMetadataReadSQL(eventPrefix, fieldsPrefix string) string {
	projected := fieldsPrefix + "pricing_metadata"
	return "CASE WHEN " + fieldsPrefix + "id IS NOT NULL AND substr(" + projected + ",1," +
		strconv.Itoa(len(pricingMetadataProjectionPrefix)) + ")='" + pricingMetadataProjectionPrefix + "' THEN " +
		projected + " ELSE " + eventPrefix + "metadata END"
}

func ensureEventFields(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version string
	if err := tx.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key=?", eventFieldsVersionKey,
	).Scan(&version); err != nil && err != sql.ErrNoRows {
		return err
	}
	if version != "" && version != eventFieldsVersion {
		return fmt.Errorf("unsupported event fields layout %q (want %s)", version, eventFieldsVersion)
	}
	columns := []string{"id INTEGER PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE ON UPDATE CASCADE"}
	for _, field := range eventCoreColumns[1:] {
		kind := " INTEGER"
		if field == "cost_status" || field == "cost_reason" {
			kind = " TEXT"
		}
		columns = append(columns, field+kind)
	}
	// No affinity preserves json_extract's integer, real, text and NULL LIKE semantics.
	columns = append(columns, eventMetadataColumns...)
	columns = append(
		columns, "metadata_state INTEGER NOT NULL", "observed_speed TEXT NOT NULL", "pricing_metadata TEXT",
	)
	if _, err := tx.ExecContext(
		ctx, "CREATE TABLE IF NOT EXISTS event_fields("+strings.Join(columns, ",")+")",
	); err != nil {
		return fmt.Errorf("create event fields: %w", err)
	}
	for _, field := range []string{
		"model_id", "api_key_id", "source_id", "auth_index_id", "credential_id", "provider_id", "failed",
		"observed_speed",
	} {
		if _, err := tx.ExecContext(
			ctx, "CREATE INDEX IF NOT EXISTS idx_event_fields_"+field+"_ts ON event_fields("+field+",ts_ns)",
		); err != nil {
			return err
		}
	}
	for _, field := range []string{"ts_ns", "total_tokens", "request_id"} {
		if _, err := tx.ExecContext(
			ctx, "CREATE INDEX IF NOT EXISTS idx_event_fields_"+field+" ON event_fields("+field+")",
		); err != nil {
			return err
		}
	}
	insert := "INSERT INTO event_fields(" + strings.Join(
		eventFieldsColumns(), ",",
	) + ") SELECT " + eventFieldsSelectSQL("NEW.")
	updates := make([]string, 0, len(eventFieldsColumns())-1)
	for _, field := range eventFieldsColumns()[1:] {
		updates = append(updates, field+"=excluded."+field)
	}
	insert += " ON CONFLICT(id) DO UPDATE SET " + strings.Join(updates, ",") + ";"
	coreUpdates := make([]string, 0, len(eventCoreColumns)-1)
	var coreEvents, costUpdates []string
	for _, field := range eventCoreColumns[1:] {
		update := field + "=NEW." + field
		coreUpdates = append(coreUpdates, update)
		switch field {
		case "cost_micro", "cost_status", "cost_reason":
			costUpdates = append(costUpdates, update)
		default:
			coreEvents = append(coreEvents, field)
		}
	}
	for _, trigger := range []struct{ name, event, body string }{
		{"insert", "INSERT", insert},
		{"delete", "DELETE", "DELETE FROM event_fields WHERE id=OLD.id;"},
		{
			"metadata_update", "UPDATE OF id,metadata,provider_id",
			"DELETE FROM event_fields WHERE id=OLD.id AND OLD.id<>NEW.id;" + insert,
		},
		{
			"core_update", "UPDATE OF " + strings.Join(coreEvents, ","),
			"UPDATE event_fields SET " + strings.Join(coreUpdates, ",") + " WHERE id=NEW.id;",
		},
		{
			"cost_update", "UPDATE OF cost_micro,cost_status,cost_reason",
			"UPDATE event_fields SET " + strings.Join(costUpdates, ",") + " WHERE id=NEW.id;",
		},
	} {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS event_fields_"+trigger.name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			"CREATE TRIGGER event_fields_"+trigger.name+" AFTER "+trigger.event+" ON events BEGIN "+trigger.body+" END",
		); err != nil {
			return fmt.Errorf("create event fields %s trigger: %w", trigger.name, err)
		}
	}
	if _, err := tx.ExecContext(
		ctx, "INSERT OR IGNORE INTO meta(key,value) VALUES(?,?)", eventFieldsVersionKey, eventFieldsVersion,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		"INSERT OR IGNORE INTO meta(key,value) SELECT ?,CASE WHEN EXISTS(SELECT 1 FROM events LIMIT 1) THEN '' ELSE 'complete' END",
		eventFieldsCursorKey,
	); err != nil {
		return err
	}
	if version == "" {
		if err := writeEventFieldsUpgradeTask(ctx, tx, StorageUpgradeStatus{Status: "idle"}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func eventFieldsSource(ctx context.Context, q dbQueryer) (string, error) {
	var cursor string
	if err := q.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil {
		return "", fmt.Errorf("read event fields upgrade cursor: %w", err)
	}
	if cursor == "complete" {
		return "event_fields", nil
	}
	if cursor != "" {
		if _, err := strconv.ParseInt(cursor, 10, 64); err != nil {
			return "", fmt.Errorf("invalid event fields cursor: %w", err)
		}
	}
	// A single canonical source lets SQLite apply its time index and LIMIT before projecting the page.
	return "(SELECT " + eventFieldsSelectSQL("events.") + ",events.metadata FROM events)", nil
}

// Each committed cursor covers exactly the same transaction's rows; original events and summaries stay untouched.
func (s *sqliteStore) backfillEventFieldsPage(ctx context.Context) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the write reservation before reading the cursor to avoid WAL snapshot upgrade races.
	if _, err := tx.ExecContext(ctx, "UPDATE meta SET value=value WHERE key=?", eventFieldsCursorKey); err != nil {
		return false, err
	}
	var cursor string
	if err := tx.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil {
		return false, err
	}
	if cursor == "complete" {
		return true, tx.Commit()
	}
	status, recorded, err := readEventFieldsUpgradeTask(ctx, tx)
	if err != nil {
		return false, err
	}
	// Only a newly recorded task has a known start; older pending upgrades retain an absent start time.
	if recorded && status.Status == "idle" && status.StartedAt == "" {
		status.StartedAt = costTaskNow()
	}
	where := ""
	args := []any{}
	if cursor != "" {
		id, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return false, fmt.Errorf("invalid event fields cursor: %w", err)
		}
		where = " WHERE id>?"
		args = append(args, id)
	}
	args = append(args, eventFieldsPageSize)
	rows, err := tx.QueryContext(ctx, "SELECT id FROM events"+where+" ORDER BY id LIMIT ?", args...)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(ids) == 0 {
		cursor = "complete"
	} else {
		if s.eventFieldsCapacityCheck != nil {
			if err := s.eventFieldsCapacityCheck(); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(
			ctx, "INSERT INTO event_fields("+strings.Join(eventFieldsColumns(), ",")+") SELECT "+
				eventFieldsSelectSQL("events.")+" FROM events WHERE id>=? AND id<=? AND NOT EXISTS(SELECT 1 FROM event_fields WHERE id=events.id)",
			ids[0], ids[len(ids)-1],
		); err != nil {
			return false, fmt.Errorf("backfill event fields: %w", err)
		}
		cursor = strconv.FormatInt(ids[len(ids)-1], 10)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE meta SET value=? WHERE key=?", cursor, eventFieldsCursorKey); err != nil {
		return false, err
	}
	status.Status, status.Running, status.Error = "running", true, ""
	status.UpdatedAt = costTaskNow()
	if cursor == "complete" {
		status.Status, status.Running = "complete", false
		status.CompletedAt = status.UpdatedAt
	}
	if err := writeEventFieldsUpgradeTask(ctx, tx, status); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.eventFieldsFailure.Store(nil)
	return cursor == "complete", nil
}

func (s *sqliteStore) startEventFieldsUpgrade(ctx context.Context) func() {
	done := make(chan struct{})
	s.eventFieldsFailure.Store(nil)
	s.eventFieldsRunning.Store(true)
	go func() {
		defer close(done)
		defer s.eventFieldsRunning.Store(false)
		for {
			pageCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			complete, err := s.backfillEventFieldsPage(pageCtx)
			cancel()
			if complete {
				log.Info("usage: structured event upgrade complete")
				return
			}
			if ctx.Err() != nil {
				s.recordEventFieldsUpgradeError(fmt.Errorf("structured event upgrade interrupted: %w", ctx.Err()))
				return
			}
			if err != nil {
				log.Errorf("usage: structured event upgrade paused; committed cursor retained for restart: %v", err)
				s.recordEventFieldsUpgradeError(err)
				if !isTransientWriterError(err) {
					return
				}
			}
			delay := 10 * time.Millisecond
			if err != nil {
				delay = time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				s.recordEventFieldsUpgradeError(fmt.Errorf("structured event upgrade interrupted: %w", ctx.Err()))
				return
			case <-timer.C:
			}
		}
	}()
	return func() { <-done }
}
