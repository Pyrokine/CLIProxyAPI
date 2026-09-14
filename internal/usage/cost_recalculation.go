package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	costRecalculationTaskMetaKey       = "cost_recalculation_task_v1"
	costRecalculationMaxRetries        = 12
	costRecalculationInitialRetryDelay = 100 * time.Millisecond
	costRecalculationMaximumRetryDelay = 2 * time.Second
)

type costRecalculationTask struct {
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	Phase            string  `json:"phase"`
	PriceRevision    string  `json:"price_revision,omitempty"`
	MaxEventID       int64   `json:"max_event_id"`
	EventCount       int64   `json:"event_count"`
	CursorEventID    int64   `json:"cursor_event_id"`
	ProcessedEvents  int64   `json:"processed_events"`
	ChangedEvents    int64   `json:"changed_events"`
	RollupDays       []int64 `json:"rollup_days,omitempty"`
	RollupDoneDays   int     `json:"rollup_done_days"`
	RecalculatedDays int     `json:"recalculated_days,omitempty"`
	TotalCost        float64 `json:"total_cost,omitempty"`
	LastError        string  `json:"last_error,omitempty"`
	StartedAt        string  `json:"started_at"`
	UpdatedAt        string  `json:"updated_at"`
	CompletedAt      string  `json:"completed_at,omitempty"`
	Resumed          bool    `json:"resumed,omitempty"`
	RestartReason    string  `json:"restart_reason,omitempty"`
}

type costTaskExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type costTaskQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readCostRecalculationTask(ctx context.Context, queryer costTaskQueryer) (costRecalculationTask, bool, error) {
	var raw string
	err := queryer.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", costRecalculationTaskMetaKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return costRecalculationTask{}, false, nil
	}
	if err != nil {
		return costRecalculationTask{}, false, fmt.Errorf("read cost recalculation task: %w", err)
	}
	var task costRecalculationTask
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		return costRecalculationTask{}, false, fmt.Errorf("decode cost recalculation task: %w", err)
	}
	return task, true, nil
}

func writeCostRecalculationTask(ctx context.Context, execer costTaskExecer, task costRecalculationTask) error {
	encoded, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("encode cost recalculation task: %w", err)
	}
	if _, err := execer.ExecContext(
		ctx,
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		costRecalculationTaskMetaKey,
		string(encoded),
	); err != nil {
		return fmt.Errorf("write cost recalculation task: %w", err)
	}
	return nil
}

func costTaskNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func (p *Persister) pricingSnapshotForRecalculation() pricingSnapshot {
	p.pricingMu.RLock()
	defer p.pricingMu.RUnlock()
	provider, fn := p.priceSource()
	if provider == nil {
		return pricingSnapshot{priceFn: fn}
	}
	if revisionProvider, ok := provider.(detailedPriceRevisionProvider); ok {
		detailed, revision := revisionProvider.DetailedSnapshotWithRevision()
		return pricingSnapshot{detailedPriceFn: detailed, revision: revision}
	}
	if detailedProvider, ok := provider.(detailedPriceProvider); ok {
		return pricingSnapshot{detailedPriceFn: detailedProvider.DetailedSnapshot()}
	}
	return pricingSnapshot{priceFn: provider.Snapshot()}
}

func (p *Persister) prepareCostRecalculationTask(
	ctx context.Context,
	s *Store,
	pricing pricingSnapshot,
) (costRecalculationTask, error) {
	if pricing.priceFn == nil && pricing.detailedPriceFn == nil {
		return costRecalculationTask{}, errors.New("usage: no pricing function configured")
	}

	existing, exists, err := readCostRecalculationTask(ctx, s.db)
	if err != nil {
		return costRecalculationTask{}, err
	}
	if exists && (existing.Status == "interrupted" || existing.Status == "error") &&
		pricing.revision != "" && existing.PriceRevision == pricing.revision {
		var count int64
		if err := s.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM events WHERE id <= ?",
			existing.MaxEventID,
		).Scan(&count); err != nil {
			return costRecalculationTask{}, fmt.Errorf("validate cost recalculation boundary: %w", err)
		}
		if count == existing.EventCount {
			existing.Status = "running"
			if existing.Phase == "" {
				existing.Phase = "repricing"
			}
			existing.LastError = ""
			existing.Resumed = true
			existing.UpdatedAt = costTaskNow()
			if err := writeCostRecalculationTask(ctx, s.db, existing); err != nil {
				return costRecalculationTask{}, err
			}
			return existing, nil
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return costRecalculationTask{}, fmt.Errorf("begin cost recalculation task: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var maxID sql.NullInt64
	var count int64
	if err := tx.QueryRowContext(ctx, "SELECT MAX(id), COUNT(*) FROM events").Scan(&maxID, &count); err != nil {
		return costRecalculationTask{}, fmt.Errorf("read cost recalculation boundary: %w", err)
	}
	now := costTaskNow()
	task := costRecalculationTask{
		ID:            fmt.Sprintf("%s-%d", now, count),
		Status:        "running",
		Phase:         "repricing",
		PriceRevision: pricing.revision,
		EventCount:    count,
		StartedAt:     now,
		UpdatedAt:     now,
	}
	if maxID.Valid {
		task.MaxEventID = maxID.Int64
	}
	if exists && existing.ID != "" {
		task.RestartReason = "previous task could not be resumed"
	}
	if err := writeCostRecalculationTask(ctx, tx, task); err != nil {
		return costRecalculationTask{}, err
	}
	if err := tx.Commit(); err != nil {
		return costRecalculationTask{}, fmt.Errorf("commit cost recalculation task: %w", err)
	}
	committed = true
	return task, nil
}

func ensureCostRecalculationBuckets(ctx context.Context, s *Store) error {
	var events, bucketRequests int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		return fmt.Errorf("count events before cost recalculation: %w", err)
	}
	if err := s.db.QueryRowContext(
		ctx, "SELECT COALESCE(SUM(requests), 0) FROM hour_bucket",
	).Scan(&bucketRequests); err != nil {
		return fmt.Errorf("count hour bucket requests before cost recalculation: %w", err)
	}
	if events == bucketRequests {
		return nil
	}

	// Older databases and direct import paths can contain events without matching
	// aggregates. This repair path is intentionally exceptional; normal
	// recalculation only applies bounded deltas to existing buckets.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin missing usage bucket repair: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := rebuildUsageBucketsTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit missing usage bucket repair: %w", err)
	}
	committed = true
	return nil
}

func appendCostRollupDay(days []int64, day int64) []int64 {
	if slices.Contains(days, day) {
		return days
	}
	return append(days, day)
}

func appendDirtyCostRollupDays(ctx context.Context, s *Store, task *costRecalculationTask) error {
	rows, err := s.db.QueryContext(ctx, "SELECT day_ts_ns FROM dirty_day ORDER BY day_ts_ns")
	if err != nil {
		return fmt.Errorf("read dirty days for cost recalculation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var day int64
		if err := rows.Scan(&day); err != nil {
			return fmt.Errorf("scan dirty day for cost recalculation: %w", err)
		}
		task.RollupDays = appendCostRollupDay(task.RollupDays, day)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate dirty days for cost recalculation: %w", err)
	}
	return nil
}

func costRecalculationStatusFromTask(task costRecalculationTask) CostRecalculationStatus {
	status := CostRecalculationStatus{
		Status:             task.Status,
		Running:            task.Status == "running" || task.Status == "rolling_up",
		TaskID:             task.ID,
		Phase:              task.Phase,
		PriceRevision:      task.PriceRevision,
		ProcessedEvents:    task.ProcessedEvents,
		TotalEvents:        task.EventCount,
		ChangedEvents:      task.ChangedEvents,
		RollupTotalDays:    len(task.RollupDays),
		RollupDoneDays:     task.RollupDoneDays,
		DirtyDaysRemaining: len(task.RollupDays) - task.RollupDoneDays,
		RecalculatedDays:   task.RecalculatedDays,
		TotalCost:          task.TotalCost,
		Error:              task.LastError,
		StartedAt:          task.StartedAt,
		UpdatedAt:          task.UpdatedAt,
		Resumable:          task.Status == "interrupted" || task.Status == "error",
		Resumed:            task.Resumed,
		RestartReason:      task.RestartReason,
	}
	if status.DirtyDaysRemaining < 0 {
		status.DirtyDaysRemaining = 0
	}
	switch task.Status {
	case "ok":
		status.Percent = 100
	case "rolling_up":
		if len(task.RollupDays) == 0 {
			status.Percent = 100
		} else {
			status.Percent = 95 + float64(task.RollupDoneDays)*5/float64(len(task.RollupDays))
		}
	default:
		if task.EventCount == 0 {
			status.Percent = 95
		} else {
			status.Percent = float64(task.ProcessedEvents) * 95 / float64(task.EventCount)
		}
	}
	if status.Percent < 0 {
		status.Percent = 0
	}
	if status.Percent > 100 {
		status.Percent = 100
	}
	return status
}

func (p *Persister) publishCostRecalculationTask(task costRecalculationTask) {
	p.costRecalcMu.Lock()
	status := costRecalculationStatusFromTask(task)
	if p.costRecalcRunning.Load() && !status.Running {
		status.Status = "running"
		status.Running = true
	}
	p.costRecalcStatus = status
	p.costRecalcMu.Unlock()
}

type costRepriceEvent struct {
	id                                 int64
	tsNs                               int64
	modelID                            int64
	credentialID, apiKeyID, providerID sql.NullInt64
	model                              string
	tokens                             tokenStats
	metadata                           sql.NullString
	pricingReceipt                     *PricingReceipt
	current                            costResult
	updated                            costResult
}

type costBucketKey struct {
	bucketNs                           int64
	modelID                            int64
	credentialID, apiKeyID, providerID sql.NullInt64
}

type costBucketDelta struct {
	costMicro                                           int64
	completeRequests, partialRequests, unpricedRequests int64
	completeTokens, partialTokens, unpricedTokens       int64
}

func checkedAddInt64(left, right int64) (int64, bool) {
	result := left + right
	if (right > 0 && result < left) || (right < 0 && result > left) {
		return 0, false
	}
	return result, true
}

func addCostBucketDelta(target *costBucketDelta, value costBucketDelta) error {
	var ok bool
	for _, field := range []struct {
		current *int64
		delta   int64
	}{
		{&target.costMicro, value.costMicro},
		{&target.completeRequests, value.completeRequests},
		{&target.partialRequests, value.partialRequests},
		{&target.unpricedRequests, value.unpricedRequests},
		{&target.completeTokens, value.completeTokens},
		{&target.partialTokens, value.partialTokens},
		{&target.unpricedTokens, value.unpricedTokens},
	} {
		*field.current, ok = checkedAddInt64(*field.current, field.delta)
		if !ok {
			return errors.New("usage: cost recalculation bucket delta overflows int64")
		}
	}
	return nil
}

func costCoverageDelta(current, updated costResult, tokens int64) costBucketDelta {
	before := coverageBucketValues(current.Status, tokens)
	after := coverageBucketValues(updated.Status, tokens)
	return costBucketDelta{
		costMicro:        updated.Micro - current.Micro,
		completeRequests: after.completeRequests - before.completeRequests,
		partialRequests:  after.partialRequests - before.partialRequests,
		unpricedRequests: after.unpricedRequests - before.unpricedRequests,
		completeTokens:   after.completeTokens - before.completeTokens,
		partialTokens:    after.partialTokens - before.partialTokens,
		unpricedTokens:   after.unpricedTokens - before.unpricedTokens,
	}
}

func applyCostBucketDelta(ctx context.Context, tx *sql.Tx, key costBucketKey, delta costBucketDelta) error {
	const selectSQL = `SELECT cost_micro,
	       complete_requests, partial_requests, unpriced_requests,
	       complete_tokens, partial_tokens, unpriced_tokens
	  FROM hour_bucket
	 WHERE bucket_ts_ns = ? AND model_id = ?
	   AND credential_id IS ? AND api_key_id IS ? AND provider_id IS ?`
	values := []any{
		key.bucketNs, key.modelID, nullableID(key.credentialID), nullableID(key.apiKeyID), nullableID(key.providerID),
	}
	var current costBucketDelta
	if err := tx.QueryRowContext(ctx, selectSQL, values...).Scan(
		&current.costMicro,
		&current.completeRequests, &current.partialRequests, &current.unpricedRequests,
		&current.completeTokens, &current.partialTokens, &current.unpricedTokens,
	); err != nil {
		return fmt.Errorf("read hour bucket for cost recalculation: %w", err)
	}
	if err := addCostBucketDelta(&current, delta); err != nil {
		return err
	}
	if current.costMicro < 0 || current.completeRequests < 0 || current.partialRequests < 0 ||
		current.unpricedRequests < 0 || current.completeTokens < 0 || current.partialTokens < 0 || current.unpricedTokens < 0 {
		return errors.New("usage: cost recalculation would make hour bucket negative")
	}
	updateArgs := []any{
		current.costMicro,
		current.completeRequests, current.partialRequests, current.unpricedRequests,
		current.completeTokens, current.partialTokens, current.unpricedTokens,
	}
	updateArgs = append(updateArgs, values...)
	result, err := tx.ExecContext(
		ctx,
		`UPDATE hour_bucket
		    SET cost_micro = ?,
		        complete_requests = ?, partial_requests = ?, unpriced_requests = ?,
		        complete_tokens = ?, partial_tokens = ?, unpriced_tokens = ?
		  WHERE bucket_ts_ns = ? AND model_id = ?
		    AND credential_id IS ? AND api_key_id IS ? AND provider_id IS ?`,
		updateArgs...,
	)
	if err != nil {
		return fmt.Errorf("update hour bucket for cost recalculation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cost recalculation hour bucket updates: %w", err)
	}
	if affected != 1 {
		return errors.New("usage: cost recalculation hour bucket disappeared")
	}
	return nil
}

func (p *Persister) repriceCostRecalculationPage(
	ctx context.Context,
	s *Store,
	pricing pricingSnapshot,
	task *costRecalculationTask,
) (bool, error) {
	nextTask := *task
	nextTask.RollupDays = append([]int64(nil), task.RollupDays...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin cost recalculation page: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(
		ctx,
		`SELECT e.id, e.ts_ns, e.model_id, e.credential_id, e.api_key_id, e.provider_id, m.name,
		        e.input_tokens, e.output_tokens, e.reasoning_tokens, e.cached_tokens,
		        e.cache_read_tokens, e.cache_write_tokens, e.total_tokens,
		        e.metadata, e.pricing_receipt,
		        e.cost_micro, e.cost_status, e.cost_reason
		   FROM events e
		   JOIN dim_model m ON m.id = e.model_id
		  WHERE e.id > ? AND e.id <= ?
		  ORDER BY e.id
		  LIMIT ?`,
		task.CursorEventID,
		task.MaxEventID,
		repriceEventPageSize,
	)
	if err != nil {
		return false, fmt.Errorf("query cost recalculation page: %w", err)
	}
	batch := make([]costRepriceEvent, 0, repriceEventPageSize)
	for rows.Next() {
		var event costRepriceEvent
		var pricingReceipt sql.NullString
		if err := rows.Scan(
			&event.id, &event.tsNs, &event.modelID, &event.credentialID, &event.apiKeyID, &event.providerID,
			&event.model,
			&event.tokens.InputTokens, &event.tokens.OutputTokens, &event.tokens.ReasoningTokens,
			&event.tokens.CachedTokens,
			&event.tokens.CacheReadTokens, &event.tokens.CacheWriteTokens, &event.tokens.TotalTokens,
			&event.metadata, &pricingReceipt,
			&event.current.Micro, &event.current.Status, &event.current.Reason,
		); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan cost recalculation page: %w", err)
		}
		event.tokens, err = validatedTokenStats(event.tokens)
		if err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("validate cost recalculation event %d: %w", event.id, err)
		}
		event.pricingReceipt, err = decodePricingReceipt(pricingReceipt)
		if err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("decode pricing receipt for event %d: %w", event.id, err)
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
				event.updated, err = computeCost(event.tokens, breakdown, pricing, event.model, eventTime)
			}
		}
		if err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("recalculate event %d: %w", event.id, err)
		}
		batch = append(batch, event)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate cost recalculation page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close cost recalculation page: %w", err)
	}
	if len(batch) == 0 {
		return false, nil
	}

	update, err := tx.PrepareContext(
		ctx,
		"UPDATE events SET cost_micro = ?, cost_status = ?, cost_reason = ? WHERE id = ?",
	)
	if err != nil {
		return false, fmt.Errorf("prepare cost recalculation events: %w", err)
	}
	defer func() { _ = update.Close() }()

	buckets := make(map[costBucketKey]costBucketDelta)
	changedDays := make(map[int64]struct{})
	changed := int64(0)
	for _, event := range batch {
		if event.current == event.updated {
			continue
		}
		if _, err := update.ExecContext(
			ctx, event.updated.Micro, event.updated.Status, event.updated.Reason, event.id,
		); err != nil {
			return false, fmt.Errorf("update cost recalculation event %d: %w", event.id, err)
		}
		changed++
		if event.current.Micro == event.updated.Micro && event.current.Status == event.updated.Status {
			continue
		}
		hourNs, err := floorUnixNano(event.tsNs, nsPerHour)
		if err != nil {
			return false, fmt.Errorf("calculate cost recalculation hour bucket: %w", err)
		}
		dayNs, err := floorUnixNano(event.tsNs, nsPerDay)
		if err != nil {
			return false, fmt.Errorf("calculate cost recalculation day bucket: %w", err)
		}
		key := costBucketKey{
			bucketNs:     hourNs,
			modelID:      event.modelID,
			credentialID: event.credentialID,
			apiKeyID:     event.apiKeyID,
			providerID:   event.providerID,
		}
		delta := buckets[key]
		if err := addCostBucketDelta(
			&delta, costCoverageDelta(event.current, event.updated, event.tokens.TotalTokens),
		); err != nil {
			return false, err
		}
		buckets[key] = delta
		changedDays[dayNs] = struct{}{}
	}
	for key, delta := range buckets {
		if err := applyCostBucketDelta(ctx, tx, key, delta); err != nil {
			return false, err
		}
	}
	for day := range changedDays {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO dirty_day(day_ts_ns) VALUES (?)", day); err != nil {
			return false, fmt.Errorf("mark cost recalculation dirty day: %w", err)
		}
		nextTask.RollupDays = appendCostRollupDay(nextTask.RollupDays, day)
	}
	nextTask.CursorEventID = batch[len(batch)-1].id
	nextTask.ProcessedEvents += int64(len(batch))
	nextTask.ChangedEvents += changed
	nextTask.UpdatedAt = costTaskNow()
	if err := writeCostRecalculationTask(ctx, tx, nextTask); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit cost recalculation page: %w", err)
	}
	committed = true
	*task = nextTask
	return true, nil
}

func retryCostRecalculationOperation(ctx context.Context, operation string, run func() error) error {
	for retry := 0; ; retry++ {
		err := run()
		if err == nil {
			return nil
		}
		if !isTransientWriterError(err) || ctx.Err() != nil || retry >= costRecalculationMaxRetries {
			return err
		}

		delay := costRecalculationInitialRetryDelay
		for backoff := 0; backoff < retry && delay < costRecalculationMaximumRetryDelay; backoff++ {
			delay *= 2
		}
		if delay > costRecalculationMaximumRetryDelay {
			delay = costRecalculationMaximumRetryDelay
		}
		log.Warnf(
			"usage: retry %s after transient SQLite error (%d/%d): %v",
			operation,
			retry+1,
			costRecalculationMaxRetries,
			err,
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *Persister) repriceCostRecalculationPageWithRetry(
	ctx context.Context,
	s *Store,
	pricing pricingSnapshot,
	task *costRecalculationTask,
) (bool, error) {
	processed := false
	err := retryCostRecalculationOperation(
		ctx, "cost recalculation page", func() error {
			var err error
			processed, err = p.repriceCostRecalculationPage(ctx, s, pricing, task)
			return err
		},
	)
	return processed, err
}

func (p *Persister) recalculateCostsIncrementalContext(parent context.Context) (
	result RecalculateCostsResult,
	returnErr error,
) {
	ctx, cancel := p.maintenanceContext(parent, 0)
	defer cancel()
	unlockMaintenance, err := p.acquireMaintenance(ctx)
	if err != nil {
		return result, fmt.Errorf("wait for usage maintenance: %w", err)
	}
	defer unlockMaintenance()

	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return result, errors.New("usage: store not initialized")
	}
	if err := retryCostRecalculationOperation(
		ctx, "usage bucket validation", func() error {
			return ensureCostRecalculationBuckets(ctx, s)
		},
	); err != nil {
		return result, err
	}
	pricing := p.pricingSnapshotForRecalculation()
	var task costRecalculationTask
	if err := retryCostRecalculationOperation(
		ctx, "cost recalculation task preparation", func() error {
			var err error
			task, err = p.prepareCostRecalculationTask(ctx, s, pricing)
			return err
		},
	); err != nil {
		return result, err
	}
	result.TaskID = task.ID
	p.publishCostRecalculationTask(task)
	defer func() {
		if returnErr == nil {
			return
		}
		if errors.Is(returnErr, context.Canceled) {
			task.Status = "interrupted"
			task.LastError = ""
		} else {
			task.Status = "error"
			task.LastError = returnErr.Error()
		}
		task.UpdatedAt = costTaskNow()
		if err := retryCostRecalculationOperation(
			context.Background(), "interrupted cost recalculation task", func() error {
				return writeCostRecalculationTask(context.Background(), s.db, task)
			},
		); err != nil {
			log.Warnf("usage: persist interrupted cost recalculation task: %v", err)
		}
		p.publishCostRecalculationTask(task)
	}()

	pages := 0
	for task.Phase == "repricing" {
		processed, err := p.repriceCostRecalculationPageWithRetry(ctx, s, pricing, &task)
		if err != nil {
			return result, err
		}
		if !processed {
			if err := appendDirtyCostRollupDays(ctx, s, &task); err != nil {
				return result, err
			}
			task.Status = "rolling_up"
			task.Phase = "day_rollup"
			task.UpdatedAt = costTaskNow()
			if err := retryCostRecalculationOperation(
				ctx, "cost recalculation day-rollup transition", func() error {
					return writeCostRecalculationTask(ctx, s.db, task)
				},
			); err != nil {
				return result, err
			}
			p.publishCostRecalculationTask(task)
			break
		}
		pages++
		p.publishCostRecalculationTask(task)
		if pages%32 == 0 {
			if err := p.checkpointContext(ctx, "PASSIVE"); err != nil {
				log.Warnf("usage: checkpoint during cost recalculation: %v", err)
			}
		}
	}

	for task.RollupDoneDays < len(task.RollupDays) {
		day := task.RollupDays[task.RollupDoneDays]
		if err := retryCostRecalculationOperation(
			ctx, "recalculated day rollup", func() error {
				return s.rollupDay(ctx, day)
			},
		); err != nil {
			return result, fmt.Errorf("roll up recalculated day: %w", err)
		}
		task.RollupDoneDays++
		task.UpdatedAt = costTaskNow()
		if err := retryCostRecalculationOperation(
			ctx, "cost recalculation day-rollup progress", func() error {
				return writeCostRecalculationTask(ctx, s.db, task)
			},
		); err != nil {
			return result, err
		}
		p.publishCostRecalculationTask(task)
	}

	var totalMicro int64
	if err := s.db.QueryRowContext(
		ctx, "SELECT COALESCE(SUM(cost_micro), 0) FROM hour_bucket",
	).Scan(&totalMicro); err != nil {
		return result, fmt.Errorf("total cost after recalculation: %w", err)
	}
	var days int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT bucket_ts_ns) FROM day_bucket").Scan(&days); err != nil {
		return result, fmt.Errorf("count recalculated days: %w", err)
	}
	task.Status = "ok"
	task.Phase = "complete"
	task.RecalculatedDays = days
	task.TotalCost = float64(totalMicro) / costMicroPerUSD
	task.LastError = ""
	task.UpdatedAt = costTaskNow()
	task.CompletedAt = task.UpdatedAt
	if err := retryCostRecalculationOperation(
		ctx, "completed cost recalculation task", func() error {
			return writeCostRecalculationTask(ctx, s.db, task)
		},
	); err != nil {
		return result, err
	}
	p.publishCostRecalculationTask(task)
	if err := p.checkpointContext(ctx, "TRUNCATE"); err != nil {
		log.Warnf("usage: checkpoint after cost recalculation: %v", err)
	}
	return RecalculateCostsResult{
		TaskID:           task.ID,
		RecalculatedDays: task.RecalculatedDays,
		TotalCost:        task.TotalCost,
	}, nil
}

func (p *Persister) restoreCostRecalculation(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.RLock()
	s := p.store
	p.mu.RUnlock()
	if s == nil || s.db == nil {
		return
	}
	task, exists, err := readCostRecalculationTask(ctx, s.db)
	if err != nil {
		log.Warnf("usage: read cost recalculation task at startup: %v", err)
		return
	}
	if !exists {
		return
	}
	if task.Status == "running" || task.Status == "rolling_up" {
		task.Status = "interrupted"
		task.UpdatedAt = costTaskNow()
		if err := writeCostRecalculationTask(ctx, s.db, task); err != nil {
			log.Warnf("usage: persist interrupted cost recalculation task: %v", err)
			return
		}
	}
	p.publishCostRecalculationTask(task)
	if task.Status != "interrupted" && task.Status != "error" {
		return
	}
	if !p.beginCostRecalculation(false) {
		return
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
}
