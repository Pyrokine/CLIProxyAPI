// Last compiled: 2026-05-07
// Author: pyro

package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	dataDirName = "usage-data"

	// -1 means unlimited retention (no automatic cleanup).
	// Configure via config.yaml usage-retention.days to override.
	defaultRetentionDays = -1
)

// PriceFunc returns the pricing for a model: prices are in USD per 1M tokens.
type PriceFunc func(model string) (prompt, completion, cache float64, found bool)

// Persister is the SQLite-backed coordinator for usage data. It owns the
// Store + Writer + rollup goroutine, exposes a stable surface to the rest of
// the codebase, and hides the schema / WAL / dim_table details. Public method
// signatures match the JSON-era persister 1:1 so handler / SDK / test code
// keeps compiling unchanged.
type Persister struct {
	mu sync.RWMutex

	store    *Store
	writer   *Writer
	stopOnce sync.Once
	stopFn   func()

	pricesFn  PriceFunc
	baseDir   string
	retention config.UsageRetention

	costRecalcRunning atomic.Bool
}

// NewPersister constructs a Persister rooted at baseDir. A zero-valued Days
// is filled with the default (-1, disabled).
func NewPersister(baseDir string, retention config.UsageRetention) *Persister {
	if retention.Days == 0 {
		retention.Days = defaultRetentionDays
	}
	if retention.WarningThresholdPct == 0 {
		retention.WarningThresholdPct = 80
	}
	return &Persister{
		baseDir:   baseDir,
		retention: retention,
	}
}

// SetPriceFunc registers (or replaces) the pricing function.
func (p *Persister) SetPriceFunc(fn PriceFunc) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.pricesFn = fn
	w := p.writer
	p.mu.Unlock()
	if w != nil {
		w.SetPriceFunc(fn)
	}
}

// HasPricing reports whether a price lookup function has been configured.
func (p *Persister) HasPricing() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pricesFn != nil
}

// BaseDir returns the usage data directory.
func (p *Persister) BaseDir() string {
	if p == nil {
		return ""
	}
	return p.baseDir
}

// IsReady reports whether Start successfully opened the SQLite store.
// Callers (sdk/cliproxy/service.go) should branch on this immediately after
// Start: when not ready, they MUST call SetGlobalPersister(nil) so the
// LoggerPlugin's fallback to in-memory RequestStatistics kicks in. Without
// that, Record() silently drops every event behind the nil-writer guard.
func (p *Persister) IsReady() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.store != nil && p.writer != nil
}

// DBSize returns the on-disk size of events.db plus its WAL/SHM sidecars.
// Returns 0 when the store is not yet open or the file does not exist.
func (p *Persister) DBSize() int64 {
	if p == nil || p.baseDir == "" {
		return 0
	}
	var total int64
	for _, name := range []string{dbFileName, dbFileName + "-wal", dbFileName + "-shm"} {
		info, err := os.Stat(filepath.Join(p.baseDir, name))
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

func (p *Persister) maxDBSizeBytes() int64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.retention.MaxDBSizeMB <= 0 {
		return 0
	}
	return int64(p.retention.MaxDBSizeMB) << 20
}

func (p *Persister) dbSizeStatus() (
	sizeBytes int64,
	maxBytes int64,
	warningThresholdPct int,
	warning bool,
	capped bool,
) {
	sizeBytes = p.DBSize()
	if p == nil {
		return sizeBytes, 0, 0, false, false
	}
	p.mu.RLock()
	warningThresholdPct = p.retention.WarningThresholdPct
	maxDBSizeMB := p.retention.MaxDBSizeMB
	p.mu.RUnlock()
	if warningThresholdPct == 0 {
		warningThresholdPct = 80
	}
	if maxDBSizeMB <= 0 {
		return sizeBytes, 0, warningThresholdPct, false, false
	}
	maxBytes = int64(maxDBSizeMB) << 20
	capped = sizeBytes >= maxBytes
	warning = sizeBytes*100 >= maxBytes*int64(warningThresholdPct)
	return sizeBytes, maxBytes, warningThresholdPct, warning, capped
}

func (p *Persister) DBSizeStatus() (
	sizeBytes int64,
	maxBytes int64,
	warningThresholdPct int,
	warning bool,
	capped bool,
) {
	return p.dbSizeStatus()
}

func (p *Persister) overDBSizeCap() bool {
	_, _, _, _, capped := p.dbSizeStatus()
	return capped
}

// SchemaVersion returns the current schema_version meta value.
func (p *Persister) SchemaVersion(ctx context.Context) (string, error) {
	if p == nil {
		return "", errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return "", errors.New("usage: store not initialized")
	}
	var version string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("usage: query schema_version: %w", err)
	}
	return version, nil
}

// MigrationStatus returns the persisted legacy JSON migration marker.
func (p *Persister) MigrationStatus(ctx context.Context) (MigrationStatus, error) {
	if p == nil {
		return MigrationStatus{}, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return MigrationStatus{}, errors.New("usage: store not initialized")
	}
	return CheckMigrationStatus(ctx, s)
}

// Start opens the SQLite store, performs the legacy-JSON migration when
// needed, then launches the writer goroutine and the day-bucket rollup loop.
// Failure to open the store is logged with a clear "degraded mode" warning;
// the caller MUST check IsReady() afterwards and fall back to the in-memory
// RequestStatistics path (by calling SetGlobalPersister(nil)) when not ready,
// otherwise records will be silently dropped via Record's nil-writer guard.
func (p *Persister) Start(ctx context.Context) {
	if p == nil || p.baseDir == "" {
		return
	}

	if err := os.MkdirAll(p.baseDir, 0o700); err != nil {
		log.Errorf(
			"usage: failed to create data directory %s: %v — falling back to in-memory request statistics; no usage data will be persisted across restarts",
			p.baseDir, err,
		)
		return
	}

	dbPath := filepath.Join(p.baseDir, dbFileName)
	store, err := OpenStore(dbPath)
	if err != nil {
		log.Errorf(
			"usage: failed to open SQLite store %s: %v — falling back to in-memory request statistics; no usage data will be persisted across restarts; legacy summary.json / detail/* JSON files are NOT touched, so manual migration is still possible after the underlying issue is fixed",
			dbPath, err,
		)
		return
	}

	if err := MigrateLegacyToSQLite(ctx, store, p.priceFn(), p.baseDir); err != nil {
		log.Errorf("usage: legacy migration failed: %v", err)
		// Migration failure is recoverable — the user can fix the file and
		// restart. Continue serving with whatever we already have on disk.
	}

	writer := NewWriter(store, p.priceFn())
	writerCtx, writerCancel := context.WithCancel(ctx)
	writer.Start(writerCtx)

	rollupStop := store.StartRollup(ctx)

	p.mu.Lock()
	p.store = store
	p.writer = writer
	p.stopFn = func() {
		writerCancel()
		writer.Stop()
		rollupStop()
		_ = store.Close()
	}
	p.mu.Unlock()

	log.Infof("usage: SQLite store opened at %s", dbPath)
}

// Stop drains the writer, halts the rollup loop, and closes the store. Idempotent.
func (p *Persister) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(
		func() {
			p.mu.RLock()
			stop := p.stopFn
			p.mu.RUnlock()
			if stop != nil {
				stop()
			}
		},
	)
}

// Record submits a usage record to the writer goroutine. Non-blocking — the
// writer drops on overflow rather than back-pressuring the request path.
func (p *Persister) Record(detail FlatDetail) {
	if p == nil {
		return
	}
	if p.overDBSizeCap() {
		log.Warn("usage: SQLite DB size cap reached, dropping live usage record")
		return
	}
	p.mu.RLock()
	w := p.writer
	p.mu.RUnlock()
	if w == nil {
		return
	}
	w.Submit(detail)
}

// Query is the read-side entry point used by /usage/summary.
func (p *Persister) Query(
	from, to time.Time,
	filters EventFilters,
	granularity string,
	includeGroups bool,
) (QueryResult, error) {
	if p == nil {
		return QueryResult{}, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil {
		return QueryResult{}, errors.New("usage: store not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.Query(ctx, from, to, filters, granularity, includeGroups)
}

// QueryEvents serves /usage/events with SQL-native pagination.
func (p *Persister) QueryEvents(
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	sortDesc bool,
) ([]FlatDetail, int, error) {
	if p == nil {
		return nil, 0, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil {
		return nil, 0, errors.New("usage: store not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.QueryEvents(ctx, from, to, filters, page, pageSize, sortField, sortDesc)
}

// Save flushes the WAL to the main database file. Non-blocking, no
// guarantee writers are quiesced — the WAL checkpoint is "PASSIVE" so it
// completes only as much as it can without forcing other connections to wait.
func (p *Persister) Save() {
	if p == nil {
		return
	}
	_ = p.checkpoint("PASSIVE")
}

func (p *Persister) checkpoint(mode string) error {
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint("+mode+")"); err != nil {
		return fmt.Errorf("wal_checkpoint %s: %w", mode, err)
	}
	return nil
}

// Trim deletes events / hour_bucket / day_bucket rows older than retention.
func (p *Persister) Trim() {
	if p == nil {
		return
	}
	p.mu.RLock()
	s := p.store
	retention := p.retention
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return
	}
	if retention.Days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retention.Days) * 24 * time.Hour).UTC().UnixNano()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, q := range []struct {
		table string
		col   string
	}{
		{"events", "ts_ns"},
		{"hour_bucket", "bucket_ts_ns"},
		{"day_bucket", "bucket_ts_ns"},
	} {
		if _, err := s.db.ExecContext(
			ctx,
			"DELETE FROM "+q.table+" WHERE "+q.col+" < ?", cutoff,
		); err != nil {
			log.Warnf("usage: trim %s: %v", q.table, err)
		}
	}
}

// TrimPreviewResult mirrors the JSON-era preview shape so /usage/trim/preview
// continues to work without UI changes.
type TrimPreviewResult struct {
	FilesCount     int                `json:"files_count"`
	TotalSizeBytes int64              `json:"total_size_bytes"`
	DateRange      *trimDateRange     `json:"date_range,omitempty"`
	Details        []cleanPreviewFile `json:"details"`
}

type trimDateRange struct {
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
}

// cleanPreviewFile keeps the legacy field shape so the management UI can show
// per-day removal counts without changes. SizeBytes is now an estimate
// (events × 256 bytes/row) — there's no per-day file under SQLite.
type cleanPreviewFile struct {
	Date      string `json:"date"`
	SizeBytes int64  `json:"size_bytes"`
}

// TrimPreview reports what Trim would delete without actually deleting it.
func (p *Persister) TrimPreview() TrimPreviewResult {
	if p == nil {
		return TrimPreviewResult{}
	}
	p.mu.RLock()
	s := p.store
	retention := p.retention
	p.mu.RUnlock()
	if s == nil || s.db == nil || retention.Days <= 0 {
		return TrimPreviewResult{}
	}
	cutoff := time.Now().Add(-time.Duration(retention.Days) * 24 * time.Hour).UTC().UnixNano()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(
		ctx,
		"SELECT strftime('%Y-%m-%d', ts_ns / 1000000000, 'unixepoch') AS day, COUNT(*) "+
			"FROM events WHERE ts_ns < ? GROUP BY day ORDER BY day",
		cutoff,
	)
	if err != nil {
		log.Warnf("usage: trim preview: %v", err)
		return TrimPreviewResult{}
	}
	defer rows.Close()

	var details []cleanPreviewFile
	var total int64
	for rows.Next() {
		var day string
		var count int64
		if err := rows.Scan(&day, &count); err != nil {
			log.Warnf("usage: trim preview scan: %v", err)
			continue
		}
		// 256 bytes/row is a back-of-envelope estimate for the dashboard.
		size := count * 256
		details = append(details, cleanPreviewFile{Date: day, SizeBytes: size})
		total += size
	}

	result := TrimPreviewResult{
		FilesCount:     len(details),
		TotalSizeBytes: total,
		Details:        details,
	}
	if len(details) > 0 {
		result.DateRange = &trimDateRange{
			Oldest: details[0].Date,
			Newest: details[len(details)-1].Date,
		}
	}
	return result
}

// Retention returns the current retention configuration.
func (p *Persister) Retention() config.UsageRetention {
	if p == nil {
		return config.UsageRetention{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.retention
}

// SetRetention updates retention. Takes effect on the next Trim invocation.
func (p *Persister) SetRetention(r config.UsageRetention) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.retention = r
	p.mu.Unlock()
}

// Snapshot reconstructs a v1-style StatisticsSnapshot from the SQLite tables
// so /usage/export keeps returning the same response shape the UI knows how
// to consume. Heavy on memory for big datasets — same as the JSON-era path
// it replaces, but bounded by the data the user already has.
func (p *Persister) Snapshot() StatisticsSnapshot {
	return p.SnapshotFiltered(time.Time{}, time.Time{}, EventFilters{})
}

// SnapshotFiltered reconstructs a v1-style StatisticsSnapshot scoped by the
// given time range and event filters. Zero-valued from/to means "no time
// bound"; an empty EventFilters means "no dimension filter". The filtered
// version is what /usage/export uses when the user wants to export only the
// data they're currently viewing in the dashboard.
func (p *Persister) SnapshotFiltered(from, to time.Time, filters EventFilters) StatisticsSnapshot {
	snap := StatisticsSnapshot{
		APIs:           make(map[string]aPISnapshot),
		RequestsByDay:  make(map[string]int64),
		RequestsByHour: make(map[string]int64),
		TokensByDay:    make(map[string]int64),
		TokensByHour:   make(map[string]int64),
	}
	if p == nil {
		return snap
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return snap
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Resolve every active dim filter once. If any active filter has zero
	// matches, the result is empty (don't fall back to full data).
	modelF, err := resolveNamesToIDs(ctx, s.db, "dim_model", filters.Model)
	if err != nil {
		log.Warnf("usage: snapshot resolve dim_model: %v", err)
		return snap
	}
	if modelF.active && !modelF.matchedAny {
		return snap
	}
	apiKeyF, err := resolveNamesToIDs(ctx, s.db, "dim_api_key", filters.APIKey)
	if err != nil {
		log.Warnf("usage: snapshot resolve dim_api_key: %v", err)
		return snap
	}
	if apiKeyF.active && !apiKeyF.matchedAny {
		return snap
	}
	credF, err := resolveCredentialFilter(ctx, s.db, filters.Source)
	if err != nil {
		log.Warnf("usage: snapshot resolve credential filter: %v", err)
		return snap
	}
	if credF.active && !credF.matchedAny {
		return snap
	}

	// Build the WHERE clause that's shared across hour_bucket queries (totals,
	// per-day, per-hour) and the equivalent events-table clause for the
	// detail rebuild. Bucket rows are keyed by their hour-start instant, so
	// the bucket path floors from/to to hour grain — otherwise a from like
	// 10:15 would silently exclude the 10:00 bucket's 10:15-10:59 events.
	// The events path uses the user's exact instants because each row carries
	// a precise ts_ns.
	bucketFrom, bucketTo := floorToHour(from), floorToHour(to)
	bucketWhere, bucketArgs := buildFilterClause(bucketFrom, bucketTo, "bucket_ts_ns", "", modelF, apiKeyF, credF)
	eventsWhere, eventsArgs := buildFilterClause(from, to, "ts_ns", "", modelF, apiKeyF, credF)

	// Totals
	totalsSQL := "SELECT COALESCE(SUM(requests),0), COALESCE(SUM(success),0), COALESCE(SUM(failure),0)," +
		" COALESCE(SUM(total_tokens),0) FROM hour_bucket" + bucketWhere
	if err := s.db.QueryRowContext(ctx, totalsSQL, bucketArgs...).Scan(
		&snap.TotalRequests, &snap.SuccessCount, &snap.FailureCount, &snap.TotalTokens,
	); err != nil {
		log.Warnf("usage: snapshot totals: %v", err)
	}

	// Per-day requests/tokens
	daySQL := "SELECT strftime('%Y-%m-%d', bucket_ts_ns / 1000000000, 'unixepoch') AS day," +
		" SUM(requests), SUM(total_tokens) FROM hour_bucket" + bucketWhere +
		" GROUP BY day ORDER BY day"
	dayRows, err := s.db.QueryContext(ctx, daySQL, bucketArgs...)
	if err == nil {
		for dayRows.Next() {
			var day string
			var req, tokens int64
			if scanErr := dayRows.Scan(&day, &req, &tokens); scanErr == nil {
				snap.RequestsByDay[day] = req
				snap.TokensByDay[day] = tokens
			}
		}
		dayRows.Close()
	}

	// Per-hour-of-day requests/tokens
	hourSQL := "SELECT strftime('%H', bucket_ts_ns / 1000000000, 'unixepoch') AS hr," +
		" SUM(requests), SUM(total_tokens) FROM hour_bucket" + bucketWhere +
		" GROUP BY hr ORDER BY hr"
	hourRows, err := s.db.QueryContext(ctx, hourSQL, bucketArgs...)
	if err == nil {
		for hourRows.Next() {
			var hr string
			var req, tokens int64
			if scanErr := hourRows.Scan(&hr, &req, &tokens); scanErr == nil {
				snap.RequestsByHour[hr] = req
				snap.TokensByHour[hr] = tokens
			}
		}
		hourRows.Close()
	}

	// APIs[apiName].Models[modelName].Details[] — must hit events table.
	eventsSQL := `
		SELECT events.ts_ns,
		       (SELECT name FROM dim_model      WHERE id = events.model_id),
		       (SELECT name FROM dim_source     WHERE id = events.source_id),
		       (SELECT name FROM dim_auth_index WHERE id = events.auth_index_id),
		       (SELECT name FROM dim_api_key    WHERE id = events.api_key_id),
		       events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens, events.total_tokens,
		       events.failed
		  FROM events` + eventsWhere + ` ORDER BY events.ts_ns`
	rows, err := s.db.QueryContext(ctx, eventsSQL, eventsArgs...)
	if err != nil {
		log.Warnf("usage: snapshot events: %v", err)
		return snap
	}
	defer rows.Close()

	for rows.Next() {
		var tsNs int64
		var model, source, authIdx, apiKey sql.NullString
		var t tokenStats
		var failedInt int64
		if err := rows.Scan(
			&tsNs,
			&model, &source, &authIdx, &apiKey,
			&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens, &t.CachedTokens, &t.TotalTokens,
			&failedInt,
		); err != nil {
			log.Warnf("usage: snapshot scan: %v", err)
			continue
		}

		apiName := apiKey.String
		if apiName == "" {
			apiName = "unknown"
		}
		modelName := model.String
		if modelName == "" {
			modelName = "unknown"
		}

		api, ok := snap.APIs[apiName]
		if !ok {
			api = aPISnapshot{Models: make(map[string]modelSnapshot)}
		}
		ms := api.Models[modelName]
		ms.Details = append(
			ms.Details, requestDetail{
				Timestamp: time.Unix(0, tsNs).UTC(),
				Source:    source.String,
				AuthIndex: authIdx.String,
				Tokens:    t,
				Failed:    failedInt != 0,
			},
		)
		ms.TotalRequests++
		ms.TotalTokens += t.TotalTokens
		api.Models[modelName] = ms
		api.TotalRequests++
		api.TotalTokens += t.TotalTokens
		snap.APIs[apiName] = api
	}
	return snap
}

// RecalculateCostsResult holds the outcome of a cost recalculation.
type RecalculateCostsResult struct {
	RecalculatedDays int     `json:"recalculated_days"`
	TotalCost        float64 `json:"total_cost"`
	AlreadyRunning   bool    `json:"already_running,omitempty"`
}

// RecalculateCosts re-prices every event using the current PriceFunc, then
// rebuilds hour_bucket and day_bucket from the updated events table. Heavy
// SQL — this is intended for occasional admin-triggered recalculation, not
// per-request use.
func (p *Persister) RecalculateCosts() (RecalculateCostsResult, error) {
	if p == nil {
		return RecalculateCostsResult{}, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	priceFn := p.pricesFn
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return RecalculateCostsResult{}, errors.New("usage: store not initialized")
	}
	if priceFn == nil {
		return RecalculateCostsResult{}, errors.New("usage: no pricing function configured")
	}
	if !p.costRecalcRunning.CompareAndSwap(false, true) {
		return RecalculateCostsResult{AlreadyRunning: true}, nil
	}
	defer p.costRecalcRunning.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1) Update events.cost_micro per model.
	models, err := s.fetchAllModels(ctx)
	if err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("fetch models: %w", err)
	}
	for _, m := range models {
		prompt, completion, cache, ok := priceFn(m.name)
		if !ok {
			// No price → zero out so stale costs don't linger.
			if _, err := s.db.ExecContext(
				ctx,
				"UPDATE events SET cost_micro = 0 WHERE model_id = ?", m.id,
			); err != nil {
				return RecalculateCostsResult{}, fmt.Errorf("zero costs for %s: %w", m.name, err)
			}
			continue
		}
		if _, err := s.db.ExecContext(
			ctx, `
			UPDATE events
			   SET cost_micro = CAST(input_tokens * ? + output_tokens * ? + cached_tokens * ? AS INTEGER)
			 WHERE model_id = ?
		`, prompt, completion, cache, m.id,
		); err != nil {
			return RecalculateCostsResult{}, fmt.Errorf("update costs for %s: %w", m.name, err)
		}
	}

	// 2) Wipe and rebuild hour_bucket from events.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("begin rebuild tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, "DELETE FROM hour_bucket"); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("clear hour_bucket: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx, fmt.Sprintf(
			`
		INSERT INTO hour_bucket (
		    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
		    requests, success, failure,
		    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		    cost_micro
		)
		SELECT
		    (ts_ns / %d) * %d AS bucket_ts_ns,
		    model_id, credential_id, api_key_id, provider_id,
		    COUNT(*) AS requests,
		    SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END) AS success,
		    SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) AS failure,
		    SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens), SUM(total_tokens),
		    SUM(cost_micro)
		  FROM events
		 GROUP BY bucket_ts_ns, model_id, credential_id, api_key_id, provider_id
	`, nsPerHour, nsPerHour,
		),
	); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("rebuild hour_bucket: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM day_bucket"); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("clear day_bucket: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("commit rebuild: %w", err)
	}
	committed = true

	// 3) Roll up day_bucket from the freshly-rebuilt hour_bucket.
	earliestDayNs, err := s.earliestHourBucketDay(ctx)
	if err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("query earliest day after recalc: %w", err)
	}
	latestDayNs, hasData, err := s.latestHourBucketDay(ctx)
	if err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("query latest day after recalc: %w", err)
	}
	if hasData {
		if err := s.rollupDayRange(ctx, earliestDayNs, latestDayNs); err != nil {
			return RecalculateCostsResult{}, fmt.Errorf("rollup day_bucket after recalc: %w", err)
		}
		if err := s.setDayRollupWatermark(ctx, latestDayNs); err != nil {
			return RecalculateCostsResult{}, fmt.Errorf("persist day_bucket watermark after recalc: %w", err)
		}
	}

	// 4) Total cost for the response.
	var totalMicro int64
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT COALESCE(SUM(cost_micro), 0) FROM hour_bucket",
	).Scan(&totalMicro); err != nil {
		log.Warnf("usage: total cost after recalc: %v", err)
	}

	// Days touched ≈ distinct day buckets after rollup.
	var days int
	_ = s.db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT bucket_ts_ns) FROM day_bucket").Scan(&days)

	return RecalculateCostsResult{
		RecalculatedDays: days,
		TotalCost:        float64(totalMicro) / costMicroPerUSD,
	}, nil
}

type modelRow struct {
	id   int64
	name string
}

func (s *Store) fetchAllModels(ctx context.Context) ([]modelRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM dim_model")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelRow
	for rows.Next() {
		var m modelRow
		if err := rows.Scan(&m.id, &m.name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Import reads a payload (any of the supported formats), inserts the events,
// and returns counts. Used by /usage/import.
func (p *Persister) Import(data []byte) (ImportResult, error) {
	if p == nil {
		return ImportResult{}, errors.New("usage: nil persister")
	}
	if p.overDBSizeCap() {
		return ImportResult{}, errors.New("usage: SQLite DB size cap reached, import refused")
	}
	p.mu.RLock()
	s := p.store
	priceFn := p.pricesFn
	p.mu.RUnlock()
	if s == nil {
		return ImportResult{}, errors.New("usage: store not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := ImportBytes(ctx, s, priceFn, data)
	if err != nil {
		return ImportResult{}, err
	}
	if result.HasDayRange {
		if err := s.rollupImportedDays(ctx, result.EarliestDayNs, result.LatestDayNs); err != nil {
			return ImportResult{}, err
		}
	}
	if err := s.rollupPendingDays(ctx); err != nil {
		return ImportResult{}, err
	}
	if err := p.checkpoint("TRUNCATE"); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

func (p *Persister) priceFn() PriceFunc {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pricesFn
}

// ResolveDataDir picks the usage data directory:
// configured > WRITABLE_PATH/usage-data > config dir/usage-data.
func ResolveDataDir(configured, configFilePath string) string {
	if configured != "" {
		return configured
	}
	if wp := util.WritablePath(); wp != "" {
		return filepath.Join(wp, dataDirName)
	}
	if configFilePath != "" {
		return filepath.Join(filepath.Dir(configFilePath), dataDirName)
	}
	return ""
}

// atomicWrite writes data to path via a temp file + rename. Files end up at
// 0600, the parent directory at 0700. Used by tests + (formerly) the JSON
// persister; left in place so persister_test.go and any future caller works.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to write %s: %w", tmpPath, err)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to chmod %s: %w", tmpPath, err)
	}
	_ = tmpFile.Close()
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// MigrateDataDirIfNeeded moves an existing usage-data directory from any
// legacy location to the resolved new location when WRITABLE_PATH or install
// layout changed between restarts. Cross-filesystem moves fall back to copy +
// remove. Skips when paths match or when the new location already has
// summary.json / events.db.
func MigrateDataDirIfNeeded(configFilePath, newBaseDir string) error {
	if newBaseDir == "" {
		return nil
	}
	for _, marker := range []string{"events.db", "summary.json"} {
		if _, err := os.Stat(filepath.Join(newBaseDir, marker)); err == nil {
			return nil
		}
	}

	candidates := make([]string, 0, 2)
	if configFilePath != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(configFilePath), dataDirName))
	}
	if wp := util.WritablePath(); wp != "" {
		candidates = append(candidates, filepath.Join(wp, dataDirName))
	}

	for _, oldBaseDir := range candidates {
		if filepath.Clean(oldBaseDir) == filepath.Clean(newBaseDir) {
			continue
		}
		oldInfo, err := os.Stat(oldBaseDir)
		if err != nil || !oldInfo.IsDir() {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(newBaseDir), 0o700); err != nil {
			return fmt.Errorf("create parent dir for %s: %w", newBaseDir, err)
		}
		if err := os.Rename(oldBaseDir, newBaseDir); err != nil {
			if !isCrossDeviceError(err) {
				return fmt.Errorf("move %s -> %s: %w", oldBaseDir, newBaseDir, err)
			}
			if copyErr := copyDirRecursive(oldBaseDir, newBaseDir); copyErr != nil {
				return fmt.Errorf("cross-device copy %s -> %s: %w", oldBaseDir, newBaseDir, copyErr)
			}
			if rmErr := os.RemoveAll(oldBaseDir); rmErr != nil {
				log.Warnf(
					"usage: migrated %s -> %s via copy; failed to remove old dir: %v",
					oldBaseDir, newBaseDir, rmErr,
				)
			}
		}
		log.Infof("usage: migrated data directory %s -> %s", oldBaseDir, newBaseDir)
		return nil
	}
	return nil
}

func isCrossDeviceError(err error) bool {
	if err == nil {
		return false
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return errors.Is(err, syscall.EXDEV)
}

func copyDirRecursive(src, dst string) error {
	return filepath.WalkDir(
		src, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			return copyRegularFile(path, target)
		},
	)
}

func copyRegularFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
