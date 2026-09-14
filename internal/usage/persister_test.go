package usage

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
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

type blockingPriceProvider struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (p *blockingPriceProvider) Snapshot() PriceFunc {
	p.once.Do(
		func() {
			close(p.entered)
			<-p.release
		},
	)
	return nil
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
	for i := range goroutines {
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
	if got := buildModelPriceFunc(configPath); got != nil {
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
	priceFn := buildModelPriceFunc(configPath)
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

func TestBuildModelPriceProviderExposesExplicitUnpricedModels(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := modelcatalog.ReplacePrices(
		configPath,
		map[string]modelcatalog.Price{
			"unpriced-model": {
				Status:         modelcatalog.PriceStatusUnpriced,
				UnpricedReason: "official token price is unavailable",
			},
		},
	); err != nil {
		t.Fatalf("write unpriced manifest: %v", err)
	}
	provider := BuildModelPriceProvider(configPath)
	if provider == nil {
		t.Fatal("explicit unpriced manifest did not create a price provider")
	}
	legacy := provider.Snapshot()
	if legacy == nil {
		t.Fatal("legacy snapshot function is unavailable")
	}
	if _, _, _, found := legacy("unpriced-model"); found {
		t.Fatal("legacy price snapshot exposed reference rates for an unpriced model")
	}
	detailedProvider, ok := provider.(detailedPriceProvider)
	if !ok {
		t.Fatal("model price provider does not expose detailed prices")
	}
	price, found := detailedProvider.DetailedSnapshot()("unpriced-model", time.Now())
	if !found || price.Status != CostStatusUnpriced || price.Reason != "official token price is unavailable" {
		t.Fatalf("detailed unpriced model = %+v, found=%t", price, found)
	}
	persister := &Persister{priceProvider: provider}
	if !persister.HasPricing() {
		t.Fatal("persister did not recognize an explicit unpriced manifest")
	}
}

func TestModelPriceCatalogResolvesOnlyUnambiguousExplicitAliases(t *testing.T) {
	catalog := modelcatalog.File{
		Prices: map[string]modelcatalog.Price{
			"canonical-a":  {Prompt: 2, Completion: 4, Cache: 1},
			"canonical-b":  {Prompt: 3, Completion: 6, Cache: 1.5},
			"direct-alias": {Prompt: 4, Completion: 8, Cache: 2},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"claude": {
				{Name: "canonical-a", Alias: "alias-a"},
				{Name: "canonical-a", Alias: "direct-alias"},
				{Name: "canonical-a", Alias: "conflicting-alias"},
			},
			"codex": {
				{Name: "canonical-b", Alias: "conflicting-alias"},
			},
		},
	}
	priceFn := detailedPriceFuncFromCatalog(catalog)
	if priceFn == nil {
		t.Fatal("detailed price snapshot is nil")
	}
	for model, want := range map[string]struct {
		prompt    float64
		canonical string
	}{
		"canonical-a":  {prompt: 2, canonical: "canonical-a"},
		"alias-a":      {prompt: 2, canonical: "canonical-a"},
		"direct-alias": {prompt: 4, canonical: "direct-alias"},
	} {
		price, found := priceFn(model, time.Now())
		if !found || price.Prompt != want.prompt || price.CanonicalModel != want.canonical {
			t.Errorf(
				"price(%q) = %+v, found=%t, want prompt/canonical=%v/%q", model, price, found, want.prompt,
				want.canonical,
			)
		}
	}
	if _, found := priceFn("conflicting-alias", time.Now()); found {
		t.Fatal("ambiguous cross-channel alias resolved to a price")
	}
}

func TestModelPriceCatalogRevisionAndDetailsIncludeCacheWriteTTLRates(t *testing.T) {
	cacheRead := 0.2
	cacheWrite := 3.0
	cacheWrite5m := 2.5
	cacheWrite1h := 4.0
	catalog := modelcatalog.File{
		Prices: map[string]modelcatalog.Price{
			"canonical": {
				Prompt:         1,
				Completion:     5,
				CacheRead:      &cacheRead,
				CacheWrite:     &cacheWrite,
				CacheWrite5m:   &cacheWrite5m,
				CacheWrite1h:   &cacheWrite1h,
				Status:         modelcatalog.PriceStatusComplete,
				Currency:       "usd",
				Source:         "catalog-source",
				EffectiveFrom:  "2026-09-01",
				EffectiveUntil: "2026-10-01",
			},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"claude": {{Name: "canonical", Alias: "alias"}},
		},
	}
	revision := catalogPriceRevision(catalog)
	if revision == "" {
		t.Fatal("catalog price revision is empty")
	}
	changed := catalog
	changed.Prices = make(map[string]modelcatalog.Price, len(catalog.Prices))
	maps.Copy(changed.Prices, catalog.Prices)
	changedPrice := changed.Prices["canonical"]
	changedCacheWrite1h := 4.5
	changedPrice.CacheWrite1h = &changedCacheWrite1h
	changed.Prices["canonical"] = changedPrice
	if changedRevision := catalogPriceRevision(changed); changedRevision == "" || changedRevision == revision {
		t.Fatalf("changed catalog revision = %q, original = %q", changedRevision, revision)
	}

	priceFn := detailedPriceFuncFromCatalog(catalog)
	if priceFn == nil {
		t.Fatal("detailed price snapshot is nil")
	}
	price, found := priceFn("alias", time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))
	if !found || price.CanonicalModel != "canonical" || price.Source != "catalog-source" ||
		price.Currency != "USD" || price.EffectiveFrom != "2026-09-01" ||
		price.EffectiveUntil != "2026-10-01" || price.CacheWrite5m == nil ||
		*price.CacheWrite5m != 2.5 || price.CacheWrite1h == nil || *price.CacheWrite1h != 4 {
		t.Fatalf("detailed alias price = %+v, found=%t", price, found)
	}

	unsupported := catalog
	unsupported.Prices = make(map[string]modelcatalog.Price, len(catalog.Prices))
	for name, catalogPrice := range catalog.Prices {
		catalogPrice.Currency = "EUR"
		unsupported.Prices[name] = catalogPrice
	}
	legacyPriceFn := priceFuncFromCatalog(unsupported)
	if _, _, _, found = legacyPriceFn("alias"); found {
		t.Fatal("legacy price function accepted a non-USD catalog rate")
	}
	unsupportedDetailFn := detailedPriceFuncFromCatalog(unsupported)
	unsupportedPrice, found := unsupportedDetailFn("alias", time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))
	if !found || unsupportedPrice.Currency != "EUR" {
		t.Fatalf("non-USD detailed price = %+v, found=%t", unsupportedPrice, found)
	}
}

func TestComputeCostUsesMutuallyExclusiveCanonicalBuckets(t *testing.T) {
	cacheRead := 0.5
	cacheWrite := 3.0
	tokens := tokenStats{
		InputTokens:      1_000,
		OutputTokens:     300,
		ReasoningTokens:  100,
		CachedTokens:     300,
		CacheReadTokens:  200,
		CacheWriteTokens: 100,
		TotalTokens:      1_300,
	}
	breakdown := coreusage.NewSubsetTokenBreakdown(1_000, 200, 100, 300, 100, 1_300)
	tests := []struct {
		name       string
		price      detailedPrice
		wantMicro  int64
		wantStatus CostStatus
		wantReason string
	}{
		{
			name: "complete",
			price: detailedPrice{
				Prompt: 2, Completion: 10,
				CacheRead: &cacheRead, CacheWrite: &cacheWrite,
				Status: costStatusComplete, Automatic: true, Effective: true,
			},
			wantMicro:  4_800,
			wantStatus: costStatusComplete,
		},
		{
			name: "missing cache write rate uses prompt reference",
			price: detailedPrice{
				Prompt: 2, Completion: 10, CacheRead: &cacheRead,
				Status: costStatusComplete, Effective: true,
			},
			wantMicro:  4_700,
			wantStatus: costStatusComplete,
			wantReason: "cache-write reference price uses prompt rate",
		},
		{
			name: "partial reference price does not require auto apply",
			price: detailedPrice{
				Prompt: 2, Completion: 10,
				Status: costStatusPartial, Effective: true,
			},
			wantMicro:  5_000,
			wantStatus: costStatusPartial,
			wantReason: "cache-read reference price uses prompt rate; cache-write reference price uses prompt rate",
		},
		{
			name: "unpriced status with reference rates",
			price: detailedPrice{
				Prompt: 2, Completion: 10, CacheRead: &cacheRead, CacheWrite: &cacheWrite,
				Status: CostStatusUnpriced, Effective: true,
				Reason: "published rates omit request conditions",
			},
			wantMicro:  4_800,
			wantStatus: CostStatusUnpriced,
			wantReason: "published rates omit request conditions",
		},
		{
			name: "outside effective range",
			price: detailedPrice{
				Status: costStatusComplete, Automatic: true,
			},
			wantStatus: CostStatusUnpriced,
			wantReason: "price is not effective at the event timestamp",
		},
		{
			name: "explicitly unpriced",
			price: detailedPrice{
				Status: CostStatusUnpriced, Effective: true,
				Reason: "media price is not token based",
			},
			wantStatus: CostStatusUnpriced,
			wantReason: "media price is not token based",
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				pricing := pricingSnapshot{
					detailedPriceFn: func(string, time.Time) (detailedPrice, bool) {
						return tt.price, true
					},
				}
				got, err := computeCost(tokens, &breakdown, pricing, "model", time.Now())
				if err != nil {
					t.Fatalf("computeCost: %v", err)
				}
				if got.Micro != tt.wantMicro || got.Status != tt.wantStatus || got.Reason != tt.wantReason {
					t.Fatalf(
						"computeCost = %+v, want micro=%d status=%q reason=%q", got, tt.wantMicro, tt.wantStatus,
						tt.wantReason,
					)
				}
			},
		)
	}
}

func TestComputeCostHonorsCanonicalAccountingQuality(t *testing.T) {
	cacheRead := 0.5
	cacheWrite := 3.0
	pricing := pricingSnapshot{
		detailedPriceFn: func(string, time.Time) (detailedPrice, bool) {
			return detailedPrice{
				Prompt: 2, Completion: 10,
				CacheRead: &cacheRead, CacheWrite: &cacheWrite,
				Status: costStatusComplete, Effective: true,
			}, true
		},
	}
	flatTokens := tokenStats{
		InputTokens: 3, OutputTokens: 4, CachedTokens: 10,
		CacheReadTokens: 8, CacheWriteTokens: 2, TotalTokens: 7,
	}
	tests := []struct {
		name       string
		breakdown  coreusage.TokenBreakdown
		wantMicro  int64
		wantStatus CostStatus
		wantReason string
	}{
		{
			name:       "unclassified",
			breakdown:  coreusage.NewPartialSubsetTokenBreakdown(1_000, 200, 100, 300, 100, 1_400),
			wantMicro:  4_800,
			wantStatus: costStatusPartial,
			wantReason: "token accounting contains unclassified tokens",
		},
		{
			name:       "inconsistent",
			breakdown:  coreusage.NewSubsetTokenBreakdown(1, 2, 0, 0, 0, 3),
			wantStatus: CostStatusUnpriced,
			wantReason: "token accounting is inconsistent",
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if !tt.breakdown.Valid() {
					t.Fatalf("breakdown is invalid: %+v", tt.breakdown)
				}
				got, err := computeCost(flatTokens, &tt.breakdown, pricing, "model", time.Now())
				if err != nil {
					t.Fatalf("computeCost: %v", err)
				}
				if got.Micro != tt.wantMicro || got.Status != tt.wantStatus || got.Reason != tt.wantReason {
					t.Fatalf(
						"computeCost = %+v, want micro=%d status=%q reason=%q", got, tt.wantMicro, tt.wantStatus,
						tt.wantReason,
					)
				}
			},
		)
	}
}

func TestDetailedPricingPersistsCanonicalCostAndCacheBuckets(t *testing.T) {
	store := openTestStore(t)
	cacheRead := 0.5
	cacheWrite := 3.0
	provider := &modelPriceProvider{
		cached: modelcatalog.File{
			Prices: map[string]modelcatalog.Price{
				"priced-model": {
					Prompt: 2, Completion: 10,
					CacheRead: &cacheRead, CacheWrite: &cacheWrite,
					Status: modelcatalog.PriceStatusComplete,
				},
			},
		},
		loadedAt: time.Now(),
	}
	breakdown := coreusage.NewSubsetTokenBreakdown(1_000, 200, 100, 300, 100, 1_300)
	detail := FlatDetail{
		Timestamp:         time.Date(2026, 8, 30, 3, 0, 0, 0, time.UTC),
		Model:             "priced-model",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    breakdown,
		Tokens: tokenStats{
			InputTokens:      3,
			OutputTokens:     4,
			CachedTokens:     10,
			CacheReadTokens:  8,
			CacheWriteTokens: 2,
			TotalTokens:      7,
		},
	}
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	if err := writer.flush([]FlatDetail{detail}); err != nil {
		t.Fatalf("persist detailed price event: %v", err)
	}

	events, total, err := store.queryEventDetails(
		context.Background(),
		detail.Timestamp.Add(-time.Second),
		detail.Timestamp.Add(time.Second),
		EventFilters{},
		1,
		10,
		"timestamp",
		false,
	)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("event count = %d/%d, want 1/1", total, len(events))
	}
	got := events[0]
	if got.Tokens.CacheReadTokens != 8 || got.Tokens.CacheWriteTokens != 2 ||
		got.Cost != 0.0048 || got.CostStatus != costStatusComplete || got.CostReason != "" {
		t.Fatalf("queried event = %+v", got)
	}
	var cacheReadTokens, cacheWriteTokens, costMicro int64
	if err = store.db.QueryRow(
		`
		SELECT cache_read_tokens, cache_write_tokens, cost_micro
		  FROM hour_bucket`,
	).Scan(&cacheReadTokens, &cacheWriteTokens, &costMicro); err != nil {
		t.Fatalf("query hour bucket: %v", err)
	}
	if cacheReadTokens != 8 || cacheWriteTokens != 2 || costMicro != 4_800 {
		t.Fatalf("hour bucket = read:%d write:%d cost:%d", cacheReadTokens, cacheWriteTokens, costMicro)
	}
}

func TestDetailedPricingPersistsReferenceCostWithoutCanonicalMetadata(t *testing.T) {
	store := openTestStore(t)
	cacheRead := 0.5
	provider := &modelPriceProvider{
		cached: modelcatalog.File{
			Prices: map[string]modelcatalog.Price{
				"partial-model": {
					Prompt: 2, Completion: 10, CacheRead: &cacheRead,
					Status: modelcatalog.PriceStatusPartial,
				},
			},
		},
		loadedAt: time.Now(),
	}
	detail := FlatDetail{
		Timestamp: time.Date(2026, 8, 30, 3, 0, 0, 0, time.UTC),
		Model:     "partial-model",
		Tokens: tokenStats{
			InputTokens:      1_000,
			OutputTokens:     300,
			ReasoningTokens:  100,
			CachedTokens:     300,
			CacheReadTokens:  200,
			CacheWriteTokens: 100,
			TotalTokens:      1_300,
		},
	}
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	if err := writer.flush([]FlatDetail{detail}); err != nil {
		t.Fatalf("persist reference price event: %v", err)
	}

	events, total, err := store.queryEventDetails(
		context.Background(),
		detail.Timestamp.Add(-time.Second),
		detail.Timestamp.Add(time.Second),
		EventFilters{},
		1,
		10,
		"timestamp",
		false,
	)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("event count = %d/%d, want 1/1", total, len(events))
	}
	if got := events[0]; got.Cost != 0.0047 || got.CostStatus != costStatusPartial ||
		got.CostReason != "legacy cache overlap is ambiguous; applied subset assumption; cache-write reference price uses prompt rate" {
		t.Fatalf("queried event = %+v", got)
	}
	var costMicro int64
	if err = store.db.QueryRow("SELECT cost_micro FROM hour_bucket").Scan(&costMicro); err != nil {
		t.Fatalf("query hour bucket: %v", err)
	}
	if costMicro != 4_700 {
		t.Fatalf("hour bucket cost_micro = %d, want 4700", costMicro)
	}
}

func TestUsageBucketsAndSummaryTrackCostCoverage(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	events := []FlatDetail{
		{
			Timestamp: base.Add(time.Minute), RequestID: "complete", Model: "coverage-model",
			Tokens: tokenStats{TotalTokens: 10}, CostStatus: costStatusComplete,
			costMicro: 100, costTrusted: true,
		},
		{
			Timestamp: base.Add(time.Hour), RequestID: "partial", Model: "coverage-model",
			Tokens: tokenStats{TotalTokens: 20}, CostStatus: costStatusPartial,
			costMicro: 50, costTrusted: true,
		},
		{
			Timestamp: base.Add(2 * time.Hour), RequestID: "unpriced", Model: "coverage-model",
			Tokens: tokenStats{TotalTokens: 30}, CostStatus: CostStatusUnpriced,
			costTrusted: true,
		},
	}
	writer := newWriter(store, nil)
	if err := writer.flush(events); err != nil {
		t.Fatalf("flush coverage events: %v", err)
	}
	if err := store.rollupDay(context.Background(), base.UnixNano()); err != nil {
		t.Fatalf("roll up coverage day: %v", err)
	}

	result, err := store.querySummary(
		context.Background(), base, base.Add(3*time.Hour), EventFilters{}, "hourly", false,
	)
	if err != nil {
		t.Fatalf("query coverage summary: %v", err)
	}
	coverage := result.Totals.CostCoverage
	if coverage.Complete.Requests != 1 || coverage.Complete.Tokens != 10 ||
		coverage.Partial.Requests != 1 || coverage.Partial.Tokens != 20 ||
		coverage.Unpriced.Requests != 1 || coverage.Unpriced.Tokens != 30 {
		t.Fatalf("summary cost coverage = %+v", coverage)
	}
	if result.Totals.Cost != 0.00015 {
		t.Fatalf("summary cost = %v, want 0.00015", result.Totals.Cost)
	}

	for _, table := range []string{"hour_bucket", "day_bucket"} {
		var completeRequests, partialRequests, unpricedRequests int64
		var completeTokens, partialTokens, unpricedTokens int64
		if err = store.db.QueryRow(
			"SELECT SUM(complete_requests), SUM(partial_requests), SUM(unpriced_requests), "+
				"SUM(complete_tokens), SUM(partial_tokens), SUM(unpriced_tokens) FROM "+table,
		).Scan(
			&completeRequests, &partialRequests, &unpricedRequests,
			&completeTokens, &partialTokens, &unpricedTokens,
		); err != nil {
			t.Fatalf("query %s coverage: %v", table, err)
		}
		if completeRequests != 1 || partialRequests != 1 || unpricedRequests != 1 ||
			completeTokens != 10 || partialTokens != 20 || unpricedTokens != 30 {
			t.Fatalf(
				"%s coverage = requests %d/%d/%d, tokens %d/%d/%d",
				table,
				completeRequests,
				partialRequests,
				unpricedRequests,
				completeTokens,
				partialTokens,
				unpricedTokens,
			)
		}
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
	store, err := openStore(filepath.Join(dir, "events.db"))
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

func TestOverDBSizeCapSkipsDisabledChecksAndRefreshesEnabledChecks(t *testing.T) {
	dir := t.TempDir()
	originalStat := statUsageFile
	statCalls := 0
	statUsageFile = func(name string) (os.FileInfo, error) {
		statCalls++
		return originalStat(name)
	}
	t.Cleanup(func() { statUsageFile = originalStat })

	persister := &Persister{
		baseDir: dir,
		retention: config.UsageRetention{
			Days:        -1,
			MaxDBSizeMB: 0,
		},
	}
	if persister.overDBSizeCap() {
		t.Fatal("disabled DB size cap reported capped")
	}
	if statCalls != 0 {
		t.Fatalf("disabled DB size cap performed %d stat calls, want 0", statCalls)
	}

	persister.SetRetention(config.UsageRetention{Days: -1, MaxDBSizeMB: 1})
	if statCalls != 3 {
		t.Fatalf("enabling DB size cap performed %d stat calls, want 3", statCalls)
	}
	if persister.dbSizeCapReached() {
		t.Fatal("empty usage directory reported capped")
	}

	if err := os.WriteFile(filepath.Join(dir, dbFileName), make([]byte, 2<<20), 0o600); err != nil {
		t.Fatalf("write oversized DB: %v", err)
	}
	if !persister.overDBSizeCap() {
		t.Fatal("exact DB size check did not detect cap")
	}
	if statCalls != 6 {
		t.Fatalf("refreshed DB size check performed %d stat calls, want 6", statCalls)
	}
	if !persister.dbSizeCapReached() {
		t.Fatal("DB size cap state was not published")
	}
	if persister.Record(FlatDetail{Timestamp: time.Now(), Model: "capped"}) {
		t.Fatal("Record accepted an event after the DB size cap was reached")
	}
}

func TestPersisterDBSizeStatus_ReportsWarningAndCap(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "events.db"))
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
		flushDone <- writer.flush(
			[]FlatDetail{
				{
					Timestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
					Model:     "m",
					Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
				},
			},
		)
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
		if _, err := persister.recalculateCosts(); err != nil {
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
			tokens: tokenStats{
				InputTokens: 12_345_678, OutputTokens: 234_567, CachedTokens: 5_678_901, TotalTokens: 12_580_245,
			},
		},
		{
			model:  "gpt-5.6-terra",
			prices: [3]float64{2.5, 15, 0.25},
			tokens: tokenStats{
				InputTokens: 98_765_432, OutputTokens: 345_678, CachedTokens: 45_678_901, TotalTokens: 99_111_110,
			},
		},
		{
			model:  "gpt-5.6-luna",
			prices: [3]float64{1, 6, 0.1},
			tokens: tokenStats{
				InputTokens: 7_654_321, OutputTokens: 123_456, CachedTokens: 3_210_987, TotalTokens: 7_777_777,
			},
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
	if _, err := persister.recalculateCosts(); err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}

	rows, err := store.db.Query(
		`
		SELECT m.name, e.input_tokens, e.output_tokens, e.cached_tokens, e.total_tokens, e.cost_micro
		FROM events e
		JOIN dim_model m ON m.id = e.model_id
		ORDER BY e.id`,
	)
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

func TestDetailedHistoricalRepricingUsesStoredTokensAndEffectivePrice(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	cacheRead := 0.5
	cacheWrite := 3.0
	provider := &modelPriceProvider{
		cached: modelcatalog.File{
			Prices: map[string]modelcatalog.Price{
				"historical-model": {
					Prompt: 2, Completion: 10,
					CacheRead: &cacheRead, CacheWrite: &cacheWrite,
					Status:         modelcatalog.PriceStatusComplete,
					EffectiveFrom:  "2026-08-01T00:00:00Z",
					EffectiveUntil: "2026-09-01T00:00:00Z",
				},
			},
		},
		loadedAt: time.Now(),
	}
	breakdown := coreusage.NewSubsetTokenBreakdown(1_000, 200, 100, 300, 100, 1_300)
	canonicalTokens := tokenStats{
		InputTokens:      1_000,
		OutputTokens:     300,
		ReasoningTokens:  100,
		CachedTokens:     300,
		CacheReadTokens:  200,
		CacheWriteTokens: 100,
		TotalTokens:      1_300,
	}
	mismatchedTokens := tokenStats{
		InputTokens:      3,
		OutputTokens:     4,
		CachedTokens:     10,
		CacheReadTokens:  8,
		CacheWriteTokens: 2,
		TotalTokens:      7,
	}
	seedPrice := func(string) (float64, float64, float64, bool) { return 99, 99, 99, true }
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	seed := []FlatDetail{
		{
			Timestamp:         time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
			Model:             "historical-model",
			AccountingVersion: coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:    breakdown,
			Tokens:            mismatchedTokens,
			CostStatus:        costStatusComplete,
			costMicro:         4_800,
			costTrusted:       true,
		},
		{
			Timestamp:   time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
			Model:       "historical-model",
			Tokens:      canonicalTokens,
			CostStatus:  costStatusComplete,
			costMicro:   4_800,
			costTrusted: true,
		},
		{
			Timestamp:   time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
			Model:       "historical-model",
			Tokens:      mismatchedTokens,
			CostStatus:  costStatusComplete,
			costMicro:   56,
			costTrusted: true,
		},
		{
			Timestamp:         time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
			Model:             "historical-model",
			AccountingVersion: coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:    breakdown,
			Tokens:            mismatchedTokens,
			CostStatus:        costStatusComplete,
			costMicro:         4_800,
			costTrusted:       true,
		},
	}
	for i := range seed {
		if _, err = persistEvent(ctx, tx, store, &seed[i], seedPrice); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist seed event %d: %v", i, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	persister := &Persister{store: store, priceProvider: provider}
	if _, err = persister.recalculateCosts(); err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}

	rows, err := store.db.Query(
		`
		SELECT ts_ns, cost_micro, cost_status, cost_reason
		  FROM events
		 ORDER BY ts_ns`,
	)
	if err != nil {
		t.Fatalf("query repriced events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type result struct {
		cost   int64
		status CostStatus
		reason string
	}
	got := make(map[string]result)
	for rows.Next() {
		var tsNs int64
		var item result
		if err = rows.Scan(&tsNs, &item.cost, &item.status, &item.reason); err != nil {
			t.Fatalf("scan repriced event: %v", err)
		}
		got[time.Unix(0, tsNs).UTC().Format("2006-01-02")] = item
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("iterate repriced events: %v", err)
	}
	if item := got["2026-08-15"]; item.cost != 4_800 || item.status != costStatusComplete || item.reason != "" {
		t.Fatalf("in-range canonical event = %+v", item)
	}
	if item := got["2026-08-16"]; item.cost != 4_800 || item.status != costStatusPartial ||
		item.reason != "legacy cache overlap is ambiguous; applied subset assumption" {
		t.Fatalf("event without canonical metadata = %+v", item)
	}
	if item := got["2026-08-17"]; item.cost != 56 || item.status != costStatusPartial ||
		item.reason != "cached input tokens were recorded independently" {
		t.Fatalf("event with independently recorded explicit cache tokens = %+v", item)
	}
	if item := got["2026-07-15"]; item.cost != 0 || item.status != CostStatusUnpriced ||
		item.reason != "price is not effective at the event timestamp" {
		t.Fatalf("out-of-range event = %+v", item)
	}
	for _, table := range []string{"hour_bucket", "day_bucket"} {
		var completeRequests, partialRequests, unpricedRequests int64
		var completeTokens, partialTokens, unpricedTokens int64
		if err := store.db.QueryRow(
			"SELECT COALESCE(SUM(complete_requests), 0), COALESCE(SUM(partial_requests), 0), "+
				"COALESCE(SUM(unpriced_requests), 0), COALESCE(SUM(complete_tokens), 0), "+
				"COALESCE(SUM(partial_tokens), 0), COALESCE(SUM(unpriced_tokens), 0) FROM "+table,
		).Scan(
			&completeRequests, &partialRequests, &unpricedRequests,
			&completeTokens, &partialTokens, &unpricedTokens,
		); err != nil {
			t.Fatalf("query %s cost coverage: %v", table, err)
		}
		if completeRequests != 1 || partialRequests != 2 || unpricedRequests != 1 ||
			completeTokens != 7 || partialTokens != 1_307 || unpricedTokens != 7 {
			t.Fatalf(
				"%s cost coverage = requests:%d/%d/%d tokens:%d/%d/%d",
				table,
				completeRequests, partialRequests, unpricedRequests,
				completeTokens, partialTokens, unpricedTokens,
			)
		}
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
		Timestamp:         time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Model:             "m",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(1_000, 0, 0, 0, 0, 1_000),
		Tokens:            tokenStats{InputTokens: 1, TotalTokens: 1},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, priceFn); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx, `
		CREATE TABLE event_update_audit (id INTEGER PRIMARY KEY);
		CREATE TRIGGER audit_event_cost_update
		AFTER UPDATE OF cost_micro ON events
		BEGIN
		    INSERT INTO event_update_audit(id) VALUES(NULL);
		END;
	`,
	); err != nil {
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
	priceOne := func(string) (float64, float64, float64, bool) { return 1, 0, 0, true }
	if err := repriceModelEvents(ctx, tx, model, pricingSnapshot{priceFn: priceOne}); err != nil {
		t.Fatalf("reprice unchanged event: %v", err)
	}
	var updates int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_update_audit").Scan(&updates); err != nil {
		t.Fatalf("count unchanged updates: %v", err)
	}
	if updates != 0 {
		t.Fatalf("unchanged event updates = %d, want 0", updates)
	}

	priceTwo := func(string) (float64, float64, float64, bool) { return 2, 0, 0, true }
	if err := repriceModelEvents(ctx, tx, model, pricingSnapshot{priceFn: priceTwo}); err != nil {
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
	if _, err := persister.recalculateCosts(); err == nil ||
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
	if _, err := persister.recalculateCosts(); err != nil {
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

	persister := &Persister{store: store, pricesFn: priceFn}
	result, err := persister.recalculateCosts()
	if err != nil {
		t.Fatalf("RecalculateCosts: %v", err)
	}
	if result.RecalculatedDays != 1 {
		t.Fatalf("recalculated days = %d, want 1", result.RecalculatedDays)
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
	if latencyTotal != 120 || latencySamples != 4 {
		t.Fatalf("day latency = %d/%d, want 120/4", latencyTotal, latencySamples)
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
	_, err = persister.recalculateCosts()
	if err == nil || !strings.Contains(err.Error(), "roll up recalculated day") {
		t.Fatalf("RecalculateCosts error = %v, want day rollup overflow", err)
	}

	var dayCost, dayTokens, repricedEvents int64
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
		"SELECT COUNT(*) FROM events WHERE model_id = ? AND cost_micro > 0",
		modelID,
	).Scan(&repricedEvents); err != nil {
		t.Fatalf("count repriced events: %v", err)
	}
	if dayCost != 123 || dayTokens != 123 || repricedEvents != 2 {
		t.Fatalf(
			"page commits day_cost=%d day_tokens=%d repriced_events=%d, want 123/123/2",
			dayCost,
			dayTokens,
			repricedEvents,
		)
	}
	status := persister.RecalculateCostsStatus()
	if status.Status != "error" || !status.Resumable {
		t.Fatalf("recalculation status = %+v, want resumable error", status)
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
		t.Fatalf(
			"events = count:%d timestamp:%d tokens:%d, want 1/%d/2", eventCount, eventTimestamp, eventTokens,
			wantTimestamp,
		)
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

func TestTrimBeforeSkipsBucketRebuildWhenNothingExpires(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	detail := FlatDetail{
		Timestamp: base,
		Model:     "m",
		Tokens:    tokenStats{TotalTokens: 1},
	}
	if _, err = persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err = store.db.ExecContext(
		ctx, `
		CREATE TRIGGER fail_unnecessary_bucket_rebuild
		BEFORE INSERT ON hour_bucket
		BEGIN
			SELECT RAISE(ABORT, 'unexpected bucket rebuild');
		END
	`,
	); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	persister := &Persister{store: store}
	if err = persister.trimBefore(base.Add(-time.Hour)); err != nil {
		t.Fatalf("trimBefore with no expired events: %v", err)
	}
	var events, buckets int
	if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hour_bucket").Scan(&buckets); err != nil {
		t.Fatalf("count hour buckets: %v", err)
	}
	if events != 1 || buckets != 1 {
		t.Fatalf("rows after empty trim = %d events/%d buckets, want 1/1", events, buckets)
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
	if _, err := store.db.ExecContext(
		ctx, `
		CREATE TRIGGER fail_trim_bucket_rebuild
		BEFORE INSERT ON hour_bucket
		BEGIN
			SELECT RAISE(ABORT, 'forced rebuild failure');
		END
	`,
	); err != nil {
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
	if errWrite := os.WriteFile(
		filepath.Join(oldBaseDir, "detail", "day.json"), []byte("detail"), 0o600,
	); errWrite != nil {
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

func TestMaintenanceContextWithoutTimeoutUsesParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	persister := &Persister{}
	ctx, cancel := persister.maintenanceContext(parent, 0)
	defer cancel()

	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("maintenance context deadline = %v, want none", deadline)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("maintenance context ended before parent cancellation: %v", ctx.Err())
	default:
	}

	cancelParent()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("maintenance context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance context did not follow parent cancellation")
	}
}

func TestPersisterStopBeforeStartPreventsInitialization(t *testing.T) {
	baseDir := t.TempDir()
	persister := NewPersister(baseDir, config.UsageRetention{})
	persister.Stop()
	persister.Start(context.Background())

	if persister.IsReady() {
		t.Fatal("persister became ready after it was stopped before Start")
	}
	if _, err := os.Stat(filepath.Join(baseDir, dbFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database file after stopped Start = %v, want not exist", err)
	}
	request := apiKeyReservationRequest(
		time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC),
		"stop-before-start",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1},
	)
	if _, err := persister.ReserveAPIKey(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "reservation store is unavailable") {
		t.Fatalf("ReserveAPIKey error = %v, want unavailable", err)
	}
}

func TestPersisterStopWaitsForConcurrentStartCleanup(t *testing.T) {
	provider := &blockingPriceProvider{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	persister := NewPersister(t.TempDir(), config.UsageRetention{})
	persister.SetPriceProvider(provider)

	startDone := make(chan struct{})
	go func() {
		persister.Start(context.Background())
		close(startDone)
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not reach the blocked pricing snapshot")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	errStop := persister.StopContext(stopCtx)
	cancelStop()
	if !errors.Is(errStop, context.DeadlineExceeded) {
		close(provider.release)
		t.Fatalf("StopContext error = %v, want deadline exceeded while Start is blocked", errStop)
	}
	if persister.IsReady() {
		close(provider.release)
		t.Fatal("persister reported ready after shutdown was requested")
	}
	request := apiKeyReservationRequest(
		time.Date(2026, 9, 8, 8, 30, 0, 0, time.UTC),
		"stop-during-start",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1},
	)
	if _, err := persister.ReserveAPIKey(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "reservation store is unavailable") {
		close(provider.release)
		t.Fatalf("ReserveAPIKey error = %v, want unavailable", err)
	}

	secondStopDone := make(chan error, 1)
	go func() {
		secondStopDone <- persister.StopContext(context.Background())
	}()
	select {
	case err := <-secondStopDone:
		close(provider.release)
		t.Fatalf("second StopContext returned before Start cleanup could run: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(provider.release)
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the pricing snapshot was released")
	}
	select {
	case err := <-secondStopDone:
		if err != nil {
			t.Fatalf("second StopContext error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second StopContext did not wait for cleanup")
	}

	persister.mu.RLock()
	store := persister.store
	writer := persister.writer
	lifecycleCtx := persister.lifecycleCtx
	persister.mu.RUnlock()
	if store != nil || writer != nil {
		t.Fatalf("persister resources after Stop = store:%p writer:%p, want nil", store, writer)
	}
	if lifecycleCtx == nil {
		t.Fatal("Start did not publish a lifecycle context before cleanup")
	}
	select {
	case <-lifecycleCtx.Done():
	default:
		t.Fatal("writer lifecycle context remained active after Stop")
	}
	if persister.Record(FlatDetail{Timestamp: time.Now().UTC(), Model: "after-stop"}) {
		t.Fatal("Record accepted an event after Stop returned")
	}
	if _, err := persister.SchemaVersion(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "store not initialized") {
		t.Fatalf("SchemaVersion error = %v, want store not initialized", err)
	}
}

func TestPersisterAPIKeyReservationStoreUnavailableAcrossLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 8, 7, 30, 0, 0, time.UTC)
	request := apiKeyReservationRequest(
		now,
		"persister-unavailable",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1},
	)
	assertUnavailable := func(t *testing.T, persister *Persister) {
		t.Helper()
		if _, err := persister.ReserveAPIKey(context.Background(), request); err == nil ||
			!strings.Contains(err.Error(), "reservation store is unavailable") {
			t.Fatalf("ReserveAPIKey error = %v, want unavailable", err)
		}
		transitions := []struct {
			name string
			call func() error
		}{
			{
				name: "release",
				call: func() error {
					return persister.ReleaseAPIKeyReservation(context.Background(), "reservation", "test")
				},
			},
			{
				name: "unknown",
				call: func() error {
					return persister.MarkAPIKeyReservationUnknown(context.Background(), "reservation", "test")
				},
			},
			{
				name: "commit",
				call: func() error {
					return persister.CommitAPIKeyReservation(context.Background(), "reservation", "test")
				},
			},
		}
		for _, transition := range transitions {
			if err := transition.call(); err == nil || !strings.Contains(
				err.Error(),
				"reservation store is unavailable",
			) {
				t.Fatalf("%s error = %v, want unavailable", transition.name, err)
			}
		}
	}

	t.Run(
		"nil", func(t *testing.T) {
			var persister *Persister
			assertUnavailable(t, persister)
		},
	)
	t.Run(
		"not started", func(t *testing.T) {
			assertUnavailable(t, NewPersister(t.TempDir(), config.UsageRetention{}))
		},
	)
	t.Run(
		"start failed", func(t *testing.T) {
			baseDir := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(baseDir, []byte("file"), 0o600); err != nil {
				t.Fatalf("write blocking file: %v", err)
			}
			persister := NewPersister(baseDir, config.UsageRetention{})
			persister.Start(context.Background())
			if persister.IsReady() {
				t.Fatal("persister became ready after start failure")
			}
			assertUnavailable(t, persister)
			persister.Stop()
		},
	)
	t.Run(
		"stopped", func(t *testing.T) {
			persister := NewPersister(t.TempDir(), config.UsageRetention{})
			persister.Start(context.Background())
			if !persister.IsReady() {
				t.Fatal("persister did not start")
			}
			persister.Stop()
			if persister.IsReady() {
				t.Fatal("persister remained ready after stop")
			}
			assertUnavailable(t, persister)
		},
	)
}

func TestPersisterWriterSurvivesRunContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	persister := NewPersister(t.TempDir(), config.UsageRetention{})
	persister.Start(ctx)
	if !persister.IsReady() {
		t.Fatal("persister did not start")
	}
	cancel()
	persister.Record(
		FlatDetail{
			Timestamp: time.Now().UTC(),
			Model:     "after-run-cancel",
			Tokens:    tokenStats{InputTokens: 1, TotalTokens: 1},
		},
	)

	deadline := time.Now().Add(time.Second)
	for {
		snapshot, err := persister.querySnapshot()
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
	persister.SetPriceFunc(
		func(string) (float64, float64, float64, bool) {
			return 1, 1, 0, true
		},
	)
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
	result, err := persister.recalculateCosts()
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
			t.Fatalf(
				"%s = bucket:%d latency:%d samples:%d, want %d/25/1", table, bucket, latencyTotal, latencySamples,
				wantBucket,
			)
		}
	}
}

func TestRecalculateCostsResumesOnlyCompatiblePriceRevision(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	configPath := filepath.Join(t.TempDir(), "models.json")
	writeCatalog := func(prompt float64) {
		t.Helper()
		if err := modelcatalog.Save(
			configPath, modelcatalog.File{
				Prices: map[string]modelcatalog.Price{
					"m": {Prompt: prompt, Status: modelcatalog.PriceStatusComplete},
				},
			},
		); err != nil {
			t.Fatalf("save catalog: %v", err)
		}
	}
	writeCatalog(1)

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	for index := 0; index <= repriceEventPageSize; index++ {
		detail := FlatDetail{
			Timestamp: time.Date(2026, 9, 1, 0, 0, index, 0, time.UTC),
			Model:     "m",
			Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist event %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	persister := &Persister{store: store, priceProvider: BuildModelPriceProvider(configPath)}
	pricing := persister.pricingSnapshotForRecalculation()
	task, err := persister.prepareCostRecalculationTask(ctx, store, pricing)
	if err != nil {
		t.Fatalf("prepare task: %v", err)
	}
	processed, err := persister.repriceCostRecalculationPage(ctx, store, pricing, &task)
	if err != nil || !processed {
		t.Fatalf("reprice first page = %t, %v", processed, err)
	}
	if task.ProcessedEvents != repriceEventPageSize || task.CursorEventID == 0 {
		t.Fatalf("first page progress = %+v", task)
	}
	task.Status = "interrupted"
	task.UpdatedAt = costTaskNow()
	if err := writeCostRecalculationTask(ctx, store.db, task); err != nil {
		t.Fatalf("persist interrupted task: %v", err)
	}

	if _, err := persister.recalculateCosts(); err != nil {
		t.Fatalf("resume compatible task: %v", err)
	}
	status := persister.RecalculateCostsStatus()
	if !status.Resumed || status.ProcessedEvents != int64(repriceEventPageSize+1) || status.Percent != 100 {
		t.Fatalf("compatible resume status = %+v", status)
	}

	writeCatalog(2)
	persister.SetPriceProvider(BuildModelPriceProvider(configPath))
	task.Status = "interrupted"
	task.UpdatedAt = costTaskNow()
	if err := writeCostRecalculationTask(ctx, store.db, task); err != nil {
		t.Fatalf("persist stale interrupted task: %v", err)
	}
	result, err := persister.recalculateCosts()
	if err != nil {
		t.Fatalf("restart changed revision task: %v", err)
	}
	if result.TotalCost != float64((repriceEventPageSize+1)*2000)/costMicroPerUSD {
		t.Fatalf("changed revision total cost = %v", result.TotalCost)
	}
	status = persister.RecalculateCostsStatus()
	if status.Resumed || status.RestartReason == "" || status.ProcessedEvents != int64(repriceEventPageSize+1) {
		t.Fatalf("changed revision status = %+v", status)
	}
}

func TestRecalculateCostsRollsBackPageOnMalformedV2Metadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedPrice := func(string) (float64, float64, float64, bool) { return 1, 0, 0, true }
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	timestamps := []time.Time{
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 0, 1, 0, 0, time.UTC),
	}
	for _, timestamp := range timestamps {
		detail := FlatDetail{
			Timestamp: timestamp,
			Model:     "m",
			Tokens:    tokenStats{InputTokens: 1_000, TotalTokens: 1_000},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, seedPrice); err != nil {
			_ = tx.Rollback()
			t.Fatalf("persist seed event: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		"UPDATE events SET metadata = ? WHERE ts_ns = ?",
		`{"accounting_version":2,"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":1}}`,
		timestamps[1].UnixNano(),
	); err != nil {
		t.Fatalf("set malformed v2 metadata: %v", err)
	}

	persister := &Persister{
		store: store, pricesFn: func(string) (float64, float64, float64, bool) {
			return 2, 0, 0, true
		},
	}
	if _, err := persister.recalculateCosts(); err == nil {
		t.Fatal("RecalculateCosts accepted malformed v2 metadata")
	}
	var firstCost int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT cost_micro FROM events WHERE ts_ns = ?",
		timestamps[0].UnixNano(),
	).Scan(&firstCost); err != nil {
		t.Fatalf("read first event cost: %v", err)
	}
	if firstCost != 1_000 {
		t.Fatalf("first event cost = %d, want page rollback to preserve 1000", firstCost)
	}
	if status := persister.RecalculateCostsStatus(); status.Status != "error" || status.Running {
		t.Fatalf("recalculation status = %+v", status)
	}
}

func TestRepriceCostRecalculationPageRetriesLockedDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	initialized, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	if err := initialized.Close(); err != nil {
		t.Fatalf("close initialized store: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(20)")
	if err != nil {
		t.Fatalf("open recalculation database: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	detail := FlatDetail{
		Timestamp: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		Model:     "m",
		Tokens:    tokenStats{InputTokens: 1000, TotalTokens: 1000},
	}
	if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist event: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	persister := &Persister{store: store}
	var priceLookups atomic.Int32
	pricing := pricingSnapshot{
		priceFn: func(string) (float64, float64, float64, bool) {
			priceLookups.Add(1)
			return 1, 0, 0, true
		},
	}
	task, err := persister.prepareCostRecalculationTask(ctx, store, pricing)
	if err != nil {
		t.Fatalf("prepare task: %v", err)
	}

	lockerDB, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(20)")
	if err != nil {
		t.Fatalf("open locker database: %v", err)
	}
	defer func() { _ = lockerDB.Close() }()
	locker, err := lockerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("open locker connection: %v", err)
	}
	defer func() { _ = locker.Close() }()
	if _, err := locker.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = locker.ExecContext(ctx, "ROLLBACK")
		}
	}()

	type repriceResult struct {
		processed bool
		err       error
	}
	done := make(chan repriceResult, 1)
	go func() {
		processed, err := persister.repriceCostRecalculationPageWithRetry(ctx, store, pricing, &task)
		done <- repriceResult{processed: processed, err: err}
	}()
	select {
	case result := <-done:
		t.Fatalf("reprice returned while write lock was held: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := locker.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("release write lock: %v", err)
	}
	locked = false

	select {
	case result := <-done:
		if result.err != nil || !result.processed {
			t.Fatalf("retry result = %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reprice did not complete after write lock was released")
	}
	if priceLookups.Load() < 2 {
		t.Fatalf("price lookups = %d, want retry after write lock", priceLookups.Load())
	}
	if task.ProcessedEvents != 1 || task.CursorEventID == 0 || task.ChangedEvents != 1 {
		t.Fatalf("reprice progress = %+v", task)
	}
	var costMicro int64
	if err := db.QueryRowContext(ctx, "SELECT cost_micro FROM events").Scan(&costMicro); err != nil {
		t.Fatalf("read recalculated cost: %v", err)
	}
	if costMicro != 1000 {
		t.Fatalf("recalculated cost = %d, want 1000", costMicro)
	}
}
