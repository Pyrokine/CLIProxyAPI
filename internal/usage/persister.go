package usage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	dataDirName              = "usage-data"
	migrationStagingSuffix   = ".migration-staging"
	migrationReadyMarkerFile = ".migration-ready"

	// -1 means unlimited retention (no automatic cleanup).
	// Configure via config.yaml usage-retention.days to override.
	defaultRetentionDays = -1
)

// PriceFunc returns the pricing for a model: prices are in USD per 1M tokens.
type PriceFunc func(model string) (prompt, completion, cache float64, found bool)

var renameUsageDataDir = os.Rename

// Persister is the SQLite-backed coordinator for usage data. It owns the
// Store + Writer + rollup goroutine, exposes a stable surface to the rest of
// the codebase, and hides the schema / WAL / dim_table details. Public method
// signatures match the JSON-era persister 1:1 so handler / SDK / test code
// keeps compiling unchanged.
type Persister struct {
	mu           sync.RWMutex
	pricingMu    sync.RWMutex
	costRecalcMu sync.RWMutex

	store           *Store
	writer          *Writer
	stopOnce        sync.Once
	stopFn          func()
	stopDone        chan struct{}
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	maintenance     chan struct{}

	pricesFn      PriceFunc
	priceProvider PriceProvider
	baseDir       string
	retention     config.UsageRetention

	costRecalcRunning atomic.Bool
	costRecalcPending bool
	costRecalcStatus  CostRecalculationStatus
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
	maintenance := make(chan struct{}, 1)
	maintenance <- struct{}{}
	return &Persister{
		baseDir:     baseDir,
		retention:   retention,
		stopDone:    make(chan struct{}),
		maintenance: maintenance,
	}
}

// SetPriceFunc registers (or replaces) a static pricing function.
func (p *Persister) SetPriceFunc(fn PriceFunc) {
	if p == nil {
		return
	}
	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()

	p.mu.Lock()
	p.pricesFn = fn
	p.priceProvider = nil
	w := p.writer
	p.mu.Unlock()
	if w != nil {
		w.SetPriceFunc(fn)
	}
}

// SetPriceProvider registers a provider that supplies one immutable price
// snapshot per persistence operation.
func (p *Persister) SetPriceProvider(provider PriceProvider) {
	if p == nil {
		return
	}
	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()

	p.mu.Lock()
	p.pricesFn = nil
	p.priceProvider = provider
	w := p.writer
	p.mu.Unlock()
	if w != nil {
		w.SetPriceProvider(provider)
	}
}

// HasPricing reports whether a price lookup function has been configured.
func (p *Persister) HasPricing() bool {
	return p != nil && p.priceFn() != nil
}

// BaseDir returns the usage data directory.
func (p *Persister) BaseDir() string {
	if p == nil {
		return ""
	}
	return p.baseDir
}

// IsReady reports whether Start successfully opened the SQLite store.
// Service callers branch on this before registering their logger plugin; when
// startup fails, they keep the instance persister nil so records continue into
// that service's in-memory RequestStatistics.
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
// Failure to open the store is logged with a clear "degraded mode" warning.
// The caller checks IsReady() before binding this persister to its logger plugin
// so the service can retain its in-memory RequestStatistics path.
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

	p.pricingMu.RLock()
	priceProvider, configuredPriceFn := p.priceSource()
	priceFn := configuredPriceFn
	if priceProvider != nil {
		priceFn = priceProvider.Snapshot()
	}
	if err := MigrateLegacyToSQLite(ctx, store, priceFn, p.baseDir); err != nil {
		log.Errorf("usage: legacy migration failed: %v", err)
		// Migration failure is recoverable — the user can fix the file and
		// restart. Continue serving with whatever we already have on disk.
	}

	writer := newWriterWithPriceProviderAndGate(
		store,
		priceProvider,
		configuredPriceFn,
		&p.pricingMu,
	)
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	writer.Start(lifecycleCtx)

	rollupStop := store.StartRollup(lifecycleCtx)

	p.mu.Lock()
	p.store = store
	p.writer = writer
	p.lifecycleCtx = lifecycleCtx
	p.lifecycleCancel = lifecycleCancel
	p.mu.Unlock()
	p.pricingMu.RUnlock()

	trimStop := p.startRetentionTrim(lifecycleCtx)

	p.mu.Lock()
	p.stopFn = func() {
		lifecycleCancel()
		writer.Stop()
		trimStop()
		rollupStop()
		_ = store.Close()
		p.mu.Lock()
		p.store = nil
		p.writer = nil
		p.mu.Unlock()
	}
	p.mu.Unlock()

	log.Infof("usage: SQLite store opened at %s", dbPath)
}

func (p *Persister) startRetentionTrim(ctx context.Context) func() {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		p.Trim()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.Trim()
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

// Stop drains the writer, halts the rollup loop, and closes the store. Idempotent.
func (p *Persister) Stop() {
	_ = p.StopContext(context.Background())
}

// StopContext begins shutdown once and waits for completion or ctx cancellation.
func (p *Persister) StopContext(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.stopOnce.Do(func() {
		p.mu.RLock()
		stop := p.stopFn
		cancel := p.lifecycleCancel
		p.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
		go func() {
			if stop != nil {
				stop()
			}
			close(p.stopDone)
		}()
	})
	select {
	case <-p.stopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Persister) maintenanceContext(parent context.Context, timeout time.Duration) (
	context.Context,
	context.CancelFunc,
) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	p.mu.RLock()
	lifecycleCtx := p.lifecycleCtx
	p.mu.RUnlock()
	if lifecycleCtx == nil {
		return ctx, cancel
	}
	stopLifecycleWatch := context.AfterFunc(lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycleWatch()
		cancel()
	}
}

func (p *Persister) acquireMaintenance(ctx context.Context) (func(), error) {
	if p == nil {
		return nil, errors.New("usage: maintenance lock unavailable")
	}
	p.mu.Lock()
	if p.maintenance == nil {
		p.maintenance = make(chan struct{}, 1)
		p.maintenance <- struct{}{}
	}
	maintenance := p.maintenance
	p.mu.Unlock()
	select {
	case <-maintenance:
		return func() { maintenance <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

// QueryBackupEventsContext reads a complete, transactionally consistent event
// set while inheriting request cancellation and the export timeout.
func (p *Persister) QueryBackupEventsContext(
	parent context.Context,
	from, to time.Time,
	filters EventFilters,
) ([]FlatDetail, error) {
	if p == nil {
		return nil, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil {
		return nil, errors.New("usage: store not initialized")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	return s.QueryBackupEvents(ctx, from, to, filters)
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

// Trim deletes exact events older than retention and rebuilds every aggregate
// in the same transaction so partial-hour cutoffs remain consistent.
func (p *Persister) Trim() {
	if p == nil {
		return
	}
	p.mu.RLock()
	retention := p.retention
	p.mu.RUnlock()
	if retention.Days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retention.Days) * 24 * time.Hour).UTC()
	if err := p.trimBefore(cutoff); err != nil {
		log.Warnf("usage: trim: %v", err)
	}
}

func (p *Persister) trimBefore(cutoff time.Time) error {
	if p == nil {
		return errors.New("usage: nil persister")
	}
	cutoffNs, err := unixNanoUTC(cutoff)
	if err != nil {
		return fmt.Errorf("invalid trim cutoff: %w", err)
	}

	ctx, cancel := p.maintenanceContext(context.Background(), 60*time.Second)
	defer cancel()
	unlockMaintenance, errLock := p.acquireMaintenance(ctx)
	if errLock != nil {
		return fmt.Errorf("wait for usage maintenance: %w", errLock)
	}
	defer unlockMaintenance()

	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()

	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return errors.New("usage: store not initialized")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin trim tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, "DELETE FROM events WHERE ts_ns < ?", cutoffNs); err != nil {
		return fmt.Errorf("delete expired events: %w", err)
	}
	if err := rebuildUsageBucketsTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trim tx: %w", err)
	}
	committed = true
	return nil
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
	defer func() { _ = rows.Close() }()

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

func emptyStatisticsSnapshot() StatisticsSnapshot {
	return StatisticsSnapshot{
		APIs:           make(map[string]aPISnapshot),
		RequestsByDay:  make(map[string]int64),
		RequestsByHour: make(map[string]int64),
		TokensByDay:    make(map[string]int64),
		TokensByHour:   make(map[string]int64),
	}
}

// Snapshot reconstructs a v1-style StatisticsSnapshot from the SQLite tables
// so /usage/export keeps returning the same response shape the UI knows how
// to consume. Heavy on memory for big datasets — same as the JSON-era path
// it replaces, but bounded by the data the user already has.
func (p *Persister) Snapshot() StatisticsSnapshot {
	snap, err := p.QuerySnapshot()
	if err != nil {
		log.Warnf("usage: snapshot: %v", err)
		return emptyStatisticsSnapshot()
	}
	return snap
}

// QuerySnapshot reconstructs the full v1-style snapshot and reports storage
// errors instead of returning an incomplete snapshot.
func (p *Persister) QuerySnapshot() (StatisticsSnapshot, error) {
	return p.QuerySnapshotFiltered(time.Time{}, time.Time{}, EventFilters{})
}

// SnapshotFiltered reconstructs a v1-style StatisticsSnapshot scoped by the
// given time range and event filters. Storage errors are logged and represented
// by an empty snapshot for compatibility with callers that predate SQLite.
func (p *Persister) SnapshotFiltered(from, to time.Time, filters EventFilters) StatisticsSnapshot {
	snap, err := p.QuerySnapshotFiltered(from, to, filters)
	if err != nil {
		log.Warnf("usage: filtered snapshot: %v", err)
		return emptyStatisticsSnapshot()
	}
	return snap
}

// QuerySnapshotFiltered reconstructs a filtered v1-style snapshot. Zero-valued
// from/to means "no time bound"; an empty EventFilters means "no dimension
// filter". Every storage error aborts the query so callers never receive
// partial usage data.
func (p *Persister) QuerySnapshotFiltered(
	from, to time.Time,
	filters EventFilters,
) (StatisticsSnapshot, error) {
	snap := emptyStatisticsSnapshot()
	if p == nil {
		return snap, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return snap, errors.New("usage: store not initialized")
	}
	if !from.IsZero() {
		if _, err := unixNanoUTC(from); err != nil {
			return StatisticsSnapshot{}, fmt.Errorf("invalid snapshot start: %w", err)
		}
	}
	if !to.IsZero() {
		if _, err := unixNanoUTC(to); err != nil {
			return StatisticsSnapshot{}, fmt.Errorf("invalid snapshot end: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("begin snapshot tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Resolve every active dim filter once. If any active filter has zero
	// matches, the result is empty (don't fall back to full data).
	modelF, err := resolveNamesToIDs(ctx, tx, "dim_model", filters.Model)
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("resolve snapshot model filter: %w", err)
	}
	if modelF.active && !modelF.matchedAny {
		return snap, nil
	}
	apiKeyF, err := resolveFilterNamesToIDs(ctx, tx, "dim_api_key", filters.APIKey, filters.APIKeys)
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("resolve snapshot API key filter: %w", err)
	}
	if apiKeyF.active && !apiKeyF.matchedAny {
		return snap, nil
	}
	credF, err := resolveCredentialFilter(ctx, tx, filters.Source)
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("resolve snapshot credential filter: %w", err)
	}
	if credF.active && !credF.matchedAny {
		return snap, nil
	}

	// Snapshot totals, time maps, API/model aggregates, and details all come
	// from the same exact event set. Bucket tables cannot represent partial-hour
	// bounds without including events outside the requested interval.
	eventsWhere, eventsArgs := buildFilterClause(from, to, "ts_ns", "", modelF, apiKeyF, credF)

	// APIs[apiName].Models[modelName].Details[] must hit the events table.
	eventsSQL := `
		SELECT events.ts_ns,
		       (SELECT name FROM dim_model      WHERE id = events.model_id),
		       (SELECT name FROM dim_source     WHERE id = events.source_id),
		       (SELECT name FROM dim_auth_index WHERE id = events.auth_index_id),
		       (SELECT name FROM dim_api_key    WHERE id = events.api_key_id),
		       events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens, events.total_tokens,
		       events.failed, events.metadata
		  FROM events` + eventsWhere + ` ORDER BY events.ts_ns`
	rows, err := tx.QueryContext(ctx, eventsSQL, eventsArgs...)
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("query snapshot events: %w", err)
	}
	addAggregate := func(current, delta int64, name string) (int64, error) {
		value, ok := coreusage.AddNonNegativeInt64(current, delta)
		if !ok {
			return 0, fmt.Errorf("usage: snapshot %s aggregate overflows int64", name)
		}
		return value, nil
	}

	for rows.Next() {
		var tsNs int64
		var model, source, authIdx, apiKey sql.NullString
		var metadata sql.NullString
		var t tokenStats
		var failedInt int64
		if err := rows.Scan(
			&tsNs,
			&model, &source, &authIdx, &apiKey,
			&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens, &t.CachedTokens, &t.TotalTokens,
			&failedInt,
			&metadata,
		); err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, fmt.Errorf("scan snapshot event: %w", err)
		}

		apiName := apiKey.String
		if apiName == "" {
			apiName = "unknown"
		}
		modelName := model.String
		if modelName == "" {
			modelName = "unknown"
		}

		t, err = validatedTokenStats(t)
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, fmt.Errorf("validate snapshot event tokens at ts_ns=%d: %w", tsNs, err)
		}
		api, ok := snap.APIs[apiName]
		if !ok {
			api = aPISnapshot{Models: make(map[string]modelSnapshot)}
		}
		ms := api.Models[modelName]
		timestamp := time.Unix(0, tsNs).UTC()
		detail := requestDetail{
			Timestamp: timestamp,
			Source:    source.String,
			AuthIndex: authIdx.String,
			Tokens:    t,
			Failed:    failedInt != 0,
		}
		flat := FlatDetail{}
		if err := decodeEventMetadata(&flat, metadata); err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, fmt.Errorf("decode snapshot event metadata at ts_ns=%d: %w", tsNs, err)
		}
		detail.LatencyMs = flat.LatencyMs
		detail.RequestID = flat.RequestID
		detail.TimeToFirstByteMs = flat.TimeToFirstByteMs
		detail.TotalDurationMs = flat.TotalDurationMs
		detail.Completed = flat.Completed
		detail.ReasoningEffort = flat.ReasoningEffort
		detail.ServiceTier = flat.ServiceTier
		detail.RequestServiceTier = flat.RequestServiceTier
		detail.ResponseServiceTier = flat.ResponseServiceTier
		detail.ExecutorType = flat.ExecutorType
		detail.Thinking = cloneThinking(flat.Thinking)
		detail.ResponseHeaders = flat.ResponseHeaders

		dayKey := timestamp.Format("2006-01-02")
		hourKey := formatHour(timestamp.Hour())
		totalRequests, err := addAggregate(snap.TotalRequests, 1, "request count")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		successCount := snap.SuccessCount
		failureCount := snap.FailureCount
		if detail.Failed {
			failureCount, err = addAggregate(failureCount, 1, "failure count")
		} else {
			successCount, err = addAggregate(successCount, 1, "success count")
		}
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		totalTokens, err := addAggregate(snap.TotalTokens, t.TotalTokens, "token")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		requestsByDay, err := addAggregate(snap.RequestsByDay[dayKey], 1, "daily request")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		requestsByHour, err := addAggregate(snap.RequestsByHour[hourKey], 1, "hourly request")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		tokensByDay, err := addAggregate(snap.TokensByDay[dayKey], t.TotalTokens, "daily token")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		tokensByHour, err := addAggregate(snap.TokensByHour[hourKey], t.TotalTokens, "hourly token")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		modelRequests, err := addAggregate(ms.TotalRequests, 1, "model request")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		apiRequests, err := addAggregate(api.TotalRequests, 1, "API request")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		modelTotal, err := addAggregate(ms.TotalTokens, t.TotalTokens, "model token")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}
		apiTotal, err := addAggregate(api.TotalTokens, t.TotalTokens, "API token")
		if err != nil {
			_ = rows.Close()
			return StatisticsSnapshot{}, err
		}

		snap.TotalRequests = totalRequests
		snap.SuccessCount = successCount
		snap.FailureCount = failureCount
		snap.TotalTokens = totalTokens
		snap.RequestsByDay[dayKey] = requestsByDay
		snap.RequestsByHour[hourKey] = requestsByHour
		snap.TokensByDay[dayKey] = tokensByDay
		snap.TokensByHour[hourKey] = tokensByHour
		ms.Details = append(ms.Details, detail)
		ms.TotalRequests = modelRequests
		ms.TotalTokens = modelTotal
		api.TotalRequests = apiRequests
		api.TotalTokens = apiTotal
		api.Models[modelName] = ms
		snap.APIs[apiName] = api
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return StatisticsSnapshot{}, fmt.Errorf("iterate snapshot events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("close snapshot events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("commit snapshot tx: %w", err)
	}
	committed = true
	return snap, nil
}

// RecalculateCostsResult holds the outcome of a cost recalculation.
type RecalculateCostsResult struct {
	RecalculatedDays int     `json:"recalculated_days"`
	TotalCost        float64 `json:"total_cost"`
	AlreadyRunning   bool    `json:"already_running,omitempty"`
}

// CostRecalculationStatus reports the latest asynchronous recalculation job.
type CostRecalculationStatus struct {
	Status           string  `json:"status"`
	Running          bool    `json:"running"`
	RecalculatedDays int     `json:"recalculated_days,omitempty"`
	TotalCost        float64 `json:"total_cost,omitempty"`
	Error            string  `json:"error,omitempty"`
}

// RecalculateCosts re-prices every event using the current PriceFunc, then
// rebuilds hour_bucket and day_bucket from the updated events table. Heavy
// SQL — this is intended for occasional admin-triggered recalculation, not
// per-request use.
func (p *Persister) RecalculateCosts() (RecalculateCostsResult, error) {
	return p.RecalculateCostsContext(context.Background())
}

// RecalculateCostsContext recalculates historical costs and allows lock waits,
// SQL work, and shutdown cancellation to share one deadline.
func (p *Persister) RecalculateCostsContext(parent context.Context) (RecalculateCostsResult, error) {
	if p == nil {
		return RecalculateCostsResult{}, errors.New("usage: nil persister")
	}
	if !p.beginCostRecalculation(false) {
		return RecalculateCostsResult{AlreadyRunning: true}, nil
	}
	return p.runCostRecalculations(parent)
}

// StartRecalculateCosts starts one lifecycle-bound recalculation job. Calls
// made while a job is running queue one follow-up pass with the latest prices.
func (p *Persister) StartRecalculateCosts() bool {
	if !p.beginCostRecalculation(true) {
		return false
	}
	p.mu.RLock()
	parent := p.lifecycleCtx
	p.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		_, _ = p.runCostRecalculations(parent)
	}()
	return true
}

// RecalculateCostsStatus returns a copy of the latest asynchronous job state.
func (p *Persister) RecalculateCostsStatus() CostRecalculationStatus {
	if p == nil {
		return CostRecalculationStatus{Status: "idle"}
	}
	p.costRecalcMu.RLock()
	defer p.costRecalcMu.RUnlock()
	status := p.costRecalcStatus
	if status.Status == "" {
		status.Status = "idle"
	}
	return status
}

func (p *Persister) beginCostRecalculation(queueIfRunning bool) bool {
	if p == nil {
		return false
	}
	p.costRecalcMu.Lock()
	defer p.costRecalcMu.Unlock()
	if p.costRecalcRunning.Load() {
		if queueIfRunning {
			p.costRecalcPending = true
		}
		return false
	}
	p.costRecalcRunning.Store(true)
	p.costRecalcPending = false
	p.costRecalcStatus = CostRecalculationStatus{Status: "running", Running: true}
	return true
}

func (p *Persister) runCostRecalculations(parent context.Context) (RecalculateCostsResult, error) {
	for {
		result, err := p.recalculateCostsContext(parent)
		if !p.finishCostRecalculation(result, err) {
			return result, err
		}
	}
}

// finishCostRecalculation publishes the result unless a newer price update
// arrived while the pass was running. In that case it keeps the job visible as
// running and asks the caller to execute one coalesced follow-up pass.
func (p *Persister) finishCostRecalculation(result RecalculateCostsResult, err error) bool {
	p.costRecalcMu.Lock()
	if p.costRecalcPending {
		p.costRecalcPending = false
		p.costRecalcStatus = CostRecalculationStatus{Status: "running", Running: true}
		p.costRecalcMu.Unlock()
		log.Info("usage: historical cost recalculation queued again for updated prices")
		return true
	}
	status := CostRecalculationStatus{
		Status:           "ok",
		RecalculatedDays: result.RecalculatedDays,
		TotalCost:        result.TotalCost,
	}
	if err != nil {
		status = CostRecalculationStatus{Status: "error", Error: err.Error()}
	}
	p.costRecalcStatus = status
	p.costRecalcRunning.Store(false)
	p.costRecalcMu.Unlock()
	if err != nil {
		log.Errorf("usage: historical cost recalculation failed: %v", err)
		return false
	}
	log.Infof(
		"usage: historical cost recalculation complete (days=%d, total_cost=%.6f)",
		result.RecalculatedDays,
		result.TotalCost,
	)
	return false
}

func (p *Persister) recalculateCostsContext(parent context.Context) (RecalculateCostsResult, error) {
	ctx, cancel := p.maintenanceContext(parent, 5*time.Minute)
	defer cancel()
	unlockMaintenance, errLock := p.acquireMaintenance(ctx)
	if errLock != nil {
		return RecalculateCostsResult{}, fmt.Errorf("wait for usage maintenance: %w", errLock)
	}
	defer unlockMaintenance()

	p.pricingMu.RLock()
	priceFn := p.priceFn()
	p.pricingMu.RUnlock()

	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return RecalculateCostsResult{}, errors.New("usage: store not initialized")
	}
	if priceFn == nil {
		return RecalculateCostsResult{}, errors.New("usage: no pricing function configured")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("begin recalculation tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Enumerate models inside the same transaction as repricing so no model can
	// appear between the snapshot and the bucket rebuild.
	models, err := fetchAllModels(ctx, tx)
	if err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("fetch models: %w", err)
	}
	for _, m := range models {
		prompt, completion, cache, ok := priceFn(m.name)
		if !ok {
			// No price → zero out so stale costs don't linger.
			if _, err := tx.ExecContext(
				ctx,
				"UPDATE events SET cost_micro = 0 WHERE model_id = ?", m.id,
			); err != nil {
				return RecalculateCostsResult{}, fmt.Errorf("zero costs for %s: %w", m.name, err)
			}
			continue
		}
		if err := repriceModelEvents(
			ctx,
			tx,
			m,
			prompt,
			completion,
			cache,
		); err != nil {
			return RecalculateCostsResult{}, err
		}
	}

	if err := rebuildUsageBucketsTx(ctx, tx); err != nil {
		return RecalculateCostsResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("commit rebuild: %w", err)
	}
	committed = true

	var totalMicro float64
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT TOTAL(cost_micro) FROM hour_bucket",
	).Scan(&totalMicro); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("total cost after recalc: %w", err)
	}

	var days int
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT COUNT(DISTINCT bucket_ts_ns) FROM day_bucket",
	).Scan(&days); err != nil {
		return RecalculateCostsResult{}, fmt.Errorf("count recalculated days: %w", err)
	}

	return RecalculateCostsResult{
		RecalculatedDays: days,
		TotalCost:        totalMicro / costMicroPerUSD,
	}, nil
}

func rebuildUsageBucketsTx(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("usage: rebuild buckets on nil transaction")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hour_bucket"); err != nil {
		return fmt.Errorf("clear hour_bucket: %w", err)
	}
	hourExpr := sqliteFloorBucketExpr("ts_ns", nsPerHour)
	if _, err := tx.ExecContext(
		ctx,
		fmt.Sprintf(`
		INSERT INTO hour_bucket (
		    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
		    requests, success, failure,
		    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		    cost_micro, latency_ms_total, latency_samples
		)
		SELECT
		    %s AS bucket_ts_ns,
		    model_id, credential_id, api_key_id, provider_id,
		    COUNT(*) AS requests,
		    SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END) AS success,
		    SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) AS failure,
		    SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens), SUM(total_tokens),
		    SUM(cost_micro), 0, 0
		  FROM events
		 GROUP BY %s, model_id, credential_id, api_key_id, provider_id
	`, hourExpr, hourExpr),
	); err != nil {
		return fmt.Errorf("rebuild hour_bucket: %w", err)
	}
	if err := backfillLatencyBuckets(ctx, tx, false); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM day_bucket"); err != nil {
		return fmt.Errorf("clear day_bucket: %w", err)
	}
	dayExpr := sqliteFloorBucketExpr("bucket_ts_ns", nsPerDay)
	if _, err := tx.ExecContext(
		ctx,
		fmt.Sprintf(`
		INSERT INTO day_bucket (
		    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
		    requests, success, failure,
		    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		    cost_micro, latency_ms_total, latency_samples
		)
		SELECT
		    %s AS bucket_ts_ns,
		    model_id, credential_id, api_key_id, provider_id,
		    SUM(requests), SUM(success), SUM(failure),
		    SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens), SUM(total_tokens),
		    SUM(cost_micro), SUM(latency_ms_total), SUM(latency_samples)
		  FROM hour_bucket
		 GROUP BY %s, model_id, credential_id, api_key_id, provider_id
		`, dayExpr, dayExpr),
	); err != nil {
		return fmt.Errorf("rebuild day_bucket: %w", err)
	}

	var latestDayNs sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MAX(bucket_ts_ns) FROM day_bucket").Scan(&latestDayNs); err != nil {
		return fmt.Errorf("query latest day after bucket rebuild: %w", err)
	}
	if latestDayNs.Valid {
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			dayRollupWatermarkKey,
			fmt.Sprintf("%d", latestDayNs.Int64),
		); err != nil {
			return fmt.Errorf("persist day_bucket watermark after rebuild: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, "DELETE FROM meta WHERE key = ?", dayRollupWatermarkKey); err != nil {
		return fmt.Errorf("clear day_bucket watermark after rebuild: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM dirty_day"); err != nil {
		return fmt.Errorf("clear dirty days after bucket rebuild: %w", err)
	}
	return nil
}

type modelRow struct {
	id   int64
	name string
}

const costMicroSQLExpression = `CAST(input_tokens AS REAL) * ?1 +
	CAST(output_tokens AS REAL) * ?2 +
	CAST(cached_tokens AS REAL) * ?3`

func repriceModelEvents(
	ctx context.Context,
	tx *sql.Tx,
	model modelRow,
	promptPrice float64,
	completionPrice float64,
	cachePrice float64,
) error {
	priceFn := func(string) (float64, float64, float64, bool) {
		return promptPrice, completionPrice, cachePrice, true
	}
	if _, err := computeCostMicro(tokenStats{}, priceFn, model.name); err != nil {
		return err
	}

	var maxCost sql.NullFloat64
	if err := tx.QueryRowContext(
		ctx,
		"SELECT MAX("+costMicroSQLExpression+") FROM events WHERE model_id = ?",
		promptPrice,
		completionPrice,
		cachePrice,
		model.id,
	).Scan(&maxCost); err != nil {
		return fmt.Errorf("validate costs for %s: %w", model.name, err)
	}
	if maxCost.Valid && (math.IsNaN(maxCost.Float64) || math.IsInf(maxCost.Float64, 0) ||
		maxCost.Float64 < 0 || maxCost.Float64 >= math.Exp2(63)) {
		return fmt.Errorf("usage: cost_micro out of int64 range for model %q", model.name)
	}

	if _, err := tx.ExecContext(
		ctx,
		"UPDATE events SET cost_micro = CAST("+costMicroSQLExpression+
			" AS INTEGER) WHERE model_id = ? AND cost_micro <> CAST("+costMicroSQLExpression+" AS INTEGER)",
		promptPrice,
		completionPrice,
		cachePrice,
		model.id,
	); err != nil {
		return fmt.Errorf("update costs for %s: %w", model.name, err)
	}
	return nil
}

type dbQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func fetchAllModels(ctx context.Context, queryer dbQueryer) ([]modelRow, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT id, name FROM dim_model")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

// Import reads a payload (any of the supported legacy formats), inserts the
// events, and returns counts. Encrypted backups use ImportContextWithSecret.
func (p *Persister) Import(data []byte) (ImportResult, error) {
	return p.ImportContext(context.Background(), data)
}

// ImportContext imports non-encrypted usage data under the cancellable
// maintenance gate.
func (p *Persister) ImportContext(parent context.Context, data []byte) (ImportResult, error) {
	return p.ImportContextWithSecret(parent, data, "")
}

// ImportContextWithSecret imports encrypted backups and every legacy format
// under the same cancellable maintenance gate.
func (p *Persister) ImportContextWithSecret(
	parent context.Context,
	data []byte,
	secret string,
) (ImportResult, error) {
	if p == nil {
		return ImportResult{}, errors.New("usage: nil persister")
	}
	if p.overDBSizeCap() {
		return ImportResult{}, errors.New("usage: SQLite DB size cap reached, import refused")
	}

	ctx, cancel := p.maintenanceContext(parent, 5*time.Minute)
	defer cancel()
	unlockMaintenance, errLock := p.acquireMaintenance(ctx)
	if errLock != nil {
		return ImportResult{}, fmt.Errorf("wait for usage maintenance: %w", errLock)
	}
	defer unlockMaintenance()

	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()

	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	priceFn := p.priceFn()
	if s == nil {
		return ImportResult{}, errors.New("usage: store not initialized")
	}
	result, err := ImportBytesWithSecret(ctx, s, priceFn, data, secret)
	if err != nil {
		return ImportResult{}, err
	}
	if err := s.rollupPendingDays(ctx); err != nil {
		log.Warnf("usage: roll up pending days after import: %v", err)
	}
	if err := p.checkpoint("TRUNCATE"); err != nil {
		log.Warnf("usage: checkpoint after import: %v", err)
	}
	return result, nil
}

func (p *Persister) priceFn() PriceFunc {
	provider, fn := p.priceSource()
	if provider != nil {
		return provider.Snapshot()
	}
	return fn
}

func (p *Persister) priceSource() (PriceProvider, PriceFunc) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.priceProvider, p.pricesFn
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
			_ = os.Remove(filepath.Join(newBaseDir, migrationReadyMarkerFile))
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
		if err := renameUsageDataDir(oldBaseDir, newBaseDir); err != nil {
			if !isCrossDeviceError(err) {
				return fmt.Errorf("move %s -> %s: %w", oldBaseDir, newBaseDir, err)
			}
			if migrateErr := migrateDataDirCrossDevice(oldBaseDir, newBaseDir); migrateErr != nil {
				return fmt.Errorf("cross-device migrate %s -> %s: %w", oldBaseDir, newBaseDir, migrateErr)
			}
		} else if errSync := syncDirectory(filepath.Dir(newBaseDir)); errSync != nil {
			return fmt.Errorf("sync destination parent %s: %w", filepath.Dir(newBaseDir), errSync)
		}
		log.Infof("usage: migrated data directory %s -> %s", oldBaseDir, newBaseDir)
		return nil
	}
	return nil
}

func migrateDataDirCrossDevice(oldBaseDir, newBaseDir string) error {
	stagingDir := newBaseDir + migrationStagingSuffix
	promoted, errRecover := recoverStagedDataDir(oldBaseDir, newBaseDir, stagingDir)
	if errRecover != nil {
		return errRecover
	}
	if !promoted {
		if errRemove := os.RemoveAll(stagingDir); errRemove != nil {
			return fmt.Errorf("remove stale staging directory %s: %w", stagingDir, errRemove)
		}
		if errCopy := copyDirRecursive(oldBaseDir, stagingDir); errCopy != nil {
			return errCopy
		}
		if errMarker := writeMigrationReadyMarker(stagingDir, oldBaseDir); errMarker != nil {
			return errMarker
		}
		if errSync := syncDirectory(stagingDir); errSync != nil {
			return fmt.Errorf("sync staging directory %s: %w", stagingDir, errSync)
		}
		if errRename := renameUsageDataDir(stagingDir, newBaseDir); errRename != nil {
			return fmt.Errorf("promote staging directory %s -> %s: %w", stagingDir, newBaseDir, errRename)
		}
		if errSync := syncDirectory(filepath.Dir(newBaseDir)); errSync != nil {
			return fmt.Errorf("sync destination parent %s: %w", filepath.Dir(newBaseDir), errSync)
		}
	}
	if errRemove := os.RemoveAll(oldBaseDir); errRemove != nil {
		log.Warnf(
			"usage: migrated %s -> %s; failed to remove old dir: %v",
			oldBaseDir, newBaseDir, errRemove,
		)
	}
	if errRemove := os.Remove(filepath.Join(newBaseDir, migrationReadyMarkerFile)); errRemove != nil &&
		!errors.Is(errRemove, os.ErrNotExist) {
		log.Warnf("usage: failed to remove migration marker from %s: %v", newBaseDir, errRemove)
	}
	return nil
}

func recoverStagedDataDir(oldBaseDir, newBaseDir, stagingDir string) (bool, error) {
	rawMarker, errRead := os.ReadFile(filepath.Join(stagingDir, migrationReadyMarkerFile))
	if errors.Is(errRead, os.ErrNotExist) {
		return false, nil
	}
	if errRead != nil {
		return false, fmt.Errorf("read migration marker from %s: %w", stagingDir, errRead)
	}
	if filepath.Clean(string(bytes.TrimSpace(rawMarker))) != filepath.Clean(oldBaseDir) {
		return false, nil
	}
	if errRename := renameUsageDataDir(stagingDir, newBaseDir); errRename != nil {
		return false, fmt.Errorf("recover staging directory %s -> %s: %w", stagingDir, newBaseDir, errRename)
	}
	if errSync := syncDirectory(filepath.Dir(newBaseDir)); errSync != nil {
		return false, fmt.Errorf("sync recovered destination parent %s: %w", filepath.Dir(newBaseDir), errSync)
	}
	return true, nil
}

func writeMigrationReadyMarker(stagingDir, oldBaseDir string) error {
	markerPath := filepath.Join(stagingDir, migrationReadyMarkerFile)
	marker, errOpen := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		return fmt.Errorf("create migration marker %s: %w", markerPath, errOpen)
	}
	if _, errWrite := io.WriteString(marker, filepath.Clean(oldBaseDir)+"\n"); errWrite != nil {
		_ = marker.Close()
		return fmt.Errorf("write migration marker %s: %w", markerPath, errWrite)
	}
	if errSync := marker.Sync(); errSync != nil {
		_ = marker.Close()
		return fmt.Errorf("sync migration marker %s: %w", markerPath, errSync)
	}
	if errClose := marker.Close(); errClose != nil {
		return fmt.Errorf("close migration marker %s: %w", markerPath, errClose)
	}
	return nil
}

func isCrossDeviceError(err error) bool {
	if err == nil {
		return false
	}
	if linkErr, ok := errors.AsType[*os.LinkError](err); ok {
		return errors.Is(linkErr.Err, syscall.EXDEV) ||
			isNativeCrossDeviceError(linkErr.Err)
	}
	return errors.Is(err, syscall.EXDEV) || isNativeCrossDeviceError(err)
}

func copyDirRecursive(src, dst string) error {
	directories := make([]string, 0)
	errWalk := filepath.WalkDir(
		src, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, errRel := filepath.Rel(src, path)
			if errRel != nil {
				return errRel
			}
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				if errMkdir := os.MkdirAll(target, 0o700); errMkdir != nil {
					return errMkdir
				}
				directories = append(directories, target)
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to migrate symlink %s", path)
			}
			info, errInfo := d.Info()
			if errInfo != nil {
				return errInfo
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("refusing to migrate non-regular file %s", path)
			}
			return copyRegularFile(path, target)
		},
	)
	if errWalk != nil {
		return errWalk
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if errSync := syncDirectory(directories[index]); errSync != nil {
			return fmt.Errorf("sync copied directory %s: %w", directories[index], errSync)
		}
	}
	return nil
}

func copyRegularFile(src, dst string) error {
	info, errStat := os.Stat(src)
	if errStat != nil {
		return errStat
	}
	in, errOpen := os.Open(src)
	if errOpen != nil {
		return errOpen
	}
	defer func() { _ = in.Close() }()
	if errMkdir := os.MkdirAll(filepath.Dir(dst), 0o700); errMkdir != nil {
		return errMkdir
	}
	out, errCreate := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if errCreate != nil {
		return errCreate
	}
	copied, errCopy := io.Copy(out, in)
	if errCopy != nil {
		_ = out.Close()
		return errCopy
	}
	if copied != info.Size() {
		_ = out.Close()
		return fmt.Errorf("copied %d bytes from %s, want %d", copied, src, info.Size())
	}
	if errSync := out.Sync(); errSync != nil {
		_ = out.Close()
		return errSync
	}
	return out.Close()
}
