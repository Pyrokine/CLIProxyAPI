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

	log "github.com/sirupsen/logrus"
)

const (
	writerChanCap              = 4096
	writerBatchSize            = 100
	writerFlushInterval        = 100 * time.Millisecond
	writerFlushTimeout         = 10 * time.Second
	writerShutdownFlushTimeout = 2 * time.Second
	writerShutdownRetryLimit   = 3
)

var errWriterFlushTransient = errors.New("usage: transient writer flush failure")

// Writer ingests FlatDetail records and persists them in batched transactions
// against a Store. A single goroutine owns all SQL writes so we never collide
// with SQLite's "one writer at a time" rule.
type Writer struct {
	store         *Store
	priceFn       PriceFunc
	priceProvider PriceProvider
	pricingMu     *sync.RWMutex

	ch       chan FlatDetail
	done     chan struct{}
	stop     chan struct{}
	started  atomic.Bool
	stopOnce sync.Once
	submitMu sync.RWMutex
	stopped  atomic.Bool

	priceMu sync.RWMutex // guards priceFn during runtime swaps
}

type writerPriceSnapshot struct {
	priceFn PriceFunc
	release func()
}

func (s *writerPriceSnapshot) close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

// NewWriter wires a writer to a store. priceFn may be nil — events still land
// on disk, just with cost_micro = 0 until pricing is configured.
func NewWriter(store *Store, priceFn PriceFunc) *Writer {
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
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
	}
}

func (w *Writer) acquirePriceSnapshot() *writerPriceSnapshot {
	if w.pricingMu != nil {
		w.pricingMu.RLock()
	}
	w.priceMu.RLock()
	priceFn := w.priceFn
	if w.priceProvider != nil {
		priceFn = w.priceProvider.Snapshot()
	}
	return &writerPriceSnapshot{
		priceFn: priceFn,
		release: func() {
			w.priceMu.RUnlock()
			if w.pricingMu != nil {
				w.pricingMu.RUnlock()
			}
		},
	}
}

// Start launches the writer goroutine. Calling Start more than once is a no-op
// so callers don't need their own guard.
func (w *Writer) Start(ctx context.Context) {
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
	w.stopOnce.Do(func() {
		w.stopped.Store(true)
		close(w.stop)
		w.submitMu.Lock()
		close(w.ch)
		w.submitMu.Unlock()
	})
}

// SetPriceFunc lets callers swap pricing at runtime (e.g. after a model
// catalog refresh) without restarting the writer.
func (w *Writer) SetPriceFunc(fn PriceFunc) {
	if w == nil {
		return
	}
	w.priceMu.Lock()
	w.priceFn = fn
	w.priceProvider = nil
	w.priceMu.Unlock()
}

func (w *Writer) SetPriceProvider(provider PriceProvider) {
	if w == nil {
		return
	}
	w.priceMu.Lock()
	w.priceFn = nil
	w.priceProvider = provider
	w.priceMu.Unlock()
}

// Submit enqueues a record for persistence. It blocks when the channel is full
// so accepted live usage records are not discarded under burst load.
func (w *Writer) Submit(detail FlatDetail) {
	if w == nil {
		return
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

	w.submitMu.RLock()
	defer w.submitMu.RUnlock()
	if w.stopped.Load() {
		log.Warn("usage: writer stopped, live usage record was not accepted")
		return
	}
	select {
	case w.ch <- detail:
	case <-w.stop:
		log.Warn("usage: writer stopped, live usage record was not accepted")
	}
}

func (w *Writer) loop(ctx context.Context) {
	defer close(w.done)

	batch := make([]FlatDetail, 0, writerBatchSize)
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
	flush := func(timeout time.Duration) bool {
		if len(batch) == 0 {
			return true
		}
		if priceSnapshot == nil && w.store != nil && w.store.DB() != nil {
			priceSnapshot = w.acquirePriceSnapshot()
		}
		var priceFn PriceFunc
		if priceSnapshot != nil {
			priceFn = priceSnapshot.priceFn
		}
		err := w.flushWithPrice(batch, timeout, priceFn)
		releasePriceSnapshot()
		if err == nil {
			batch = batch[:0]
			return true
		}
		if errors.Is(err, errWriterFlushTransient) {
			log.Warnf("usage: writer flush will retry (%d records): %v", len(batch), err)
			return false
		}
		log.Errorf("usage: writer permanently rejected records (%d records): %v", len(batch), err)
		batch = batch[:0]
		return true
	}
	drainAvailable := func() {
		for {
			select {
			case detail, ok := <-w.ch:
				if !ok {
					return
				}
				batch = append(batch, detail)
			default:
				return
			}
		}
	}
	flushBeforeExit := func() {
		drainAvailable()
		for attempt := 1; attempt <= writerShutdownRetryLimit; attempt++ {
			if flush(writerShutdownFlushTimeout) {
				return
			}
			if attempt < writerShutdownRetryLimit {
				time.Sleep(writerFlushInterval)
			}
		}
		if len(batch) > 0 {
			log.Errorf(
				"usage: writer shutdown dropped %d accepted records after %d transient failures",
				len(batch),
				writerShutdownRetryLimit,
			)
			releasePriceSnapshot()
			batch = batch[:0]
		}
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
				retryPending = !flush(writerFlushTimeout)
				timer.Reset(writerFlushInterval)
			}
			continue
		}

		select {
		case <-ctxDone:
			w.requestStop()
			ctxDone = nil
		case detail, ok := <-w.ch:
			if !ok {
				flushBeforeExit()
				return
			}
			batch = append(batch, detail)
			if len(batch) >= writerBatchSize {
				retryPending = !flush(writerFlushTimeout)
				resetTimer()
			}
		case <-timer.C:
			retryPending = !flush(writerFlushTimeout)
			timer.Reset(writerFlushInterval)
		}
	}
}

const (
	insertEventSQL = `
INSERT OR IGNORE INTO events (
    fingerprint, ts_ns, model_id, api_key_id, source_id, auth_index_id, credential_id, provider_id,
    failed, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_micro, metadata
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

	upsertHourBucketSQL = `
INSERT INTO hour_bucket (
    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
    requests, success, failure,
    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_micro,
    latency_ms_total, latency_samples
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0), ifnull(provider_id, 0))
DO UPDATE SET
    requests         = hour_bucket.requests         + excluded.requests,
    success          = hour_bucket.success          + excluded.success,
    failure          = hour_bucket.failure          + excluded.failure,
    input_tokens     = hour_bucket.input_tokens     + excluded.input_tokens,
    output_tokens    = hour_bucket.output_tokens    + excluded.output_tokens,
    reasoning_tokens = hour_bucket.reasoning_tokens + excluded.reasoning_tokens,
    cached_tokens     = hour_bucket.cached_tokens     + excluded.cached_tokens,
    total_tokens      = hour_bucket.total_tokens      + excluded.total_tokens,
    cost_micro        = hour_bucket.cost_micro        + excluded.cost_micro,
    latency_ms_total  = hour_bucket.latency_ms_total  + excluded.latency_ms_total,
    latency_samples   = hour_bucket.latency_samples   + excluded.latency_samples
WHERE hour_bucket.requests         <= 9223372036854775807 - excluded.requests
  AND hour_bucket.success          <= 9223372036854775807 - excluded.success
  AND hour_bucket.failure          <= 9223372036854775807 - excluded.failure
  AND hour_bucket.input_tokens     <= 9223372036854775807 - excluded.input_tokens
  AND hour_bucket.output_tokens    <= 9223372036854775807 - excluded.output_tokens
  AND hour_bucket.reasoning_tokens <= 9223372036854775807 - excluded.reasoning_tokens
  AND hour_bucket.cached_tokens    <= 9223372036854775807 - excluded.cached_tokens
  AND hour_bucket.total_tokens     <= 9223372036854775807 - excluded.total_tokens
  AND hour_bucket.cost_micro       <= 9223372036854775807 - excluded.cost_micro
  AND hour_bucket.latency_ms_total <= 9223372036854775807 - excluded.latency_ms_total
  AND hour_bucket.latency_samples  <= 9223372036854775807 - excluded.latency_samples
`
)

func (w *Writer) flush(batch []FlatDetail) error {
	return w.flushWithTimeout(batch, writerFlushTimeout)
}

func (w *Writer) flushWithTimeout(batch []FlatDetail, timeout time.Duration) error {
	if len(batch) == 0 || w.store == nil {
		return nil
	}
	if w.store.DB() == nil {
		return errors.New("usage: writer store has nil DB")
	}
	priceSnapshot := w.acquirePriceSnapshot()
	defer priceSnapshot.close()
	return w.flushWithPrice(batch, timeout, priceSnapshot.priceFn)
}

func (w *Writer) flushWithPrice(batch []FlatDetail, timeout time.Duration, priceFn PriceFunc) error {
	if len(batch) == 0 || w.store == nil {
		return nil
	}
	if w.store.DB() == nil {
		return errors.New("usage: writer store has nil DB")
	}

	flushTransaction := func(items []FlatDetail) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return w.flushTransaction(ctx, items, priceFn)
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
		warmCaches(batch)
		return nil
	}
	if isTransientWriterError(batchErr) {
		return fmt.Errorf("%w: %v", errWriterFlushTransient, batchErr)
	}
	if len(batch) == 1 {
		return batchErr
	}

	var recordErrs []error
	for i := range batch {
		err := flushTransaction(batch[i : i+1])
		if err == nil {
			warmCaches(batch[i : i+1])
			continue
		}
		if isTransientWriterError(err) {
			return fmt.Errorf("%w: event[%d]: %v", errWriterFlushTransient, i, err)
		}
		recordErrs = append(recordErrs, fmt.Errorf("event[%d]: %w", i, err))
	}
	if len(recordErrs) == 0 {
		log.Warnf(
			"usage: batch transaction failed but all %d records were recovered individually: %v",
			len(batch),
			batchErr,
		)
		return nil
	}
	return fmt.Errorf(
		"usage: batch transaction failed; %d of %d records were rejected: %w",
		len(recordErrs),
		len(batch),
		errors.Join(append([]error{batchErr}, recordErrs...)...),
	)
}

func isTransientWriterError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
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

func (w *Writer) flushTransaction(ctx context.Context, batch []FlatDetail, priceFn PriceFunc) error {
	tx, err := w.store.DB().BeginTx(ctx, nil)
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
		if err := w.persistOne(ctx, tx, &batch[i], priceFn); err != nil {
			return fmt.Errorf("usage: persist event[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit tx: %w", err)
	}
	committed = true
	return nil
}

// persistOne resolves dimensions, performs INSERT OR IGNORE on the events
// table, and only when a row was actually inserted does it accumulate into
// hour_bucket. The RowsAffected guard is what makes import idempotent.
func (w *Writer) persistOne(ctx context.Context, tx *sql.Tx, d *FlatDetail, priceFn PriceFunc) error {
	_, err := persistEvent(ctx, tx, w.store, d, priceFn)
	return err
}

// persistEvent is the shared write path used by both the live Writer and the
// synchronous Import path. It returns added=true when INSERT OR IGNORE
// actually inserted a new row, false when the fingerprint was already known.
//
// hour_bucket is only touched when added=true so re-importing the same file
// can never double-count.
func persistEvent(ctx context.Context, tx *sql.Tx, store *Store, d *FlatDetail, priceFn PriceFunc) (bool, error) {
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

	modelID, err := store.ResolveModelID(ctx, tx, d.Model)
	if err != nil {
		return false, err
	}
	apiKeyID, err := store.ResolveAPIKeyID(ctx, tx, d.APIKey)
	if err != nil {
		return false, err
	}
	sourceID, err := store.ResolveSourceID(ctx, tx, d.Source)
	if err != nil {
		return false, err
	}
	authIdxID, err := store.ResolveAuthIndexID(ctx, tx, d.AuthIndex)
	if err != nil {
		return false, err
	}
	credID, err := store.ResolveCredentialID(ctx, tx, d.Source, d.AuthIndex)
	if err != nil {
		return false, err
	}
	providerID, err := store.ResolveProviderID(ctx, tx, d.Provider)
	if err != nil {
		return false, err
	}

	failedInt := int64(0)
	if d.Failed {
		failedInt = 1
	}
	successInt := int64(1) - failedInt
	costMicro := d.costMicro
	if d.costTrusted {
		if costMicro < 0 {
			return false, errors.New("usage: cost_micro must be non-negative")
		}
	} else {
		costMicro, err = computeCostMicro(tokens, priceFn, d.Model)
		if err != nil {
			return false, err
		}
	}
	metadata, err := encodeEventMetadata(d)
	if err != nil {
		return false, err
	}
	latencyMs, latencySamples := latencyBucketValues(d.LatencyMs)

	fp := ComputeFingerprint(*d)
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
		tokens.TotalTokens,
		costMicro,
		metadata,
	)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
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
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens, tokens.CachedTokens, tokens.TotalTokens,
		costMicro,
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

// computeCostMicro turns token counts and per-1M-token prices into cost_micro
// (USD × 1e6 INTEGER). The 1e6 factors cancel: cost_USD = tokens*price/1e6,
// cost_micro = cost_USD * 1e6, leaving cost_micro = tokens * price directly.
// Returns 0 when no price is registered.
func computeCostMicro(tokens tokenStats, priceFn PriceFunc, model string) (int64, error) {
	if priceFn == nil {
		return 0, nil
	}
	prompt, completion, cache, ok := priceFn(model)
	if !ok {
		return 0, nil
	}
	for _, item := range []struct {
		name  string
		price float64
	}{
		{name: "prompt", price: prompt},
		{name: "completion", price: completion},
		{name: "cache", price: cache},
	} {
		if math.IsNaN(item.price) || math.IsInf(item.price, 0) || item.price < 0 {
			return 0, fmt.Errorf("usage: invalid %s price for model %q", item.name, model)
		}
	}
	raw := float64(tokens.InputTokens)*prompt +
		float64(tokens.OutputTokens)*completion +
		float64(tokens.CachedTokens)*cache
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw >= math.Exp2(63) {
		return 0, fmt.Errorf("usage: cost_micro out of int64 range for model %q", model)
	}
	return int64(raw), nil
}
