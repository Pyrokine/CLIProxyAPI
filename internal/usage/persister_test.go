package usage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	log "github.com/sirupsen/logrus"
)

type scriptedPriceProvider struct {
	mu        sync.Mutex
	snapshots []PriceFunc
	calls     int
}

func (p *scriptedPriceProvider) Snapshot() PriceFunc {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.snapshots) == 0 {
		return nil
	}
	index := p.calls - 1
	if index >= len(p.snapshots) {
		index = len(p.snapshots) - 1
	}
	return p.snapshots[index]
}

func (p *scriptedPriceProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestAtomicWrite_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-file.json")
	data := []byte(`{"test": true}`)

	if err := atomicWrite(path, data); err != nil {
		t.Fatalf("atomicWrite failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("expected file permissions 0600, got %o", perm)
	}

	// Verify content is correct.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(content) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", string(content), string(data))
	}
}

func TestAtomicWrite_DirectoryPermissions(t *testing.T) {
	base := t.TempDir()
	subDir := filepath.Join(base, "sub", "dir")
	path := filepath.Join(subDir, "test.json")

	if err := atomicWrite(path, []byte("data")); err != nil {
		t.Fatalf("atomicWrite failed: %v", err)
	}

	info, err := os.Stat(subDir)
	if err != nil {
		t.Fatalf("stat dir failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o700 {
		t.Fatalf("expected directory permissions 0700, got %o", perm)
	}
}

func TestAtomicWrite_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.json")
	var wg sync.WaitGroup

	goroutines := 20
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			data := []byte(`{"index":` + string(rune('0'+idx%10)) + `}`)
			_ = atomicWrite(path, data)
		}(i)
	}
	wg.Wait()

	// File should exist and be valid (not corrupted).
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read failed after concurrent writes: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("file should not be empty after concurrent writes")
	}
}

func TestBuildModelPriceFunc_ReturnsNilWithoutPriceFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if got := BuildModelPriceFunc(configPath); got != nil {
		t.Fatal("expected nil PriceFunc when model-prices.json is missing")
	}
}

func TestBuildModelPriceFunc_LoadsValidPriceFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if _, err := modelcatalog.ReplacePrices(
		configPath, map[string]modelcatalog.Price{
			"claude-opus-4-6": {Prompt: 5, Completion: 25, Cache: 0.5},
		},
	); err != nil {
		t.Fatalf("write prices: %v", err)
	}
	priceFn := BuildModelPriceFunc(configPath)
	if priceFn == nil {
		t.Fatal("expected non-nil PriceFunc when model-prices.json exists")
	}
	prompt, completion, cache, found := priceFn("claude-opus-4-6")
	if !found {
		t.Fatal("expected claude-opus-4-6 pricing to resolve")
	}
	if prompt != 5 || completion != 25 || cache != 0.5 {
		t.Fatalf("unexpected prices prompt=%v completion=%v cache=%v", prompt, completion, cache)
	}
}

func TestModelPriceProviderKeepsOperationSnapshotsImmutable(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if _, err := modelcatalog.ReplacePrices(
		configPath, map[string]modelcatalog.Price{
			"model": {Prompt: 1, Completion: 2, Cache: 3},
		},
	); err != nil {
		t.Fatalf("write initial prices: %v", err)
	}
	provider, ok := BuildModelPriceProvider(configPath).(*modelPriceProvider)
	if !ok || provider == nil {
		t.Fatal("expected model price provider")
	}
	oldSnapshot := provider.Snapshot()

	if _, err := modelcatalog.ReplacePrices(
		configPath, map[string]modelcatalog.Price{
			"model": {Prompt: 10, Completion: 20, Cache: 30},
		},
	); err != nil {
		t.Fatalf("write replacement prices: %v", err)
	}
	provider.mu.Lock()
	provider.loadedAt = time.Time{}
	provider.mu.Unlock()
	newSnapshot := provider.Snapshot()

	oldPrompt, oldCompletion, oldCache, oldFound := oldSnapshot("model")
	if !oldFound || oldPrompt != 1 || oldCompletion != 2 || oldCache != 3 {
		t.Fatalf(
			"old snapshot = (%v, %v, %v, %v), want (1, 2, 3, true)",
			oldPrompt,
			oldCompletion,
			oldCache,
			oldFound,
		)
	}
	newPrompt, newCompletion, newCache, newFound := newSnapshot("model")
	if !newFound || newPrompt != 10 || newCompletion != 20 || newCache != 30 {
		t.Fatalf(
			"new snapshot = (%v, %v, %v, %v), want (10, 20, 30, true)",
			newPrompt,
			newCompletion,
			newCache,
			newFound,
		)
	}
}

func TestPersisterImportUsesOnePriceSnapshotPerOperation(t *testing.T) {
	store := openTestStore(t)
	priceFor := func(prompt float64) PriceFunc {
		return func(string) (float64, float64, float64, bool) {
			return prompt, 0, 0, true
		}
	}
	provider := &scriptedPriceProvider{snapshots: []PriceFunc{priceFor(1), priceFor(2)}}
	persister := &Persister{store: store, priceProvider: provider}

	payloads := [][]byte{
		[]byte(`[
			{"timestamp":"2026-04-27T10:00:00Z","model":"m1","tokens":{"input_tokens":1,"total_tokens":1}},
			{"timestamp":"2026-04-27T10:01:00Z","model":"m2","tokens":{"input_tokens":1,"total_tokens":1}}
		]`),
		[]byte(`[
			{"timestamp":"2026-04-27T11:00:00Z","model":"m1","tokens":{"input_tokens":1,"total_tokens":1}},
			{"timestamp":"2026-04-27T11:01:00Z","model":"m2","tokens":{"input_tokens":1,"total_tokens":1}}
		]`),
	}
	for index, payload := range payloads {
		if _, err := persister.Import(payload); err != nil {
			t.Fatalf("Import operation %d: %v", index+1, err)
		}
		if got := provider.CallCount(); got != index+1 {
			t.Fatalf("Snapshot calls after operation %d = %d, want %d", index+1, got, index+1)
		}
	}

	rows, err := store.db.Query("SELECT cost_micro FROM events ORDER BY ts_ns")
	if err != nil {
		t.Fatalf("query imported costs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var costs []int64
	for rows.Next() {
		var cost int64
		if err := rows.Scan(&cost); err != nil {
			t.Fatalf("scan imported cost: %v", err)
		}
		costs = append(costs, cost)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate imported costs: %v", err)
	}
	want := []int64{1, 1, 2, 2}
	if len(costs) != len(want) {
		t.Fatalf("imported costs = %v, want %v", costs, want)
	}
	for index := range want {
		if costs[index] != want[index] {
			t.Fatalf("imported costs = %v, want %v", costs, want)
		}
	}
}

func TestPersisterImport_RejectsWhenDBSizeCapReached(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()
	walPath := filepath.Join(dir, "events.db-wal")
	if err := os.WriteFile(walPath, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatalf("write wal: %v", err)
	}
	persister := &Persister{
		store:   store,
		baseDir: dir,
		retention: config.UsageRetention{
			Days:                -1,
			MaxDBSizeMB:         1,
			WarningThresholdPct: 80,
		},
	}
	_, err = persister.Import([]byte(`[]`))
	if err == nil {
		t.Fatal("expected import to be rejected when db size cap is reached")
	}
	if err.Error() != "usage: SQLite DB size cap reached, import refused" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPersisterDBSizeStatus_ReportsWarningAndCap(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()
	walPath := filepath.Join(dir, "events.db-wal")
	if err := os.WriteFile(walPath, make([]byte, 900<<10), 0o600); err != nil {
		t.Fatalf("write wal: %v", err)
	}
	persister := &Persister{
		store:   store,
		baseDir: dir,
		retention: config.UsageRetention{
			Days:                -1,
			MaxDBSizeMB:         1,
			WarningThresholdPct: 80,
		},
	}
	sizeBytes, maxBytes, warningThresholdPct, warning, capped := persister.DBSizeStatus()
	if maxBytes != 1<<20 {
		t.Fatalf("maxBytes=%d, want %d", maxBytes, 1<<20)
	}
	if sizeBytes == 0 {
		t.Fatal("expected non-zero sizeBytes")
	}
	if warningThresholdPct != 80 {
		t.Fatalf("warningThresholdPct=%d, want 80", warningThresholdPct)
	}
	if !warning {
		t.Fatal("expected warning=true when db size is above 80% of cap")
	}
	if capped {
		t.Fatal("expected capped=false while size is still below the hard cap")
	}
}

func TestSetPriceFuncWaitsForInFlightWriterGeneration(t *testing.T) {
	store := openTestStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var signalOnce sync.Once
	oldPrice := func(string) (float64, float64, float64, bool) {
		signalOnce.Do(func() { close(entered) })
		<-release
		return 1, 0, 0, true
	}
	newPrice := func(string) (float64, float64, float64, bool) {
		return 2, 0, 0, true
	}
	persister := &Persister{store: store, pricesFn: oldPrice}
	writer := newWriterWithPricingGate(store, oldPrice, &persister.pricingMu)
	persister.writer = writer

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- writer.flush([]FlatDetail{{
			Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
			Model:     "m",
			Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
		}})
	}()
	<-entered

	setDone := make(chan struct{})
	go func() {
		persister.SetPriceFunc(newPrice)
		close(setDone)
	}()
	select {
	case <-setDone:
		t.Fatal("SetPriceFunc returned while old pricing generation was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-flushDone; err != nil {
		t.Fatalf("writer flush: %v", err)
	}
	select {
	case <-setDone:
	case <-time.After(time.Second):
		t.Fatal("SetPriceFunc did not return after old pricing generation committed")
	}

	var cost int64
	if err := store.db.QueryRow("SELECT cost_micro FROM events").Scan(&cost); err != nil {
		t.Fatalf("query event cost: %v", err)
	}
	if cost != 1 {
		t.Fatalf("event cost = %d, want old generation cost 1", cost)
	}
}

func TestRecalculateCostsUsesOnePriceSnapshotPerOperation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, model := range []string{"m1", "m2"} {
		detail := FlatDetail{
			Timestamp: time.Date(2026, 4, 27, 10, index, 0, 0, time.UTC),
			Model:     model,
			Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist %s: %v", model, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	priceFor := func(prompt float64) PriceFunc {
		return func(string) (float64, float64, float64, bool) {
			return prompt, 0, 0, true
		}
	}
	provider := &scriptedPriceProvider{snapshots: []PriceFunc{priceFor(3), priceFor(4)}}
	persister := &Persister{store: store, priceProvider: provider}
	for operation, wantCost := range []int64{3, 4} {
		if _, err := persister.RecalculateCosts(); err != nil {
			t.Fatalf("RecalculateCosts operation %d: %v", operation+1, err)
		}
		if got := provider.CallCount(); got != operation+1 {
			t.Fatalf("Snapshot calls after operation %d = %d, want %d", operation+1, got, operation+1)
		}
		rows, err := store.db.Query("SELECT cost_micro FROM events ORDER BY ts_ns")
		if err != nil {
			t.Fatalf("query recalculated costs: %v", err)
		}
		count := 0
		for rows.Next() {
			var cost int64
			if err := rows.Scan(&cost); err != nil {
				_ = rows.Close()
				t.Fatalf("scan recalculated cost: %v", err)
			}
			if cost != wantCost {
				_ = rows.Close()
				t.Fatalf("operation %d event cost = %d, want %d", operation+1, cost, wantCost)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("iterate recalculated costs: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close recalculated costs: %v", err)
		}
		if count != 2 {
			t.Fatalf("operation %d event count = %d, want 2", operation+1, count)
		}
	}
}

func TestRecalculateCostsMatchesGoCostCalculation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	tests := []struct {
		model  string
		prices [3]float64
		tokens tokenStats
	}{
		{
			model:  "gpt-5.6-sol",
			prices: [3]float64{5, 30, 0.5},
			tokens: tokenStats{InputTokens: 12_345_678, OutputTokens: 234_567, CachedTokens: 5_678_901, TotalTokens: 12_580_245},
		},
		{
			model:  "gpt-5.6-terra",
			prices: [3]float64{2.5, 15, 0.25},
			tokens: tokenStats{InputTokens: 98_765_432, OutputTokens: 345_678, CachedTokens: 45_678_901, TotalTokens: 99_111_110},
		},
		{
			model:  "gpt-5.6-luna",
			prices: [3]float64{1, 6, 0.1},
			tokens: tokenStats{InputTokens: 7_654_321, OutputTokens: 123_456, CachedTokens: 3_210_987, TotalTokens: 7_777_777},
		},
		{
			model:  "deepseek-v4-pro",
			prices: [3]float64{0.435, 0.87, 0.003625},
			tokens: tokenStats{InputTokens: 2_877, OutputTokens: 60, TotalTokens: 2_937},
		},
	}
	prices := make(map[string][3]float64, len(tests))
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index := range tests {
		prices[tests[index].model] = tests[index].prices
		detail := FlatDetail{
			Timestamp: time.Date(2026, 7, 31, 0, index, 0, 0, time.UTC),
			Model:     tests[index].model,
			Tokens:    tests[index].tokens,
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist %s: %v", tests[index].model, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	priceFn := func(model string) (float64, float64, float64, bool) {
		price, ok := prices[model]
		return price[0], price[1], price[2], ok
	}
	persister := &Persister{store: store, pricesFn: priceFn}
	if _, err := persister.RecalculateCosts(); err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}

	rows, err := store.db.Query(`
		SELECT m.name, e.input_tokens, e.output_tokens, e.cached_tokens, e.total_tokens, e.cost_micro
		FROM events e
		JOIN dim_model m ON m.id = e.model_id
		ORDER BY e.id`)
	if err != nil {
		t.Fatalf("query costs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	index := 0
	for rows.Next() {
		var (
			model  string
			tokens tokenStats
			cost   int64
		)
		if err := rows.Scan(
			&model,
			&tokens.InputTokens,
			&tokens.OutputTokens,
			&tokens.CachedTokens,
			&tokens.TotalTokens,
			&cost,
		); err != nil {
			t.Fatalf("scan cost: %v", err)
		}
		if index >= len(tests) || model != tests[index].model {
			t.Fatalf("row %d model = %q", index, model)
		}
		want, err := computeCostMicro(tokens, priceFn, model)
		if err != nil {
			t.Fatalf("computeCostMicro(%s): %v", model, err)
		}
		if cost != want {
			t.Fatalf("%s cost_micro = %d, want %d", model, cost, want)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate costs: %v", err)
	}
	if index != len(tests) {
		t.Fatalf("cost rows = %d, want %d", index, len(tests))
	}
}

func TestRepriceModelEventsSkipsUnchangedCosts(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	priceFn := func(string) (float64, float64, float64, bool) {
		return 1, 0, 0, true
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	detail := FlatDetail{
		Timestamp: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Model:     "m",
		Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, priceFn); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TABLE event_update_audit (id INTEGER PRIMARY KEY);
		CREATE TRIGGER audit_event_cost_update
		AFTER UPDATE OF cost_micro ON events
		BEGIN
		    INSERT INTO event_update_audit(id) VALUES(NULL);
		END;
	`); err != nil {
		t.Fatalf("create update audit: %v", err)
	}

	var model modelRow
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT id, name FROM dim_model WHERE name = ?",
		detail.Model,
	).Scan(&model.id, &model.name); err != nil {
		t.Fatalf("query model: %v", err)
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin reprice: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := repriceModelEvents(ctx, tx, model, 1, 0, 0); err != nil {
		t.Fatalf("reprice unchanged event: %v", err)
	}
	var updates int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_update_audit").Scan(&updates); err != nil {
		t.Fatalf("count unchanged updates: %v", err)
	}
	if updates != 0 {
		t.Fatalf("unchanged event updates = %d, want 0", updates)
	}

	if err := repriceModelEvents(ctx, tx, model, 2, 0, 0); err != nil {
		t.Fatalf("reprice changed event: %v", err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_update_audit").Scan(&updates); err != nil {
		t.Fatalf("count changed updates: %v", err)
	}
	if updates != 1 {
		t.Fatalf("changed event updates = %d, want 1", updates)
	}
	var cost int64
	if err := tx.QueryRowContext(ctx, "SELECT cost_micro FROM events").Scan(&cost); err != nil {
		t.Fatalf("query repriced cost: %v", err)
	}
	if cost != 2000 {
		t.Fatalf("repriced cost = %d, want 2000", cost)
	}
}

func TestStartRecalculateCostsPublishesStatus(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	priceFn := func(string) (float64, float64, float64, bool) {
		return 1, 0, 0, true
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	detail := FlatDetail{
		Timestamp: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Model:     "m",
		Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	persister := &Persister{store: store, pricesFn: priceFn}
	unlock, err := persister.acquireMaintenance(ctx)
	if err != nil {
		t.Fatalf("acquire maintenance: %v", err)
	}
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	if !persister.StartRecalculateCosts() {
		t.Fatal("first recalculation did not start")
	}
	status := persister.RecalculateCostsStatus()
	if status.Status != "running" || !status.Running {
		t.Fatalf("running status = %+v", status)
	}
	if persister.StartRecalculateCosts() {
		t.Fatal("second recalculation started while first was running")
	}
	unlock()
	released = true

	deadline := time.Now().Add(2 * time.Second)
	for {
		status = persister.RecalculateCostsStatus()
		if !status.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recalculation did not finish: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "ok" || status.Error != "" {
		t.Fatalf("completed status = %+v", status)
	}
	if status.RecalculatedDays != 1 || status.TotalCost != 0.001 {
		t.Fatalf("completed result = %+v, want 1 day and 0.001 cost", status)
	}
}

func TestStartRecalculateCostsQueuesLatestPrices(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	oldPriceFn := func(string) (float64, float64, float64, bool) {
		close(entered)
		<-release
		return 1, 0, 0, true
	}
	newPriceFn := func(string) (float64, float64, float64, bool) {
		return 2, 0, 0, true
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	detail := FlatDetail{
		Timestamp: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Model:     "m",
		Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	persister := &Persister{store: store, pricesFn: oldPriceFn}
	if !persister.StartRecalculateCosts() {
		t.Fatal("first recalculation did not start")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first recalculation did not reach price lookup")
	}

	updated := make(chan struct{})
	go func() {
		persister.SetPriceFunc(newPriceFn)
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("price update blocked behind running recalculation")
	}
	if persister.StartRecalculateCosts() {
		close(release)
		t.Fatal("second recalculation started instead of being queued")
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		status := persister.RecalculateCostsStatus()
		if !status.Running {
			if status.Status != "ok" || status.Error != "" {
				t.Fatalf("completed status = %+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued recalculation did not finish: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var cost int64
	if err := store.db.QueryRowContext(ctx, "SELECT cost_micro FROM events").Scan(&cost); err != nil {
		t.Fatalf("query repriced cost: %v", err)
	}
	if cost != 2000 {
		t.Fatalf("repriced cost = %d, want latest price cost 2000", cost)
	}
}

func TestRecalculateCostsRejectsEventCostOverflow(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	detail := FlatDetail{
		Timestamp: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Model:     "overflow-model",
		Tokens:    tokenStats{InputTokens: 2, TotalTokens: 2},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	priceFn := func(string) (float64, float64, float64, bool) {
		return math.Exp2(62), 0, 0, true
	}
	persister := &Persister{store: store, pricesFn: priceFn}
	if _, err := persister.RecalculateCosts(); err == nil ||
		!strings.Contains(err.Error(), "cost_micro out of int64 range") {
		t.Fatalf("RecalculateCosts error = %v, want cost overflow", err)
	}
	var cost int64
	if err := store.db.QueryRowContext(ctx, "SELECT cost_micro FROM events").Scan(&cost); err != nil {
		t.Fatalf("query preserved event cost: %v", err)
	}
	if cost != 0 {
		t.Fatalf("event cost_micro = %d, want 0", cost)
	}
}

func TestRecalculateCosts_RebuildsProviderBuckets(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	priceFn := func(model string) (prompt, completion, cache float64, found bool) {
		return 1.0, 0.0, 0.0, true
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, detail := range []FlatDetail{
		{
			Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
			Model:     "m",
			APIKey:    "k",
			Source:    "same@example.com",
			Provider:  "claude",
			Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
		},
		{
			Timestamp: time.Date(2026, 4, 27, 10, 5, 0, 0, time.UTC),
			Model:     "m",
			APIKey:    "k",
			Source:    "same@example.com",
			Provider:  "codex",
			Tokens:    tokenStats{InputTokens: 2000, TotalTokens: 2000},
		},
	} {
		if _, err := persistEvent(ctx, tx, store, &detail, priceFn); err != nil {
			t.Fatalf("persist event: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	persister := &Persister{store: store, pricesFn: priceFn}
	if _, err := persister.RecalculateCosts(); err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}

	var hourBuckets int
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM hour_bucket WHERE bucket_ts_ns = ?",
		time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC).UnixNano(),
	).Scan(&hourBuckets); err != nil {
		t.Fatalf("count hour_bucket: %v", err)
	}
	if hourBuckets != 2 {
		t.Fatalf("hour_bucket rows=%d, want 2 distinct provider buckets", hourBuckets)
	}

	var dayBuckets int
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM day_bucket WHERE bucket_ts_ns = ?",
		time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC).UnixNano(),
	).Scan(&dayBuckets); err != nil {
		t.Fatalf("count day_bucket: %v", err)
	}
	if dayBuckets != 2 {
		t.Fatalf("day_bucket rows=%d, want 2 distinct provider buckets", dayBuckets)
	}

	var providerCount int
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(DISTINCT provider_id) FROM hour_bucket WHERE provider_id IS NOT NULL",
	).Scan(&providerCount); err != nil {
		t.Fatalf("count providers: %v", err)
	}
	if providerCount != 2 {
		t.Fatalf("distinct provider_id=%d, want 2", providerCount)
	}

	var watermark string
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT value FROM meta WHERE key = ?",
		dayRollupWatermarkKey,
	).Scan(&watermark); err != nil {
		t.Fatalf("query watermark: %v", err)
	}
	if watermark == "" {
		t.Fatal("expected day rollup watermark after recalculation")
	}

	var totalCost sql.NullInt64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT SUM(cost_micro) FROM hour_bucket",
	).Scan(&totalCost); err != nil {
		t.Fatalf("sum cost_micro: %v", err)
	}
	if !totalCost.Valid || totalCost.Int64 <= 0 {
		t.Fatalf("expected positive cost_micro after recalculation, got %+v", totalCost)
	}
}

func TestRecalculateCostsCombinesHourBucketsIntoOneDay(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	priceFn := func(string) (float64, float64, float64, bool) {
		return 1, 0, 0, true
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	details := []FlatDetail{
		{
			Timestamp: base,
			Model:     "m",
			LatencyMs: 15,
			Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
		},
		{
			Timestamp: base.Add(time.Hour),
			Model:     "m",
			LatencyMs: 25,
			Tokens:    tokenStats{InputTokens: 2000, TotalTokens: 2000},
		},
		{
			Timestamp: base.Add(90 * time.Minute),
			Model:     "m",
			LatencyMs: 35,
			Tokens:    tokenStats{InputTokens: 3000, TotalTokens: 3000},
		},
		{
			Timestamp: base.Add(2 * time.Hour),
			Model:     "m",
			LatencyMs: 45,
			Tokens:    tokenStats{InputTokens: 4000, TotalTokens: 4000},
		},
	}
	for index := range details {
		if _, err := persistEvent(ctx, tx, store, &details[index], priceFn); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist event[%d]: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit events: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		"UPDATE events SET metadata = ? WHERE ts_ns = ?",
		"{",
		details[2].Timestamp.UnixNano(),
	); err != nil {
		t.Fatalf("set malformed metadata: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		"UPDATE events SET metadata = ? WHERE ts_ns = ?",
		`{"latency_ms":"45"}`,
		details[3].Timestamp.UnixNano(),
	); err != nil {
		t.Fatalf("set invalid latency metadata: %v", err)
	}

	var logs bytes.Buffer
	logger := log.StandardLogger()
	originalOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(originalOutput) })

	persister := &Persister{store: store, pricesFn: priceFn}
	result, err := persister.RecalculateCosts()
	if err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}
	if result.RecalculatedDays != 1 {
		t.Fatalf("recalculated days = %d, want 1", result.RecalculatedDays)
	}
	if !strings.Contains(logs.String(), "skip latency backfill for 2 invalid metadata records") {
		t.Fatalf("latency warning missing from logs: %q", logs.String())
	}

	var hourBuckets int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hour_bucket").Scan(&hourBuckets); err != nil {
		t.Fatalf("count hour buckets: %v", err)
	}
	if hourBuckets != 3 {
		t.Fatalf("hour buckets = %d, want 3", hourBuckets)
	}

	dayStartNs := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC).UnixNano()
	var (
		requests, success, failure               int64
		inputTokens, totalTokens, costMicro      int64
		latencyTotal, latencySamples             int64
		credentialNull, apiKeyNull, providerNull int64
	)
	if err := store.db.QueryRowContext(
		ctx,
		`SELECT
		    requests, success, failure,
		    input_tokens, total_tokens, cost_micro,
		    latency_ms_total, latency_samples,
		    credential_id IS NULL, api_key_id IS NULL, provider_id IS NULL
		 FROM day_bucket
		 WHERE bucket_ts_ns = ?`,
		dayStartNs,
	).Scan(
		&requests,
		&success,
		&failure,
		&inputTokens,
		&totalTokens,
		&costMicro,
		&latencyTotal,
		&latencySamples,
		&credentialNull,
		&apiKeyNull,
		&providerNull,
	); err != nil {
		t.Fatalf("query day bucket: %v", err)
	}
	if requests != 4 || success != 4 || failure != 0 {
		t.Fatalf("day requests/success/failure = %d/%d/%d, want 4/4/0", requests, success, failure)
	}
	if inputTokens != 10_000 || totalTokens != 10_000 || costMicro != 10_000 {
		t.Fatalf("day input/total/cost = %d/%d/%d, want 10000/10000/10000", inputTokens, totalTokens, costMicro)
	}
	if latencyTotal != 40 || latencySamples != 2 {
		t.Fatalf("day latency = %d/%d, want 40/2", latencyTotal, latencySamples)
	}
	if credentialNull != 1 || apiKeyNull != 1 || providerNull != 1 {
		t.Fatalf(
			"day nullable dimensions = %d/%d/%d, want 1/1/1",
			credentialNull,
			apiKeyNull,
			providerNull,
		)
	}
}

func TestRecalculateCostsDayOverflowPreservesExistingBuckets(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, timestamp := range []time.Time{base, base.Add(time.Hour)} {
		detail := FlatDetail{
			Timestamp: timestamp,
			Model:     "overflow-model",
			Tokens:    tokenStats{InputTokens: 1, TotalTokens: int64(index + 1)},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist event[%d]: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit events: %v", err)
	}

	var modelID int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT id FROM dim_model WHERE name = 'overflow-model'",
	).Scan(&modelID); err != nil {
		t.Fatalf("query model id: %v", err)
	}
	dayStartNs := base.Truncate(24 * time.Hour).UnixNano()
	if _, err := store.db.ExecContext(
		ctx,
		`INSERT INTO day_bucket (
			bucket_ts_ns, model_id,
			requests, success, failure,
			input_tokens, output_tokens, reasoning_tokens,
			cached_tokens, total_tokens, cost_micro,
			latency_ms_total, latency_samples
		) VALUES (?, ?, 1, 1, 0, 0, 0, 0, 0, 123, 123, 0, 0)`,
		dayStartNs,
		modelID,
	); err != nil {
		t.Fatalf("seed day bucket: %v", err)
	}

	priceFn := func(string) (float64, float64, float64, bool) {
		return math.Exp2(62), 0, 0, true
	}
	persister := &Persister{store: store, pricesFn: priceFn}
	_, err = persister.RecalculateCosts()
	if err == nil || !strings.Contains(err.Error(), "rebuild day_bucket") {
		t.Fatalf("RecalculateCosts error = %v, want day bucket overflow", err)
	}

	var dayCost, dayTokens, eventCost, hourCost int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT cost_micro, total_tokens FROM day_bucket WHERE bucket_ts_ns = ? AND model_id = ?",
		dayStartNs,
		modelID,
	).Scan(&dayCost, &dayTokens); err != nil {
		t.Fatalf("query preserved day bucket: %v", err)
	}
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COALESCE(SUM(cost_micro), 0) FROM events WHERE model_id = ?",
		modelID,
	).Scan(&eventCost); err != nil {
		t.Fatalf("query preserved event cost: %v", err)
	}
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COALESCE(SUM(cost_micro), 0) FROM hour_bucket WHERE model_id = ?",
		modelID,
	).Scan(&hourCost); err != nil {
		t.Fatalf("query preserved hour cost: %v", err)
	}
	if dayCost != 123 || dayTokens != 123 || eventCost != 0 || hourCost != 0 {
		t.Fatalf(
			"preserved values day_cost=%d day_tokens=%d event_cost=%d hour_cost=%d, want 123/123/0/0",
			dayCost,
			dayTokens,
			eventCost,
			hourCost,
		)
	}
}

func TestTrimBeforeUsesExactCutoffAndRebuildsBuckets(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, minute := range []int{10, 40} {
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(minute) * time.Minute),
			Model:     "m",
			Tokens:    tokenStats{TotalTokens: int64(index + 1)},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	persister := &Persister{store: store}
	if err := persister.trimBefore(base.Add(30 * time.Minute)); err != nil {
		t.Fatalf("trimBefore: %v", err)
	}

	var eventCount, eventTokens int64
	var eventTimestamp int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*), MIN(ts_ns), SUM(total_tokens) FROM events",
	).Scan(&eventCount, &eventTimestamp, &eventTokens); err != nil {
		t.Fatalf("query events: %v", err)
	}
	wantTimestamp := base.Add(40 * time.Minute).UnixNano()
	if eventCount != 1 || eventTimestamp != wantTimestamp || eventTokens != 2 {
		t.Fatalf("events = count:%d timestamp:%d tokens:%d, want 1/%d/2", eventCount, eventTimestamp, eventTokens, wantTimestamp)
	}

	for _, table := range []string{"hour_bucket", "day_bucket"} {
		var rows, requests, tokens int64
		if err := store.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*), SUM(requests), SUM(total_tokens) FROM "+table,
		).Scan(&rows, &requests, &tokens); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if rows != 1 || requests != 1 || tokens != 2 {
			t.Fatalf("%s = rows:%d requests:%d tokens:%d, want 1/1/2", table, rows, requests, tokens)
		}
	}
	var dirtyDays int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dirty_day").Scan(&dirtyDays); err != nil {
		t.Fatalf("query dirty days: %v", err)
	}
	if dirtyDays != 0 {
		t.Fatalf("dirty days = %d, want 0", dirtyDays)
	}
}

func TestTrimBeforeRollbackPreservesEventsAndBuckets(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index, minute := range []int{10, 40} {
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(minute) * time.Minute),
			Model:     "m",
			Tokens:    tokenStats{TotalTokens: int64(index + 1)},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist[%d]: %v", index, err)
		}
	}
	if err := rebuildUsageBucketsTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed buckets: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_trim_bucket_rebuild
		BEFORE INSERT ON hour_bucket
		BEGIN
			SELECT RAISE(ABORT, 'forced rebuild failure');
		END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	persister := &Persister{store: store}
	err = persister.trimBefore(base.Add(30 * time.Minute))
	if err == nil || !strings.Contains(err.Error(), "rebuild hour_bucket") {
		t.Fatalf("trimBefore error = %v, want rebuild failure", err)
	}

	for _, table := range []string{"events", "hour_bucket", "day_bucket"} {
		var requests, tokens int64
		query := "SELECT COUNT(*), SUM(total_tokens) FROM " + table
		if table != "events" {
			query = "SELECT SUM(requests), SUM(total_tokens) FROM " + table
		}
		if err := store.db.QueryRowContext(ctx, query).Scan(&requests, &tokens); err != nil {
			t.Fatalf("query preserved %s: %v", table, err)
		}
		if requests != 2 || tokens != 3 {
			t.Fatalf("preserved %s = requests:%d tokens:%d, want 2/3", table, requests, tokens)
		}
	}
}

func TestTrimBeforeWaitsForPricingMaintenanceGate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	detail := FlatDetail{
		Timestamp: timestamp,
		Model:     "m",
		Tokens:    tokenStats{TotalTokens: 1},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	persister := &Persister{store: store}
	persister.pricingMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- persister.trimBefore(timestamp.Add(time.Minute))
	}()
	select {
	case err := <-done:
		persister.pricingMu.Unlock()
		t.Fatalf("trimBefore returned before pricing gate released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	var count int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		persister.pricingMu.Unlock()
		t.Fatalf("query blocked trim events: %v", err)
	}
	if count != 1 {
		persister.pricingMu.Unlock()
		t.Fatalf("events changed while trim was blocked: %d", count)
	}
	persister.pricingMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("trimBefore: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("trimBefore did not finish after pricing gate released")
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		t.Fatalf("query trimmed events: %v", err)
	}
	if count != 0 {
		t.Fatalf("events after trim = %d, want 0", count)
	}
}

func TestMigrateDataDirCrossDevicePublishesCompleteDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.yaml")
	oldBaseDir := filepath.Join(filepath.Dir(configPath), dataDirName)
	newBaseDir := filepath.Join(root, "target", dataDirName)
	if errMkdir := os.MkdirAll(filepath.Join(oldBaseDir, "detail"), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(oldBaseDir, "events.db"), []byte("database"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(oldBaseDir, "detail", "day.json"), []byte("detail"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	previousRename := renameUsageDataDir
	renameUsageDataDir = func(oldPath, newPath string) error {
		if oldPath == oldBaseDir && newPath == newBaseDir {
			return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renameUsageDataDir = previousRename })

	if errMigrate := MigrateDataDirIfNeeded(configPath, newBaseDir); errMigrate != nil {
		t.Fatalf("MigrateDataDirIfNeeded() error = %v", errMigrate)
	}
	for path, want := range map[string]string{
		filepath.Join(newBaseDir, "events.db"):          "database",
		filepath.Join(newBaseDir, "detail", "day.json"): "detail",
	} {
		raw, errRead := os.ReadFile(path)
		if errRead != nil || string(raw) != want {
			t.Fatalf("migrated file %s = %q, error=%v, want %q", path, raw, errRead, want)
		}
	}
	if _, errStat := os.Stat(oldBaseDir); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("old directory still exists: %v", errStat)
	}
	if _, errStat := os.Stat(newBaseDir + migrationStagingSuffix); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("staging directory still exists: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(newBaseDir, migrationReadyMarkerFile)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("migration marker still exists: %v", errStat)
	}
}

func TestMigrateDataDirRecoversCompletedStagingDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.yaml")
	oldBaseDir := filepath.Join(filepath.Dir(configPath), dataDirName)
	newBaseDir := filepath.Join(root, "target", dataDirName)
	stagingDir := newBaseDir + migrationStagingSuffix
	if errMkdir := os.MkdirAll(oldBaseDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errMkdir := os.MkdirAll(stagingDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(oldBaseDir, "events.db"), []byte("source"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(stagingDir, "events.db"), []byte("staged"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errMarker := writeMigrationReadyMarker(stagingDir, oldBaseDir); errMarker != nil {
		t.Fatal(errMarker)
	}

	previousRename := renameUsageDataDir
	renameUsageDataDir = func(oldPath, newPath string) error {
		if oldPath == oldBaseDir && newPath == newBaseDir {
			return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renameUsageDataDir = previousRename })

	if errMigrate := MigrateDataDirIfNeeded(configPath, newBaseDir); errMigrate != nil {
		t.Fatalf("MigrateDataDirIfNeeded() error = %v", errMigrate)
	}
	raw, errRead := os.ReadFile(filepath.Join(newBaseDir, "events.db"))
	if errRead != nil || string(raw) != "staged" {
		t.Fatalf("recovered events.db = %q, error=%v", raw, errRead)
	}
}

func TestPersisterWriterSurvivesRunContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	persister := NewPersister(t.TempDir(), config.UsageRetention{})
	persister.Start(ctx)
	if !persister.IsReady() {
		t.Fatal("persister did not start")
	}
	cancel()
	persister.Record(FlatDetail{
		Timestamp: time.Now().UTC(),
		Model:     "after-run-cancel",
		Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
	})

	deadline := time.Now().Add(time.Second)
	for {
		snapshot, err := persister.QuerySnapshot()
		if err == nil && snapshot.TotalRequests == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record was not persisted after caller cancellation: snapshot=%#v error=%v", snapshot, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	persister.Stop()
}

func TestPersisterShutdownCancelsMaintenanceLockWait(t *testing.T) {
	persister := NewPersister(t.TempDir(), config.UsageRetention{})
	persister.SetPriceFunc(func(string) (float64, float64, float64, bool) {
		return 1, 1, 0, true
	})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("persister did not start")
	}
	unlock, errLock := persister.acquireMaintenance(context.Background())
	if errLock != nil {
		t.Fatalf("acquireMaintenance() error = %v", errLock)
	}
	defer unlock()

	recalculateDone := make(chan error, 1)
	go func() {
		_, err := persister.RecalculateCostsContext(context.Background())
		recalculateDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !persister.costRecalcRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("recalculation did not begin waiting for maintenance")
		}
		time.Sleep(time.Millisecond)
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	if errStop := persister.StopContext(stopCtx); errStop != nil {
		cancelStop()
		t.Fatalf("StopContext() error = %v", errStop)
	}
	cancelStop()
	select {
	case errRecalculate := <-recalculateDone:
		if !errors.Is(errRecalculate, context.Canceled) {
			t.Fatalf("RecalculateCostsContext() error = %v, want canceled", errRecalculate)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance lock wait was not canceled by shutdown")
	}
}

func TestRecalculateCostsPreservesPreUnixEpochBucketsAndLatency(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	timestamp := time.Date(1969, 12, 31, 23, 30, 0, 0, time.UTC)
	priceFn := func(string) (float64, float64, float64, bool) {
		return 1, 0, 0, true
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	detail := FlatDetail{
		Timestamp: timestamp,
		Model:     "m",
		LatencyMs: 25,
		Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, priceFn); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	persister := &Persister{store: store, pricesFn: priceFn}
	result, err := persister.RecalculateCosts()
	if err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}
	if result.RecalculatedDays != 1 {
		t.Fatalf("recalculated days = %d, want 1", result.RecalculatedDays)
	}

	wantHour := time.Date(1969, 12, 31, 23, 0, 0, 0, time.UTC).UnixNano()
	wantDay := time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC).UnixNano()
	for table, wantBucket := range map[string]int64{
		"hour_bucket": wantHour,
		"day_bucket":  wantDay,
	} {
		var bucket, latencyTotal, latencySamples int64
		if err := store.db.QueryRowContext(
			ctx,
			"SELECT bucket_ts_ns, latency_ms_total, latency_samples FROM "+table,
		).Scan(&bucket, &latencyTotal, &latencySamples); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if bucket != wantBucket || latencyTotal != 25 || latencySamples != 1 {
			t.Fatalf("%s = bucket:%d latency:%d samples:%d, want %d/25/1", table, bucket, latencyTotal, latencySamples, wantBucket)
		}
	}
}
