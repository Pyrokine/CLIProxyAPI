package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	writerChanCap              = 4096
	writerBatchSize            = 100
	writerFlushInterval        = 100 * time.Millisecond
	writerFlushTimeout         = 10 * time.Second
	writerShutdownFlushTimeout = 2 * time.Second
)

var (
	errWriterFlushTransient = errors.New("usage: transient writer flush failure")
	errWriterStorageCap     = errors.New("usage: SQLite DB size cap reached")
)

// Writer ingests FlatDetail records and persists them in batched transactions
// against a Store. A single goroutine owns all SQL writes so we never collide
// with SQLite's "one writer at a time" rule.
type Writer struct {
	store         *Store
	priceFn       PriceFunc
	priceProvider PriceProvider
	pricingMu     *sync.RWMutex

	ch         chan FlatDetail
	artifactCh chan logging.RequestLogArtifact
	done       chan struct{}
	stop       chan struct{}
	started    atomic.Bool
	stopOnce   sync.Once
	submitMu   sync.RWMutex
	stopped    atomic.Bool

	eventAccepted     atomic.Uint64
	eventCommitted    atomic.Uint64
	eventRejected     atomic.Uint64
	artifactAccepted  atomic.Uint64
	artifactCommitted atomic.Uint64
	artifactRejected  atomic.Uint64

	capacityMu   sync.RWMutex
	capacityFull func() bool

	priceMu sync.RWMutex // guards priceFn during runtime swaps
}

// writerStats is an eventually consistent snapshot of accepted records and
// their terminal persistence outcomes.
type writerStats struct {
	EventAccepted     uint64
	EventCommitted    uint64
	EventRejected     uint64
	ArtifactAccepted  uint64
	ArtifactCommitted uint64
	ArtifactRejected  uint64
}

type eventFlushResult struct {
	committed int
	rejected  int
	retry     []FlatDetail
	err       error
}

type pricingSnapshot struct {
	priceFn         PriceFunc
	detailedPriceFn detailedPriceFunc
	revision        string
}

type writerPriceSnapshot struct {
	pricing pricingSnapshot
	release func()
}

func (s *writerPriceSnapshot) close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

// newWriter wires a writer to a store. priceFn may be nil — events still land
// on disk, just with cost_micro = 0 until pricing is configured.
func newWriter(store *Store, priceFn PriceFunc) *Writer {
	return newWriterWithPricingGate(store, priceFn, nil)
}

func newWriterWithPricingGate(store *Store, priceFn PriceFunc, pricingMu *sync.RWMutex) *Writer {
	return newWriterWithPriceProviderAndGate(store, nil, priceFn, pricingMu)
}

func newWriterWithPriceProviderAndGate(
	store *Store,
	provider PriceProvider,
	priceFn PriceFunc,
	pricingMu *sync.RWMutex,
) *Writer {
	return &Writer{
		store:         store,
		priceFn:       priceFn,
		priceProvider: provider,
		pricingMu:     pricingMu,
		ch:            make(chan FlatDetail, writerChanCap),
		artifactCh:    make(chan logging.RequestLogArtifact, writerChanCap),
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
	}
}

func (w *Writer) acquirePriceSnapshot() *writerPriceSnapshot {
	if w.pricingMu != nil {
		w.pricingMu.RLock()
	}
	w.priceMu.RLock()
	pricing := pricingSnapshot{priceFn: w.priceFn}
	if provider := w.priceProvider; provider != nil {
		if revisionProvider, ok := provider.(detailedPriceRevisionProvider); ok {
			detailedPriceFn, revision := revisionProvider.DetailedSnapshotWithRevision()
			pricing = pricingSnapshot{detailedPriceFn: detailedPriceFn, revision: revision}
		} else if detailedProvider, ok := provider.(detailedPriceProvider); ok {
			pricing = pricingSnapshot{detailedPriceFn: detailedProvider.DetailedSnapshot()}
		} else {
			pricing = pricingSnapshot{priceFn: provider.Snapshot()}
		}
	}
	return &writerPriceSnapshot{
		pricing: pricing,
		release: func() {
			w.priceMu.RUnlock()
			if w.pricingMu != nil {
				w.pricingMu.RUnlock()
			}
		},
	}
}

// start launches the writer goroutine. Calling start more than once is a no-op
// so callers don't need their own guard.
func (w *Writer) start(ctx context.Context) {
	if w == nil || !w.started.CompareAndSwap(false, true) {
		return
	}
	go w.loop(ctx)
}

// Stop closes the input channel, lets the loop drain anything in-flight, then
// waits for shutdown. Idempotent.
func (w *Writer) Stop() {
	if w == nil {
		return
	}
	w.requestStop()
	if w.started.Load() {
		<-w.done
	}
}

func (w *Writer) requestStop() {
	w.stopOnce.Do(
		func() {
			w.stopped.Store(true)
			close(w.stop)
			w.submitMu.Lock()
			close(w.ch)
			close(w.artifactCh)
			w.submitMu.Unlock()
		},
	)
}

// setPriceFunc lets callers swap pricing at runtime (e.g. after a model
// catalog refresh) without restarting the writer.
func (w *Writer) setPriceFunc(fn PriceFunc) {
	if w == nil {
		return
	}
	w.priceMu.Lock()
	w.priceFn = fn
	w.priceProvider = nil
	w.priceMu.Unlock()
}

func (w *Writer) setCapacityCheck(capacityFull func() bool) {
	if w == nil {
		return
	}
	w.capacityMu.Lock()
	w.capacityFull = capacityFull
	w.capacityMu.Unlock()
}

func (w *Writer) storageCapacityReached() bool {
	if w == nil {
		return false
	}
	w.capacityMu.RLock()
	capacityFull := w.capacityFull
	w.capacityMu.RUnlock()
	return capacityFull != nil && capacityFull()
}

func (w *Writer) setPriceProvider(provider PriceProvider) {
	if w == nil {
		return
	}
	w.priceMu.Lock()
	w.priceFn = nil
	w.priceProvider = provider
	w.priceMu.Unlock()
}

// submit applies backpressure until the record is accepted or the writer stops.
func (w *Writer) submit(detail FlatDetail) bool {
	if w == nil {
		return false
	}
	// R-458/R-459:在入口统一规范化 model/source/api_key 字段(trim、空→
	// unknown、统一 case),保证 SQLite 里的 dim_* 表不会出现 "unknown" /
	// "Unknown" / "UNKNOWN" 三个不同的 id,也避免旧 JSON 导入时的脏数据
	// 在 by_model 等聚合里以分裂形式出现。
	detail.Model = normaliseDimName(detail.Model, "unknown")
	detail.Source = normaliseDimName(detail.Source, "")
	detail.APIKey = normaliseDimName(detail.APIKey, "")
	detail.AuthIndex = normaliseDimName(detail.AuthIndex, "")
	detail.Provider = normaliseDimName(detail.Provider, "")

	priceSnapshot := w.acquirePriceSnapshot()
	cost, receipt, err := buildPricingReceipt(&detail, priceSnapshot.pricing, time.Now().UTC())
	priceSnapshot.close()
	if err != nil {
		log.WithError(err).Warn("usage: live usage record has invalid request-time pricing data")
		return false
	}
	detail.PricingReceipt = receipt
	detail.costMicro = cost.Micro
	detail.Cost = float64(cost.Micro) / costMicroPerUSD
	detail.CostStatus = cost.Status
	detail.CostReason = cost.Reason
	detail.costTrusted = true

	w.submitMu.RLock()
	defer w.submitMu.RUnlock()
	if w.stopped.Load() {
		log.Warn("usage: writer stopped, live usage record was not accepted")
		return false
	}
	select {
	case w.ch <- detail:
		w.eventAccepted.Add(1)
		return true
	case <-w.stop:
		log.Warn("usage: writer stopped, live usage record was not accepted")
		return false
	}
}

// SubmitRequestLogArtifact applies backpressure until the lifecycle update is
// accepted by the same writer goroutine that owns event writes.
func (w *Writer) SubmitRequestLogArtifact(artifact logging.RequestLogArtifact) bool {
	if w == nil {
		return false
	}
	artifact = logging.PrepareRequestLogArtifact(artifact)
	if err := logging.ValidateRequestLogArtifact(artifact); err != nil {
		log.WithError(err).Warn("usage: rejected invalid request log artifact")
		return false
	}

	w.submitMu.RLock()
	defer w.submitMu.RUnlock()
	if w.stopped.Load() {
		log.Warn("usage: writer stopped, request log artifact was not accepted")
		return false
	}
	select {
	case w.artifactCh <- artifact:
		w.artifactAccepted.Add(1)
		return true
	case <-w.stop:
		log.Warn("usage: writer stopped, request log artifact was not accepted")
		return false
	}
}

// stats returns accepted, committed, and permanently rejected writer counts.
func (w *Writer) stats() writerStats {
	if w == nil {
		return writerStats{}
	}
	return writerStats{
		EventAccepted:     w.eventAccepted.Load(),
		EventCommitted:    w.eventCommitted.Load(),
		EventRejected:     w.eventRejected.Load(),
		ArtifactAccepted:  w.artifactAccepted.Load(),
		ArtifactCommitted: w.artifactCommitted.Load(),
		ArtifactRejected:  w.artifactRejected.Load(),
	}
}

func (w *Writer) loop(ctx context.Context) {
	defer close(w.done)

	eventCh := w.ch
	artifactCh := w.artifactCh
	eventBatch := make([]FlatDetail, 0, writerBatchSize)
	artifactBatch := make([]logging.RequestLogArtifact, 0, writerBatchSize)
	var priceSnapshot *writerPriceSnapshot
	timer := time.NewTimer(writerFlushInterval)
	defer timer.Stop()

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(writerFlushInterval)
	}
	releasePriceSnapshot := func() {
		priceSnapshot.close()
		priceSnapshot = nil
	}
	flushEvents := func(timeout time.Duration) bool {
		if len(eventBatch) == 0 {
			return true
		}
		if priceSnapshot == nil && w.store != nil && w.store.database() != nil {
			priceSnapshot = w.acquirePriceSnapshot()
		}
		var pricing pricingSnapshot
		if priceSnapshot != nil {
			pricing = priceSnapshot.pricing
		}
		attempted := len(eventBatch)
		result := w.flushEventBatchWithPricing(eventBatch, timeout, pricing)
		releasePriceSnapshot()
		if result.committed > 0 {
			w.eventCommitted.Add(uint64(result.committed))
		}
		if result.rejected > 0 {
			w.eventRejected.Add(uint64(result.rejected))
			log.Errorf(
				"usage: writer permanently rejected %d of %d accepted records: %v",
				result.rejected,
				attempted,
				result.err,
			)
		}
		eventBatch = result.retry
		if len(eventBatch) > 0 {
			log.Warnf("usage: writer flush will retry (%d records): %v", len(eventBatch), result.err)
			return false
		}
		return true
	}
	flushArtifacts := func(timeout time.Duration) bool {
		if len(artifactBatch) == 0 {
			return true
		}
		err := w.flushRequestLogArtifacts(artifactBatch, timeout)
		if err == nil {
			w.artifactCommitted.Add(uint64(len(artifactBatch)))
			artifactBatch = artifactBatch[:0]
			return true
		}
		if isTransientWriterError(err) {
			log.Warnf("usage: request log artifact flush will retry (%d records): %v", len(artifactBatch), err)
			return false
		}
		w.artifactRejected.Add(uint64(len(artifactBatch)))
		log.Errorf(
			"usage: writer permanently rejected request log artifact updates (%d records): %v",
			len(artifactBatch),
			err,
		)
		artifactBatch = artifactBatch[:0]
		return true
	}
	flushAll := func(timeout time.Duration) bool {
		eventsFlushed := flushEvents(timeout)
		artifactsFlushed := flushArtifacts(timeout)
		return eventsFlushed && artifactsFlushed
	}
	drainAvailable := func() {
		for eventCh != nil || artifactCh != nil {
			select {
			case detail, ok := <-eventCh:
				if !ok {
					eventCh = nil
					continue
				}
				eventBatch = append(eventBatch, detail)
			case artifact, ok := <-artifactCh:
				if !ok {
					artifactCh = nil
					continue
				}
				artifactBatch = append(artifactBatch, artifact)
			default:
				return
			}
		}
	}
	flushBeforeExit := func() {
		drainAvailable()
		for !flushAll(writerShutdownFlushTimeout) {
			time.Sleep(writerFlushInterval)
		}
		releasePriceSnapshot()
	}

	ctxDone := ctx.Done()
	retryPending := false
	for {
		if retryPending {
			if w.stopped.Load() {
				flushBeforeExit()
				return
			}
			select {
			case <-ctxDone:
				w.requestStop()
				ctxDone = nil
			case <-timer.C:
				retryPending = !flushAll(writerFlushTimeout)
				timer.Reset(writerFlushInterval)
			}
			continue
		}
		if eventCh == nil && artifactCh == nil {
			flushBeforeExit()
			return
		}

		select {
		case <-ctxDone:
			w.requestStop()
			ctxDone = nil
		case detail, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			eventBatch = append(eventBatch, detail)
			if len(eventBatch) >= writerBatchSize {
				retryPending = !flushAll(writerFlushTimeout)
				resetTimer()
			}
		case artifact, ok := <-artifactCh:
			if !ok {
				artifactCh = nil
				continue
			}
			artifactBatch = append(artifactBatch, artifact)
			if len(artifactBatch) >= writerBatchSize {
				retryPending = !flushAll(writerFlushTimeout)
				resetTimer()
			}
		case <-timer.C:
			retryPending = !flushAll(writerFlushTimeout)
			timer.Reset(writerFlushInterval)
		}
	}
}

const (
	insertEventSQL = `
INSERT OR IGNORE INTO events (
    fingerprint, ts_ns, model_id, api_key_id, source_id, auth_index_id, credential_id, provider_id,
    failed, input_tokens, output_tokens, reasoning_tokens, cached_tokens,
    cache_read_tokens, cache_write_tokens, total_tokens,
    cost_micro, cost_status, cost_reason, pricing_receipt, metadata
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

	upsertHourBucketSQL = `
INSERT INTO hour_bucket (
    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
    requests, success, failure,
    input_tokens, output_tokens, reasoning_tokens, cached_tokens,
    cache_read_tokens, cache_write_tokens, total_tokens, cost_micro,
    complete_requests, partial_requests, unpriced_requests,
    complete_tokens, partial_tokens, unpriced_tokens,
    latency_ms_total, latency_samples
) VALUES (
    ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0), ifnull(provider_id, 0))
DO UPDATE SET
    requests         = hour_bucket.requests         + excluded.requests,
    success          = hour_bucket.success          + excluded.success,
    failure          = hour_bucket.failure          + excluded.failure,
    input_tokens     = hour_bucket.input_tokens     + excluded.input_tokens,
    output_tokens    = hour_bucket.output_tokens    + excluded.output_tokens,
    reasoning_tokens = hour_bucket.reasoning_tokens + excluded.reasoning_tokens,
    cached_tokens     = hour_bucket.cached_tokens     + excluded.cached_tokens,
    cache_read_tokens = hour_bucket.cache_read_tokens + excluded.cache_read_tokens,
    cache_write_tokens = hour_bucket.cache_write_tokens + excluded.cache_write_tokens,
    total_tokens      = hour_bucket.total_tokens      + excluded.total_tokens,
    cost_micro        = hour_bucket.cost_micro        + excluded.cost_micro,
    complete_requests = hour_bucket.complete_requests + excluded.complete_requests,
    partial_requests  = hour_bucket.partial_requests  + excluded.partial_requests,
    unpriced_requests = hour_bucket.unpriced_requests + excluded.unpriced_requests,
    complete_tokens   = hour_bucket.complete_tokens   + excluded.complete_tokens,
    partial_tokens    = hour_bucket.partial_tokens    + excluded.partial_tokens,
    unpriced_tokens   = hour_bucket.unpriced_tokens   + excluded.unpriced_tokens,
    latency_ms_total  = hour_bucket.latency_ms_total  + excluded.latency_ms_total,
    latency_samples   = hour_bucket.latency_samples   + excluded.latency_samples
WHERE hour_bucket.requests         <= 9223372036854775807 - excluded.requests
  AND hour_bucket.success          <= 9223372036854775807 - excluded.success
  AND hour_bucket.failure          <= 9223372036854775807 - excluded.failure
  AND hour_bucket.input_tokens     <= 9223372036854775807 - excluded.input_tokens
  AND hour_bucket.output_tokens    <= 9223372036854775807 - excluded.output_tokens
  AND hour_bucket.reasoning_tokens <= 9223372036854775807 - excluded.reasoning_tokens
  AND hour_bucket.cached_tokens    <= 9223372036854775807 - excluded.cached_tokens
  AND hour_bucket.cache_read_tokens <= 9223372036854775807 - excluded.cache_read_tokens
  AND hour_bucket.cache_write_tokens <= 9223372036854775807 - excluded.cache_write_tokens
  AND hour_bucket.total_tokens      <= 9223372036854775807 - excluded.total_tokens
  AND hour_bucket.cost_micro        <= 9223372036854775807 - excluded.cost_micro
  AND hour_bucket.complete_requests <= 9223372036854775807 - excluded.complete_requests
  AND hour_bucket.partial_requests  <= 9223372036854775807 - excluded.partial_requests
  AND hour_bucket.unpriced_requests <= 9223372036854775807 - excluded.unpriced_requests
  AND hour_bucket.complete_tokens   <= 9223372036854775807 - excluded.complete_tokens
  AND hour_bucket.partial_tokens    <= 9223372036854775807 - excluded.partial_tokens
  AND hour_bucket.unpriced_tokens   <= 9223372036854775807 - excluded.unpriced_tokens
  AND hour_bucket.latency_ms_total  <= 9223372036854775807 - excluded.latency_ms_total
  AND hour_bucket.latency_samples  <= 9223372036854775807 - excluded.latency_samples
`
)

func (w *Writer) flush(batch []FlatDetail) error {
	return w.flushWithTimeout(batch, writerFlushTimeout)
}

func (w *Writer) flushWithTimeout(batch []FlatDetail, timeout time.Duration) error {
	if len(batch) == 0 {
		return nil
	}
	if w.store == nil || w.store.database() == nil {
		return errors.New("usage: writer store is unavailable")
	}
	priceSnapshot := w.acquirePriceSnapshot()
	defer priceSnapshot.close()
	return w.flushWithPricing(batch, timeout, priceSnapshot.pricing)
}

func (w *Writer) flushWithPricing(batch []FlatDetail, timeout time.Duration, pricing pricingSnapshot) error {
	return w.flushEventBatchWithPricing(batch, timeout, pricing).err
}

func (w *Writer) flushEventBatchWithPricing(
	batch []FlatDetail,
	timeout time.Duration,
	pricing pricingSnapshot,
) eventFlushResult {
	if len(batch) == 0 {
		return eventFlushResult{}
	}
	if w.store == nil || w.store.database() == nil {
		return eventFlushResult{
			rejected: len(batch),
			err:      errors.New("usage: writer store is unavailable"),
		}
	}

	flushTransaction := func(items []FlatDetail) error {
		if w.storageCapacityReached() {
			return errWriterStorageCap
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return w.flushTransactionWithPricing(ctx, items, pricing)
	}
	warmCaches := func(items []FlatDetail) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := w.store.warmDimensionCaches(ctx, items); err != nil {
			log.Warnf("usage: warm dimension caches after writer commit: %v", err)
		}
	}

	batchErr := flushTransaction(batch)
	if batchErr == nil {
		_ = w.storageCapacityReached()
		warmCaches(batch)
		return eventFlushResult{committed: len(batch)}
	}
	if isTransientWriterError(batchErr) {
		return eventFlushResult{
			retry: batch,
			err:   fmt.Errorf("%w: %v", errWriterFlushTransient, batchErr),
		}
	}
	if errors.Is(batchErr, errWriterStorageCap) {
		return eventFlushResult{rejected: len(batch), err: batchErr}
	}
	if len(batch) == 1 {
		return eventFlushResult{rejected: 1, err: batchErr}
	}

	result := eventFlushResult{}
	recordErrs := make([]error, 0, len(batch)+1)
	recordErrs = append(recordErrs, batchErr)
	for i := range batch {
		err := flushTransaction(batch[i : i+1])
		if err == nil {
			result.committed++
			warmCaches(batch[i : i+1])
			continue
		}
		if isTransientWriterError(err) {
			result.retry = batch[i:]
			recordErrs = append(recordErrs, fmt.Errorf("event[%d]: %w", i, err))
			result.err = fmt.Errorf(
				"%w: %w",
				errWriterFlushTransient,
				errors.Join(recordErrs...),
			)
			return result
		}
		result.rejected++
		recordErrs = append(recordErrs, fmt.Errorf("event[%d]: %w", i, err))
	}
	if result.rejected == 0 {
		log.Warnf(
			"usage: batch transaction failed but all %d records were recovered individually: %v",
			len(batch),
			batchErr,
		)
		return result
	}
	result.err = fmt.Errorf(
		"usage: batch transaction failed; %d of %d records were rejected: %w",
		result.rejected,
		len(batch),
		errors.Join(recordErrs...),
	)
	return result
}

func isTransientWriterError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if sqliteErr, ok := errors.AsType[interface {
		error
		Code() int
	}](err); ok {
		switch sqliteErr.Code() & 0xff {
		case 5, 6:
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked")
}

func (w *Writer) flushRequestLogArtifacts(
	batch []logging.RequestLogArtifact,
	timeout time.Duration,
) error {
	if len(batch) == 0 {
		return nil
	}
	if w.store == nil || w.store.database() == nil {
		return errors.New("usage: writer store is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tx, err := w.store.database().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage: begin request log artifact tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for i := range batch {
		if err = persistRequestLogArtifact(ctx, tx, batch[i]); err != nil {
			return fmt.Errorf("usage: persist request log artifact[%d]: %w", i, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit request log artifact tx: %w", err)
	}
	committed = true
	return nil
}

func (w *Writer) flushTransactionWithPricing(
	ctx context.Context,
	batch []FlatDetail,
	pricing pricingSnapshot,
) error {
	tx, err := w.store.database().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for i := range batch {
		if _, err = persistEventWithPricing(ctx, tx, w.store, &batch[i], pricing); err != nil {
			return fmt.Errorf("usage: persist event[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit tx: %w", err)
	}
	committed = true
	return nil
}

// persistEvent is the shared write path used by both the live Writer and the
// synchronous Import path. It returns added=true when INSERT OR IGNORE
// actually inserted a new row, false when the fingerprint was already known.
//
// hour_bucket is only touched when added=true so re-importing the same file
// can never double-count.
func persistEvent(ctx context.Context, tx *sql.Tx, store *Store, d *FlatDetail, priceFn PriceFunc) (bool, error) {
	return persistEventWithPricing(ctx, tx, store, d, pricingSnapshot{priceFn: priceFn})
}

func persistEventWithPricing(
	ctx context.Context,
	tx *sql.Tx,
	store *Store,
	d *FlatDetail,
	pricing pricingSnapshot,
) (bool, error) {
	if store == nil {
		return false, errors.New("usage: persistEvent on nil store")
	}
	tokens, err := validatedTokenStats(d.Tokens)
	if err != nil {
		return false, err
	}
	tsNs, hourNs, dayNs, err := unixNanoBucketStarts(d.Timestamp)
	if err != nil {
		return false, fmt.Errorf("usage: invalid event timestamp: %w", err)
	}
	d.Tokens = tokens
	d.APIKey = coreusage.EnsureAPIKeyIdentity(d.APIKey)

	modelID, err := store.resolveModelID(ctx, tx, d.Model)
	if err != nil {
		return false, err
	}
	apiKeyID, err := store.resolveAPIKeyID(ctx, tx, d.APIKey)
	if err != nil {
		return false, err
	}
	sourceID, err := store.resolveSourceID(ctx, tx, d.Source)
	if err != nil {
		return false, err
	}
	authIdxID, err := store.resolveAuthIndexID(ctx, tx, d.AuthIndex)
	if err != nil {
		return false, err
	}
	credID, err := store.resolveCredentialID(ctx, tx, d.Source, d.AuthIndex)
	if err != nil {
		return false, err
	}
	providerID, err := store.resolveProviderID(ctx, tx, d.Provider)
	if err != nil {
		return false, err
	}
	tokenBreakdown, err := validEventTokenBreakdown(d)
	if err != nil {
		return false, err
	}

	failedInt := int64(0)
	if d.Failed {
		failedInt = 1
	}
	successInt := int64(1) - failedInt
	cost := costResult{Micro: d.costMicro, Status: d.CostStatus, Reason: strings.TrimSpace(d.CostReason)}
	if d.costTrusted {
		if cost.Micro < 0 {
			return false, errors.New("usage: cost_micro must be non-negative")
		}
		cost = normalizeTrustedCost(cost)
	} else {
		cost, err = computeCost(tokens, tokenBreakdown, pricing, d.Model, d.Timestamp)
		if err != nil {
			return false, err
		}
	}
	d.Cost = float64(cost.Micro) / costMicroPerUSD
	d.CostStatus = cost.Status
	d.CostReason = cost.Reason
	pricingReceipt, err := encodePricingReceipt(d.PricingReceipt)
	if err != nil {
		return false, err
	}
	metadata, err := encodeEventMetadata(d)
	if err != nil {
		return false, err
	}
	coverage := coverageBucketValues(cost.Status, tokens.TotalTokens)
	latencyMs, latencySamples := latencyBucketValues(d.LatencyMs)

	fp := computeFingerprint(*d)
	if d.fingerprintTrusted {
		fp = d.fingerprint
	}
	res, err := tx.ExecContext(
		ctx, insertEventSQL,
		fp[:],
		tsNs,
		modelID,
		nullableID(apiKeyID),
		nullableID(sourceID),
		nullableID(authIdxID),
		nullableID(credID),
		nullableID(providerID),
		failedInt,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.CacheReadTokens,
		tokens.CacheWriteTokens,
		tokens.TotalTokens,
		cost.Micro,
		cost.Status,
		cost.Reason,
		pricingReceipt,
		metadata,
	)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	if artifact, ok := requestLogArtifactFromDetail(d); ok {
		if err = persistRequestLogArtifact(ctx, tx, artifact); err != nil {
			return false, err
		}
	}
	if affected == 0 {
		return false, nil
	}

	bucketResult, err := tx.ExecContext(
		ctx, upsertHourBucketSQL,
		hourNs,
		modelID,
		nullableID(credID),
		nullableID(apiKeyID),
		nullableID(providerID),
		successInt, failedInt,
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens, tokens.CachedTokens,
		tokens.CacheReadTokens, tokens.CacheWriteTokens, tokens.TotalTokens,
		cost.Micro,
		coverage.completeRequests, coverage.partialRequests, coverage.unpricedRequests,
		coverage.completeTokens, coverage.partialTokens, coverage.unpricedTokens,
		latencyMs, latencySamples,
	)
	if err != nil {
		return true, fmt.Errorf("upsert hour_bucket: %w", err)
	}
	bucketAffected, err := bucketResult.RowsAffected()
	if err != nil {
		return true, fmt.Errorf("upsert hour_bucket rows affected: %w", err)
	}
	if bucketAffected != 1 {
		return true, errors.New("upsert hour_bucket rejected integer overflow")
	}

	if _, err := tx.ExecContext(
		ctx,
		"INSERT OR IGNORE INTO dirty_day(day_ts_ns) VALUES (?)",
		dayNs,
	); err != nil {
		return true, fmt.Errorf("mark dirty day: %w", err)
	}
	if err = settleAPIKeyReservationEvent(
		ctx,
		tx,
		d.ReservationID,
		fp[:],
		tokens.TotalTokens,
		cost,
		d.Failed,
	); err != nil {
		return true, err
	}
	return true, nil
}

// nullableID returns nil when the wrapper is invalid so database/sql writes
// SQL NULL rather than the zero int64.
func nullableID(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

func latencyBucketValues(latencyMs int64) (int64, int64) {
	if latencyMs <= 0 {
		return 0, 0
	}
	return latencyMs, 1
}

type costCoverageBucketValues struct {
	completeRequests int64
	partialRequests  int64
	unpricedRequests int64
	completeTokens   int64
	partialTokens    int64
	unpricedTokens   int64
}

func coverageBucketValues(status CostStatus, totalTokens int64) costCoverageBucketValues {
	switch status {
	case costStatusComplete:
		return costCoverageBucketValues{completeRequests: 1, completeTokens: totalTokens}
	case costStatusPartial:
		return costCoverageBucketValues{partialRequests: 1, partialTokens: totalTokens}
	default:
		return costCoverageBucketValues{unpricedRequests: 1, unpricedTokens: totalTokens}
	}
}

type costResult struct {
	Micro  int64
	Status CostStatus
	Reason string
}

// computeCostMicro keeps the legacy PriceFunc contract for imports and tests.
func computeCostMicro(tokens tokenStats, priceFn PriceFunc, model string) (int64, error) {
	result, err := computeCost(tokens, nil, pricingSnapshot{priceFn: priceFn}, model, time.Time{})
	if err != nil {
		return 0, err
	}
	return result.Micro, nil
}

// computeCost applies reference prices to mutually exclusive token buckets.
// Prices use USD per one million tokens, so token count multiplied by price is
// cost_micro. Cost status is retained as diagnostic metadata only.
func computeCost(
	tokens tokenStats,
	breakdown *coreusage.TokenBreakdown,
	pricing pricingSnapshot,
	model string,
	at time.Time,
) (costResult, error) {
	if breakdown != nil {
		if !breakdown.Valid() {
			return costResult{}, errors.New("usage: invalid token breakdown")
		}
		if breakdown.Quality == coreusage.TokenAccountingQualityInconsistent {
			return costResult{
				Status: CostStatusUnpriced,
				Reason: "token accounting is inconsistent",
			}, nil
		}
	}
	price, found := lookupDetailedPrice(pricing, model, at)
	if !found {
		return costResult{Status: CostStatusUnpriced, Reason: "model price is not configured"}, nil
	}
	if !price.Effective {
		return costResult{Status: CostStatusUnpriced, Reason: "price is not effective at the event timestamp"}, nil
	}
	switch price.Status {
	case costStatusComplete, costStatusPartial, CostStatusUnpriced:
	default:
		return costResult{}, fmt.Errorf("usage: invalid price status %q for model %q", price.Status, model)
	}
	if !hasReferenceTokenRates(price) {
		reason := strings.TrimSpace(price.Reason)
		if reason == "" {
			reason = "model reference price is not configured"
		}
		return costResult{Status: CostStatusUnpriced, Reason: reason}, nil
	}

	for _, item := range []struct {
		name  string
		price float64
	}{
		{name: "prompt", price: price.Prompt},
		{name: "completion", price: price.Completion},
	} {
		if err := validateCostRate(item.name, item.price, model); err != nil {
			return costResult{}, err
		}
	}
	if price.CacheRead != nil {
		if err := validateCostRate("cache_read", *price.CacheRead, model); err != nil {
			return costResult{}, err
		}
	}
	if price.CacheWrite != nil {
		if err := validateCostRate("cache_write", *price.CacheWrite, model); err != nil {
			return costResult{}, err
		}
	}

	uncachedInput := tokens.InputTokens
	completionTokens := tokens.OutputTokens
	status := price.Status
	reasons := make([]string, 0, 4)
	cacheReadTokens := tokens.CacheReadTokens
	cacheWriteTokens := tokens.CacheWriteTokens
	if breakdown != nil {
		uncachedInput = breakdown.Input.UncachedTokens
		completionTokens = breakdown.Output.TotalTokens
		cacheReadTokens = breakdown.Input.CacheReadTokens
		cacheWriteTokens = breakdown.Input.CacheWriteTokens
		if breakdown.Quality == coreusage.TokenAccountingQualityUnclassified {
			status = mostConservativeCostStatus(status, costStatusPartial)
			reasons = append(reasons, "token accounting contains unclassified tokens")
		}
	} else {
		if cacheReadTokens == 0 && cacheWriteTokens == 0 && tokens.CachedTokens > 0 {
			cacheReadTokens = tokens.CachedTokens
		}
		cacheTokens, ok := coreusage.AddNonNegativeInt64(cacheReadTokens, cacheWriteTokens)
		if !ok {
			return costResult{}, fmt.Errorf("usage: cached token detail total overflows int64 for model %q", model)
		}
		if cacheTokens > 0 {
			status = mostConservativeCostStatus(status, costStatusPartial)
			if cacheTokens > tokens.InputTokens {
				reasons = append(reasons, "cached input tokens were recorded independently")
			} else {
				uncachedInput -= cacheTokens
				reasons = append(reasons, "legacy cache overlap is ambiguous; applied subset assumption")
			}
		}
	}
	raw := float64(uncachedInput)*price.Prompt + float64(completionTokens)*price.Completion
	if price.Reason != "" {
		reasons = append(reasons, price.Reason)
	}
	if cacheReadTokens > 0 {
		cacheReadRate := price.Prompt
		if price.CacheRead != nil {
			cacheReadRate = *price.CacheRead
		} else {
			reasons = append(reasons, "cache-read reference price uses prompt rate")
		}
		raw += float64(cacheReadTokens) * cacheReadRate
	}
	if cacheWriteTokens > 0 {
		cacheWriteRate := price.Prompt
		if price.CacheWrite != nil {
			cacheWriteRate = *price.CacheWrite
		} else {
			reasons = append(reasons, "cache-write reference price uses prompt rate")
		}
		raw += float64(cacheWriteTokens) * cacheWriteRate
	}
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw >= math.Exp2(63) {
		return costResult{}, fmt.Errorf("usage: cost_micro out of int64 range for model %q", model)
	}
	return costResult{Micro: int64(raw), Status: status, Reason: strings.Join(reasons, "; ")}, nil
}

func mostConservativeCostStatus(left, right CostStatus) CostStatus {
	if left == CostStatusUnpriced || right == CostStatusUnpriced {
		return CostStatusUnpriced
	}
	if left == costStatusPartial || right == costStatusPartial {
		return costStatusPartial
	}
	return costStatusComplete
}

func hasReferenceTokenRates(price detailedPrice) bool {
	return price.Prompt != 0 || price.Completion != 0 ||
		price.CacheRead != nil || price.CacheWrite != nil ||
		price.CacheWrite5m != nil || price.CacheWrite1h != nil
}

func lookupDetailedPrice(pricing pricingSnapshot, model string, at time.Time) (detailedPrice, bool) {
	if pricing.detailedPriceFn != nil {
		return pricing.detailedPriceFn(model, at)
	}
	if pricing.priceFn == nil {
		return detailedPrice{}, false
	}
	prompt, completion, cache, ok := pricing.priceFn(model)
	if !ok {
		return detailedPrice{}, false
	}
	return detailedPrice{
		Prompt: prompt, Completion: completion, CacheRead: &cache,
		Status: costStatusComplete, Automatic: true, Effective: true,
	}, true
}

func validateCostRate(name string, price float64, model string) error {
	if math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
		return fmt.Errorf("usage: invalid %s price for model %q", name, model)
	}
	return nil
}

func normalizeTrustedCost(cost costResult) costResult {
	switch cost.Status {
	case costStatusComplete, costStatusPartial, CostStatusUnpriced:
		return cost
	}
	if cost.Micro > 0 {
		return costResult{
			Micro:  cost.Micro,
			Status: costStatusPartial,
			Reason: "legacy backup did not preserve component price status",
		}
	}
	return costResult{Status: CostStatusUnpriced, Reason: "legacy backup has no price status"}
}
