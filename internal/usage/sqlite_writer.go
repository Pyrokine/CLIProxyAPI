// Last compiled: 2026-04-28
// Author: pyro

package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	writerChanCap       = 4096
	writerBatchSize     = 100
	writerFlushInterval = 100 * time.Millisecond
	writerFlushTimeout  = 10 * time.Second
)

// Writer ingests FlatDetail records and persists them in batched transactions
// against a Store. A single goroutine owns all SQL writes so we never collide
// with SQLite's "one writer at a time" rule.
type Writer struct {
	store   *Store
	priceFn PriceFunc

	ch       chan FlatDetail
	done     chan struct{}
	started  atomic.Bool
	stopOnce sync.Once

	priceMu sync.RWMutex // guards priceFn during runtime swaps

	dropped atomic.Int64
}

// NewWriter wires a writer to a store. priceFn may be nil — events still land
// on disk, just with cost_micro = 0 until pricing is configured.
func NewWriter(store *Store, priceFn PriceFunc) *Writer {
	return &Writer{
		store:   store,
		priceFn: priceFn,
		ch:      make(chan FlatDetail, writerChanCap),
		done:    make(chan struct{}),
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
	w.stopOnce.Do(
		func() {
			close(w.ch)
			<-w.done
		},
	)
}

// SetPriceFunc lets callers swap pricing at runtime (e.g. after a model
// catalog refresh) without restarting the writer.
func (w *Writer) SetPriceFunc(fn PriceFunc) {
	if w == nil {
		return
	}
	w.priceMu.Lock()
	w.priceFn = fn
	w.priceMu.Unlock()
}

func (w *Writer) priceFunc() PriceFunc {
	w.priceMu.RLock()
	defer w.priceMu.RUnlock()
	return w.priceFn
}

// Submit enqueues a record for persistence. Non-blocking: when the channel is
// saturated the record is counted (and warning-logged with throttling) rather
// than back-pressuring the request path. Usage tracking is observability — a
// few dropped records is preferable to slowing production traffic.
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
	select {
	case w.ch <- detail:
	default:
		n := w.dropped.Add(1)
		if n == 1 || n%1000 == 0 {
			log.Warnf("usage: writer queue full, dropped %d records cumulatively", n)
		}
	}
}

// Dropped exposes the lifetime drop count for diagnostics.
func (w *Writer) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

func (w *Writer) loop(ctx context.Context) {
	defer close(w.done)

	batch := make([]FlatDetail, 0, writerBatchSize)
	timer := time.NewTimer(writerFlushInterval)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.flush(batch); err != nil {
			log.Errorf("usage: writer flush failed (%d records): %v", len(batch), err)
		}
		batch = batch[:0]
	}

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(writerFlushInterval)
	}

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is still buffered before exiting so a normal
			// shutdown doesn't lose records sitting in the channel.
			for {
				select {
				case d, ok := <-w.ch:
					if !ok {
						flush()
						return
					}
					batch = append(batch, d)
				default:
					flush()
					return
				}
			}
		case d, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, d)
			if len(batch) >= writerBatchSize {
				flush()
				resetTimer()
			}
		case <-timer.C:
			flush()
			timer.Reset(writerFlushInterval)
		}
	}
}

const (
	insertEventSQL = `
INSERT OR IGNORE INTO events (
    fingerprint, ts_ns, model_id, api_key_id, source_id, auth_index_id, credential_id, provider_id,
    failed, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_micro
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

	upsertHourBucketSQL = `
INSERT INTO hour_bucket (
    bucket_ts_ns, model_id, credential_id, api_key_id, provider_id,
    requests, success, failure,
    input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_micro
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket_ts_ns, model_id, ifnull(credential_id, 0), ifnull(api_key_id, 0), ifnull(provider_id, 0))
DO UPDATE SET
    requests         = hour_bucket.requests         + excluded.requests,
    success          = hour_bucket.success          + excluded.success,
    failure          = hour_bucket.failure          + excluded.failure,
    input_tokens     = hour_bucket.input_tokens     + excluded.input_tokens,
    output_tokens    = hour_bucket.output_tokens    + excluded.output_tokens,
    reasoning_tokens = hour_bucket.reasoning_tokens + excluded.reasoning_tokens,
    cached_tokens    = hour_bucket.cached_tokens    + excluded.cached_tokens,
    total_tokens     = hour_bucket.total_tokens     + excluded.total_tokens,
    cost_micro       = hour_bucket.cost_micro       + excluded.cost_micro
`
)

func (w *Writer) flush(batch []FlatDetail) error {
	if len(batch) == 0 || w.store == nil {
		return nil
	}
	db := w.store.DB()
	if db == nil {
		return errors.New("usage: writer store has nil DB")
	}

	ctx, cancel := context.WithTimeout(context.Background(), writerFlushTimeout)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	priceFn := w.priceFunc()
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
	tokens := normaliseTokenStats(d.Tokens)
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
	costMicro := computeCostMicro(tokens, priceFn, d.Model)

	fp := ComputeFingerprint(*d)
	res, err := tx.ExecContext(
		ctx, insertEventSQL,
		fp[:],
		d.Timestamp.UTC().UnixNano(),
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

	bucketTsNs := d.Timestamp.UTC().Truncate(time.Hour).UnixNano()
	if _, err := tx.ExecContext(
		ctx, upsertHourBucketSQL,
		bucketTsNs,
		modelID,
		nullableID(credID),
		nullableID(apiKeyID),
		nullableID(providerID),
		successInt, failedInt,
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens, tokens.CachedTokens, tokens.TotalTokens,
		costMicro,
	); err != nil {
		return true, fmt.Errorf("upsert hour_bucket: %w", err)
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

// computeCostMicro turns token counts and per-1M-token prices into cost_micro
// (USD × 1e6 INTEGER). The 1e6 factors cancel: cost_USD = tokens*price/1e6,
// cost_micro = cost_USD * 1e6, leaving cost_micro = tokens * price directly.
// Returns 0 when no price is registered.
func computeCostMicro(tokens tokenStats, priceFn PriceFunc, model string) int64 {
	if priceFn == nil {
		return 0
	}
	prompt, completion, cache, ok := priceFn(model)
	if !ok {
		return 0
	}
	raw := float64(tokens.InputTokens)*prompt +
		float64(tokens.OutputTokens)*completion +
		float64(tokens.CachedTokens)*cache
	if raw < 0 {
		return 0
	}
	return int64(raw)
}
