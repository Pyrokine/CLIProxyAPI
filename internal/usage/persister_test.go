package usage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

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
	pricesPath := filepath.Join(dir, "model-prices.json")
	if err := os.WriteFile(
		pricesPath, []byte(`{"claude-opus-4-6":{"prompt":5,"completion":25,"cache":0.5}}`), 0o600,
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

func TestPersisterImport_RejectsWhenDBSizeCapReached(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
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
	defer store.Close()
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
