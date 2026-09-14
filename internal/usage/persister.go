package usage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
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

	// MaxLegacySnapshotEvents and MaxLegacySnapshotDetailBytes bound the
	// detail-heavy compatibility snapshot. Modern management clients use the
	// aggregate and paginated read APIs.
	MaxLegacySnapshotEvents      int64 = 10_000
	MaxLegacySnapshotDetailBytes int64 = 32 << 20

	// JSON strings may expand sixfold through HTML-safe escaping. Metadata uses
	// a larger factor to cover decoded container nodes and repeated JSON buffers.
	legacySnapshotEventOverhead       int64 = 1024
	legacySnapshotMetadataExpansion   int64 = 16
	legacySnapshotJSONStringExpansion int64 = 6
	legacySnapshotHeaderNameOverhead  int64 = 128
	legacySnapshotHeaderValueOverhead int64 = 64
)

// PriceFunc returns the pricing for a model: prices are in USD per 1M tokens.
type PriceFunc func(model string) (prompt, completion, cache float64, found bool)

var (
	renameUsageDataDir = os.Rename
	statUsageFile      = os.Stat
)

// ErrLegacySnapshotTooLarge reports that the detail-heavy compatibility
// response would exceed its event-count or estimated-size limit.
var ErrLegacySnapshotTooLarge = errors.New("usage: legacy snapshot exceeds size limit")

// Persister is the SQLite-backed coordinator for usage data. It owns the
// Store + Writer + rollup goroutine, exposes a stable surface to the rest of
// the codebase, and hides the schema / WAL / dim_table details.
type Persister struct {
	mu           sync.RWMutex
	lifecycleMu  sync.Mutex
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
	dbSizeCapped  atomic.Bool
	stopRequested atomic.Bool

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
		w.setPriceFunc(fn)
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
		w.setPriceProvider(provider)
	}
}

// HasPricing reports whether a price lookup function has been configured.
func (p *Persister) HasPricing() bool {
	if p == nil {
		return false
	}
	pricing := p.currentPricingSnapshot()
	return pricing.priceFn != nil || pricing.detailedPriceFn != nil
}

// IsReady reports whether Start successfully opened the SQLite store.
// Service callers branch on this before registering their logger plugin; when
// startup fails, they keep the instance persister nil so records continue into
// that service's in-memory RequestStatistics.
func (p *Persister) IsReady() bool {
	if p == nil || p.stopRequested.Load() {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.store != nil && p.writer != nil
}

func (p *Persister) apiKeyReservationStore() *Store {
	if p == nil || p.stopRequested.Load() {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.store
}

func (p *Persister) ReserveAPIKey(
	ctx context.Context,
	request coreusage.APIKeyReservationRequest,
) (coreusage.APIKeyReservation, error) {
	store := p.apiKeyReservationStore()
	if store == nil {
		return coreusage.APIKeyReservation{}, errors.New("usage: API key reservation store is unavailable")
	}
	return store.ReserveAPIKey(ctx, request)
}

func (p *Persister) ReleaseAPIKeyReservation(ctx context.Context, reservationID, reason string) error {
	store := p.apiKeyReservationStore()
	if store == nil {
		return errors.New("usage: API key reservation store is unavailable")
	}
	return store.ReleaseAPIKeyReservation(ctx, reservationID, reason)
}

func (p *Persister) MarkAPIKeyReservationUnknown(ctx context.Context, reservationID, reason string) error {
	store := p.apiKeyReservationStore()
	if store == nil {
		return errors.New("usage: API key reservation store is unavailable")
	}
	return store.MarkAPIKeyReservationUnknown(ctx, reservationID, reason)
}

func (p *Persister) CommitAPIKeyReservation(ctx context.Context, reservationID, reason string) error {
	store := p.apiKeyReservationStore()
	if store == nil {
		return errors.New("usage: API key reservation store is unavailable")
	}
	return store.CommitAPIKeyReservation(ctx, reservationID, reason)
}

// DBSize returns the on-disk size of events.db plus its WAL/SHM sidecars.
// Returns 0 when the store is not yet open or the file does not exist.
func (p *Persister) DBSize() int64 {
	if p == nil || p.baseDir == "" {
		return 0
	}
	var total int64
	for _, name := range []string{dbFileName, dbFileName + "-wal", dbFileName + "-shm"} {
		info, err := statUsageFile(filepath.Join(p.baseDir, name))
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

func (p *Persister) dbSizeCapLimit() int64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	maxDBSizeMB := p.retention.MaxDBSizeMB
	p.mu.RUnlock()
	if maxDBSizeMB <= 0 {
		return 0
	}
	return int64(maxDBSizeMB) << 20
}

func (p *Persister) refreshDBSizeCap() bool {
	maxBytes := p.dbSizeCapLimit()
	if maxBytes == 0 {
		p.dbSizeCapped.Store(false)
		return false
	}
	capped := p.DBSize() >= maxBytes
	p.dbSizeCapped.Store(capped)
	return capped
}

func (p *Persister) overDBSizeCap() bool {
	return p != nil && p.refreshDBSizeCap()
}

func (p *Persister) dbSizeCapReached() bool {
	return p != nil && p.dbSizeCapLimit() > 0 && p.dbSizeCapped.Load()
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
	return checkMigrationStatus(ctx, s)
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
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopRequested.Load() || p.IsReady() {
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
	store, err := openStore(dbPath)
	if err != nil {
		log.Errorf(
			"usage: failed to open SQLite store %s: %v — falling back to in-memory request statistics; no usage data will be persisted across restarts; legacy summary.json / detail/* JSON files are NOT touched, so manual migration is still possible after the underlying issue is fixed",
			dbPath, err,
		)
		return
	}
	if err = store.setMaxSizeBytes(ctx, p.dbSizeCapLimit()); err != nil {
		_ = store.Close()
		log.Errorf(
			"usage: failed to apply SQLite DB size limit: %v — falling back to in-memory request statistics",
			err,
		)
		return
	}

	p.pricingMu.RLock()
	priceProvider, configuredPriceFn := p.priceSource()
	priceFn := configuredPriceFn
	if priceProvider != nil {
		priceFn = priceProvider.Snapshot()
	}
	if err := migrateLegacyToSQLite(ctx, store, priceFn, p.baseDir); err != nil {
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
	writer.setCapacityCheck(p.refreshDBSizeCap)
	p.refreshDBSizeCap()
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	writer.start(lifecycleCtx)

	rollupStop := store.startRollup(lifecycleCtx)

	p.mu.Lock()
	p.store = store
	p.writer = writer
	p.lifecycleCtx = lifecycleCtx
	p.lifecycleCancel = lifecycleCancel
	p.mu.Unlock()
	p.pricingMu.RUnlock()

	p.restoreCostRecalculation(lifecycleCtx)
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
	p.stopRequested.Store(true)
	p.stopOnce.Do(
		func() {
			go func() {
				p.lifecycleMu.Lock()
				defer p.lifecycleMu.Unlock()
				p.mu.RLock()
				stop := p.stopFn
				cancel := p.lifecycleCancel
				p.mu.RUnlock()
				if cancel != nil {
					cancel()
				}
				if stop != nil {
					stop()
				}
				close(p.stopDone)
			}()
		},
	)
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
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
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

// Record submits a usage record and applies writer backpressure until it is
// accepted or the persister stops.
func (p *Persister) Record(detail FlatDetail) bool {
	if p == nil {
		return false
	}
	if p.dbSizeCapReached() {
		log.Warn("usage: SQLite DB size cap reached, live usage record was not accepted")
		return false
	}
	p.mu.RLock()
	w := p.writer
	p.mu.RUnlock()
	if w == nil {
		log.Warn("usage: writer unavailable, live usage record was not accepted")
		return false
	}
	return w.submit(detail)
}

// SubmitRequestLogArtifact implements logging.RequestLogArtifactSink.
func (p *Persister) SubmitRequestLogArtifact(artifact logging.RequestLogArtifact) bool {
	if p == nil {
		log.Warn("usage: persister unavailable, request log artifact was not accepted")
		return false
	}
	p.mu.RLock()
	w := p.writer
	p.mu.RUnlock()
	if w == nil {
		log.Warn("usage: writer unavailable, request log artifact was not accepted")
		return false
	}
	return w.SubmitRequestLogArtifact(artifact)
}

// QueryRequestLogArtifactContext returns the latest persisted lifecycle state.
func (p *Persister) QueryRequestLogArtifactContext(
	parent context.Context,
	requestID string,
) (logging.RequestLogArtifact, bool, error) {
	if p == nil {
		return logging.RequestLogArtifact{}, false, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil {
		return logging.RequestLogArtifact{}, false, errors.New("usage: store not initialized")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	return s.queryRequestLogArtifact(ctx, requestID)
}

// HasEventRequestIDContext reports whether a legacy event carries the request ID.
func (p *Persister) HasEventRequestIDContext(parent context.Context, requestID string) (bool, error) {
	if p == nil {
		return false, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil {
		return false, errors.New("usage: store not initialized")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	return s.hasEventRequestID(ctx, requestID)
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
	return s.querySummary(ctx, from, to, filters, granularity, includeGroups)
}

func (p *Persister) QueryWithAuthIndexGranularity(
	from, to time.Time,
	filters EventFilters,
	granularity string,
	authIndexGranularity string,
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
	return s.queryWithAuthIndexGranularity(
		ctx,
		from,
		to,
		filters,
		granularity,
		authIndexGranularity,
		includeGroups,
	)
}

// QueryRequestCountsContext returns the current request and failure totals
// without materializing per-request details.
func (p *Persister) QueryRequestCountsContext(parent context.Context) (int64, int64, error) {
	if p == nil {
		return 0, 0, errors.New("usage: nil persister")
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return 0, 0, errors.New("usage: store not initialized")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	var totalRequests int64
	var failureCount int64
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*), COALESCE(SUM(failed), 0) FROM events",
	).Scan(&totalRequests, &failureCount); err != nil {
		return 0, 0, fmt.Errorf("usage: query request counts: %w", err)
	}
	return totalRequests, failureCount, nil
}

// QueryEvents serves /usage/events with SQL-native pagination.
func (p *Persister) QueryEvents(
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	sortDesc bool,
) ([]FlatDetail, int, error) {
	return p.QueryEventsContext(
		context.Background(),
		from,
		to,
		filters,
		page,
		pageSize,
		sortField,
		sortDesc,
	)
}

func (p *Persister) QueryEventsContext(
	parent context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	sortDesc bool,
) ([]FlatDetail, int, error) {
	return p.queryEventsContext(parent, from, to, filters, page, pageSize, sortField, sortDesc, false)
}

func (p *Persister) QueryCompactEventsContext(
	parent context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	sortDesc bool,
) ([]FlatDetail, int, error) {
	return p.queryEventsContext(parent, from, to, filters, page, pageSize, sortField, sortDesc, true)
}

func (p *Persister) queryEventsContext(
	parent context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	sortDesc bool,
	compact bool,
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
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if compact {
		return s.queryCompactEventDetails(ctx, from, to, filters, page, pageSize, sortField, sortDesc)
	}
	return s.queryEventDetails(ctx, from, to, filters, page, pageSize, sortField, sortDesc)
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
	return s.queryBackupEventSet(ctx, from, to, filters)
}

func (p *Persister) checkpointContext(parent context.Context, mode string) error {
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
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

	deleteResult, err := tx.ExecContext(ctx, "DELETE FROM events WHERE ts_ns < ?", cutoffNs)
	if err != nil {
		return fmt.Errorf("delete expired events: %w", err)
	}
	deleted, err := deleteResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("count expired events: %w", err)
	}
	if deleted == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit empty trim tx: %w", err)
		}
		committed = true
		return nil
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
	store := p.store
	p.mu.Unlock()
	if store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := store.setMaxSizeBytes(ctx, p.dbSizeCapLimit()); err != nil {
			log.Errorf("usage: apply updated SQLite DB size limit: %v", err)
		}
		cancel()
	}
	p.refreshDBSizeCap()
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

// QueryAggregateSnapshotContext reconstructs the legacy aggregate fields from
// hour buckets without loading per-request details.
func (p *Persister) QueryAggregateSnapshotContext(parent context.Context) (StatisticsSnapshot, error) {
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
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("begin aggregate snapshot tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(
		ctx,
		`SELECT bucket_ts_ns,
		        SUM(requests),
		        SUM(success),
		        SUM(failure),
		        SUM(total_tokens)
		   FROM hour_bucket
		  GROUP BY bucket_ts_ns
		  ORDER BY bucket_ts_ns`,
	)
	if err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("query aggregate snapshot buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	addAggregate := func(current, delta int64, name string) (int64, error) {
		value, ok := coreusage.AddNonNegativeInt64(current, delta)
		if !ok {
			return 0, fmt.Errorf("usage: aggregate snapshot %s overflows int64", name)
		}
		return value, nil
	}
	for rows.Next() {
		var bucketNs int64
		var requests int64
		var success int64
		var failure int64
		var totalTokens int64
		if err = rows.Scan(
			&bucketNs,
			&requests,
			&success,
			&failure,
			&totalTokens,
		); err != nil {
			return StatisticsSnapshot{}, fmt.Errorf("scan aggregate snapshot bucket: %w", err)
		}

		bucketTime := time.Unix(0, bucketNs).UTC()
		dayKey := bucketTime.Format("2006-01-02")
		hourKey := formatHour(bucketTime.Hour())

		if snap.TotalRequests, err = addAggregate(snap.TotalRequests, requests, "request count"); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.SuccessCount, err = addAggregate(snap.SuccessCount, success, "success count"); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.FailureCount, err = addAggregate(snap.FailureCount, failure, "failure count"); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.TotalTokens, err = addAggregate(snap.TotalTokens, totalTokens, "token count"); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.RequestsByDay[dayKey], err = addAggregate(
			snap.RequestsByDay[dayKey], requests, "daily request count",
		); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.RequestsByHour[hourKey], err = addAggregate(
			snap.RequestsByHour[hourKey], requests, "hourly request count",
		); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.TokensByDay[dayKey], err = addAggregate(
			snap.TokensByDay[dayKey], totalTokens, "daily token count",
		); err != nil {
			return StatisticsSnapshot{}, err
		}
		if snap.TokensByHour[hourKey], err = addAggregate(
			snap.TokensByHour[hourKey], totalTokens, "hourly token count",
		); err != nil {
			return StatisticsSnapshot{}, err
		}
	}
	if err = rows.Err(); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("iterate aggregate snapshot buckets: %w", err)
	}
	if err = rows.Close(); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("close aggregate snapshot buckets: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("commit aggregate snapshot tx: %w", err)
	}
	committed = true
	return snap, nil
}

// querySnapshot reconstructs the full v1-style snapshot and reports storage
// errors instead of returning an incomplete snapshot.
func (p *Persister) querySnapshot() (StatisticsSnapshot, error) {
	return p.querySnapshotFilteredContext(
		context.Background(),
		time.Time{},
		time.Time{},
		EventFilters{},
		MaxLegacySnapshotEvents,
		MaxLegacySnapshotDetailBytes,
	)
}

// QuerySnapshotLimitedContext reconstructs the v1-style snapshot while
// rejecting result sets that exceed the event or estimated-detail limits
// before loading any details.
func (p *Persister) QuerySnapshotLimitedContext(
	parent context.Context,
	maxEvents int64,
) (StatisticsSnapshot, error) {
	if maxEvents <= 0 {
		maxEvents = MaxLegacySnapshotEvents
	}
	return p.querySnapshotFilteredContext(
		parent,
		time.Time{},
		time.Time{},
		EventFilters{},
		maxEvents,
		MaxLegacySnapshotDetailBytes,
	)
}

// querySnapshotFiltered reconstructs a filtered v1-style snapshot. Zero-valued
// from/to means "no time bound"; an empty EventFilters means "no dimension
// filter". Every storage error aborts the query so callers never receive
// partial usage data.
func (p *Persister) querySnapshotFiltered(
	from, to time.Time,
	filters EventFilters,
) (StatisticsSnapshot, error) {
	return p.querySnapshotFilteredContext(
		context.Background(),
		from,
		to,
		filters,
		MaxLegacySnapshotEvents,
		MaxLegacySnapshotDetailBytes,
	)
}

func (p *Persister) querySnapshotFilteredContext(
	parent context.Context,
	from, to time.Time,
	filters EventFilters,
	maxEvents int64,
	maxDetailBytes int64,
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
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
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
	if maxEvents > 0 {
		limitArgs := append([]any(nil), eventsArgs...)
		limitArgs = append(limitArgs, maxEvents)
		limitSQL := "SELECT EXISTS(SELECT 1 FROM events" + eventsWhere + " LIMIT 1 OFFSET ?)"
		var exceedsLimit int
		if err := tx.QueryRowContext(ctx, limitSQL, limitArgs...).Scan(&exceedsLimit); err != nil {
			return StatisticsSnapshot{}, fmt.Errorf("check snapshot event limit: %w", err)
		}
		if exceedsLimit != 0 {
			return StatisticsSnapshot{}, fmt.Errorf(
				"%w: more than %d events",
				ErrLegacySnapshotTooLarge,
				maxEvents,
			)
		}
	}
	if maxDetailBytes > 0 {
		detailSizeSQL := `SELECT COALESCE(SUM(
			? +
			? * COALESCE(LENGTH(CAST(events.metadata AS BLOB)), 0) +
			? * (
				COALESCE(LENGTH(CAST(m.name AS BLOB)), 0) +
				COALESCE(LENGTH(CAST(s.name AS BLOB)), 0) +
				COALESCE(LENGTH(CAST(a.name AS BLOB)), 0) +
				COALESCE(LENGTH(CAST(k.name AS BLOB)), 0)
			)
		), 0)
		FROM events
		JOIN dim_model m ON m.id = events.model_id
		LEFT JOIN dim_source s ON s.id = events.source_id
		LEFT JOIN dim_auth_index a ON a.id = events.auth_index_id
		LEFT JOIN dim_api_key k ON k.id = events.api_key_id` + eventsWhere
		detailSizeArgs := []any{
			legacySnapshotEventOverhead,
			legacySnapshotMetadataExpansion,
			legacySnapshotJSONStringExpansion,
		}
		detailSizeArgs = append(detailSizeArgs, eventsArgs...)
		var estimatedDetailBytes int64
		if err := tx.QueryRowContext(ctx, detailSizeSQL, detailSizeArgs...).Scan(&estimatedDetailBytes); err != nil {
			return StatisticsSnapshot{}, fmt.Errorf("check snapshot detail size: %w", err)
		}
		if estimatedDetailBytes > maxDetailBytes {
			return StatisticsSnapshot{}, fmt.Errorf(
				"%w: estimated details use %d bytes",
				ErrLegacySnapshotTooLarge,
				estimatedDetailBytes,
			)
		}
	}

	// APIs[apiName].Models[modelName].Details[] must hit the events table.
	eventsSQL := `
		SELECT events.ts_ns,
		       (SELECT name FROM dim_model      WHERE id = events.model_id),
		       (SELECT name FROM dim_source     WHERE id = events.source_id),
		       (SELECT name FROM dim_auth_index WHERE id = events.auth_index_id),
		       (SELECT name FROM dim_api_key    WHERE id = events.api_key_id),
		       events.input_tokens, events.output_tokens, events.reasoning_tokens, events.cached_tokens,
		       events.cache_read_tokens, events.cache_write_tokens, events.total_tokens,
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
			&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens, &t.CachedTokens,
			&t.CacheReadTokens, &t.CacheWriteTokens, &t.TotalTokens,
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
		detail.Generate = flat.Generate
		detail.ClientIP = flat.ClientIP
		detail.XForwardedFor = flat.XForwardedFor
		detail.UserAgent = flat.UserAgent
		detail.AccountingVersion = flat.AccountingVersion
		detail.TokenBreakdown = flat.TokenBreakdown
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
	TaskID           string  `json:"task_id,omitempty"`
	RecalculatedDays int     `json:"recalculated_days"`
	TotalCost        float64 `json:"total_cost"`
	AlreadyRunning   bool    `json:"already_running,omitempty"`
}

// CostRecalculationStatus reports the durable asynchronous job state.
type CostRecalculationStatus struct {
	Status             string  `json:"status"`
	Running            bool    `json:"running"`
	TaskID             string  `json:"task_id,omitempty"`
	Phase              string  `json:"phase,omitempty"`
	PriceRevision      string  `json:"price_revision,omitempty"`
	ProcessedEvents    int64   `json:"processed_events,omitempty"`
	TotalEvents        int64   `json:"total_events,omitempty"`
	ChangedEvents      int64   `json:"changed_events,omitempty"`
	RollupTotalDays    int     `json:"rollup_total_days,omitempty"`
	RollupDoneDays     int     `json:"rollup_done_days,omitempty"`
	DirtyDaysRemaining int     `json:"dirty_days_remaining,omitempty"`
	Percent            float64 `json:"percent,omitempty"`
	RecalculatedDays   int     `json:"recalculated_days,omitempty"`
	TotalCost          float64 `json:"total_cost,omitempty"`
	Error              string  `json:"error,omitempty"`
	StartedAt          string  `json:"started_at,omitempty"`
	UpdatedAt          string  `json:"updated_at,omitempty"`
	Resumable          bool    `json:"resumable,omitempty"`
	Resumed            bool    `json:"resumed,omitempty"`
	RestartReason      string  `json:"restart_reason,omitempty"`
}

// recalculateCosts re-prices every event using the current PriceFunc, then
// rebuilds hour_bucket and day_bucket from the updated events table. Heavy
// SQL — this is intended for occasional admin-triggered recalculation, not
// per-request use.
func (p *Persister) recalculateCosts() (RecalculateCostsResult, error) {
	return p.RecalculateCostsContext(context.Background())
}

// RecalculateCostsContext recalculates historical costs and allows lock waits,
// SQL work, caller cancellation, and shutdown cancellation to share one context.
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
	status := p.costRecalcStatus
	if err != nil {
		status.Status = "error"
		status.Running = false
		status.Error = err.Error()
	} else {
		status.Status = "ok"
		status.Running = false
		status.Phase = "complete"
		status.RecalculatedDays = result.RecalculatedDays
		status.TotalCost = result.TotalCost
		if status.TaskID == "" {
			status.TaskID = result.TaskID
		}
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
	return p.recalculateCostsIncrementalContext(parent)
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
		fmt.Sprintf(
			`
		INSERT INTO hour_bucket (
		    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
		    requests, success, failure,
		    input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			    cache_read_tokens, cache_write_tokens, total_tokens,
		    cost_micro,
			    complete_requests, partial_requests, unpriced_requests,
			    complete_tokens, partial_tokens, unpriced_tokens,
			    latency_ms_total, latency_samples
		)
		SELECT
		    %s AS bucket_ts_ns,
		    model_id, credential_id, api_key_id, provider_id,
		    COUNT(*) AS requests,
		    SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END) AS success,
		    SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) AS failure,
		    SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens),
			    SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(total_tokens),
		    SUM(cost_micro),
			    SUM(CASE WHEN cost_status = 'complete' THEN 1 ELSE 0 END),
			    SUM(CASE WHEN cost_status = 'partial' THEN 1 ELSE 0 END),
			    SUM(CASE WHEN cost_status = 'unpriced' THEN 1 ELSE 0 END),
			    SUM(CASE WHEN cost_status = 'complete' THEN total_tokens ELSE 0 END),
			    SUM(CASE WHEN cost_status = 'partial' THEN total_tokens ELSE 0 END),
			    SUM(CASE WHEN cost_status = 'unpriced' THEN total_tokens ELSE 0 END),
			    0, 0
		  FROM events
		 GROUP BY %s, model_id, credential_id, api_key_id, provider_id
	`, hourExpr, hourExpr,
		),
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
		fmt.Sprintf(
			`
		INSERT INTO day_bucket (
		    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
		    requests, success, failure,
		    input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			    cache_read_tokens, cache_write_tokens, total_tokens,
		    cost_micro,
			    complete_requests, partial_requests, unpriced_requests,
			    complete_tokens, partial_tokens, unpriced_tokens,
			    latency_ms_total, latency_samples
		)
		SELECT
		    %s AS bucket_ts_ns,
		    model_id, credential_id, api_key_id, provider_id,
		    SUM(requests), SUM(success), SUM(failure),
		    SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens),
			    SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(total_tokens),
		    SUM(cost_micro),
			    SUM(complete_requests), SUM(partial_requests), SUM(unpriced_requests),
			    SUM(complete_tokens), SUM(partial_tokens), SUM(unpriced_tokens),
			    SUM(latency_ms_total), SUM(latency_samples)
		  FROM hour_bucket
		 GROUP BY %s, model_id, credential_id, api_key_id, provider_id
		`, dayExpr, dayExpr,
		),
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

const repriceEventPageSize = 512

type repriceEvent struct {
	id             int64
	tsNs           int64
	tokens         tokenStats
	metadata       sql.NullString
	pricingReceipt *PricingReceipt
	current        costResult
	updated        costResult
}

func repriceModelEvents(
	ctx context.Context,
	tx *sql.Tx,
	model modelRow,
	pricing pricingSnapshot,
) error {
	update, err := tx.PrepareContext(
		ctx,
		`UPDATE events
		    SET cost_micro = ?, cost_status = ?, cost_reason = ?
		  WHERE id = ?
		    AND (cost_micro <> ? OR cost_status <> ? OR cost_reason <> ?)`,
	)
	if err != nil {
		return fmt.Errorf("prepare cost updates for %s: %w", model.name, err)
	}
	defer func() { _ = update.Close() }()

	var afterID int64
	for {
		rows, errQuery := tx.QueryContext(
			ctx,
			`SELECT id, ts_ns,
			        input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			        cache_read_tokens, cache_write_tokens, total_tokens,
			        metadata, pricing_receipt, cost_micro, cost_status, cost_reason
			   FROM events
			  WHERE model_id = ? AND id > ?
			  ORDER BY id
			  LIMIT ?`,
			model.id,
			afterID,
			repriceEventPageSize,
		)
		if errQuery != nil {
			return fmt.Errorf("query costs for %s: %w", model.name, errQuery)
		}

		batch := make([]repriceEvent, 0, repriceEventPageSize)
		for rows.Next() {
			var event repriceEvent
			var pricingReceipt sql.NullString
			if errScan := rows.Scan(
				&event.id,
				&event.tsNs,
				&event.tokens.InputTokens,
				&event.tokens.OutputTokens,
				&event.tokens.ReasoningTokens,
				&event.tokens.CachedTokens,
				&event.tokens.CacheReadTokens,
				&event.tokens.CacheWriteTokens,
				&event.tokens.TotalTokens,
				&event.metadata,
				&pricingReceipt,
				&event.current.Micro,
				&event.current.Status,
				&event.current.Reason,
			); errScan != nil {
				_ = rows.Close()
				return fmt.Errorf("scan costs for %s: %w", model.name, errScan)
			}
			event.tokens, err = validatedTokenStats(event.tokens)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("validate tokens for %s event %d: %w", model.name, event.id, err)
			}
			event.pricingReceipt, err = decodePricingReceipt(pricingReceipt)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("decode pricing receipt for %s event %d: %w", model.name, event.id, err)
			}
			eventTime := time.Unix(0, event.tsNs).UTC()
			if event.pricingReceipt != nil {
				event.updated, err = computeCurrentCatalogCostFromPricingReceipt(
					event.tokens,
					event.pricingReceipt,
					pricing,
					eventTime,
				)
			} else {
				var breakdown *coreusage.TokenBreakdown
				breakdown, err = decodeEventTokenBreakdown(event.metadata)
				if err == nil {
					event.updated, err = computeCost(
						event.tokens,
						breakdown,
						pricing,
						model.name,
						eventTime,
					)
				}
			}
			if err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, event)
			afterID = event.id
		}
		if errRows := rows.Err(); errRows != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate costs for %s: %w", model.name, errRows)
		}
		if errClose := rows.Close(); errClose != nil {
			return fmt.Errorf("close costs for %s: %w", model.name, errClose)
		}
		if len(batch) == 0 {
			return nil
		}

		for i := range batch {
			event := &batch[i]
			if _, errExec := update.ExecContext(
				ctx,
				event.updated.Micro,
				event.updated.Status,
				event.updated.Reason,
				event.id,
				event.updated.Micro,
				event.updated.Status,
				event.updated.Reason,
			); errExec != nil {
				return fmt.Errorf("update cost for %s event %d: %w", model.name, event.id, errExec)
			}
		}
	}
}

type dbQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
	if parent == nil {
		parent = context.Background()
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
	result, err := importBytesWithSecret(ctx, s, priceFn, data, secret)
	if err != nil {
		return ImportResult{}, err
	}
	if err := handlePostImportMaintenanceError(
		parent,
		&result,
		"roll up pending days after import",
		s.rollupPendingDays(ctx),
	); err != nil {
		return result, err
	}
	if err := handlePostImportMaintenanceError(
		parent,
		&result,
		"checkpoint after import",
		p.checkpointContext(ctx, "TRUNCATE"),
	); err != nil {
		return result, err
	}
	return result, nil
}

func handlePostImportMaintenanceError(
	parent context.Context,
	result *ImportResult,
	operation string,
	maintenanceErr error,
) error {
	if maintenanceErr == nil {
		return nil
	}
	if err := parent.Err(); err != nil {
		return err
	}
	result.Warning = "usage imported, but post-import maintenance did not finish"
	log.Warnf("usage: %s: %v", operation, maintenanceErr)
	return nil
}

func (p *Persister) currentPricingSnapshot() pricingSnapshot {
	p.pricingMu.RLock()
	defer p.pricingMu.RUnlock()
	provider, fn := p.priceSource()
	if provider == nil {
		return pricingSnapshot{priceFn: fn}
	}
	if detailedProvider, ok := provider.(detailedPriceProvider); ok {
		return pricingSnapshot{detailedPriceFn: detailedProvider.DetailedSnapshot()}
	}
	return pricingSnapshot{priceFn: provider.Snapshot()}
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
	for _, directory := range slices.Backward(directories) {
		if errSync := syncDirectory(directory); errSync != nil {
			return fmt.Errorf("sync copied directory %s: %w", directory, errSync)
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
