package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	_ "modernc.org/sqlite"
)

type detailedPriceSnapshotFixture struct {
	price    detailedPrice
	revision string
}

type scriptedDetailedPriceProvider struct {
	mu        sync.Mutex
	snapshots []detailedPriceSnapshotFixture
	calls     int
}

func (*scriptedDetailedPriceProvider) Snapshot() PriceFunc {
	return nil
}

func (p *scriptedDetailedPriceProvider) DetailedSnapshotWithRevision() (detailedPriceFunc, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.snapshots) == 0 {
		return nil, ""
	}
	index := p.calls
	if index >= len(p.snapshots) {
		index = len(p.snapshots) - 1
	}
	p.calls++
	snapshot := p.snapshots[index]
	return func(string, time.Time) (detailedPrice, bool) {
		return snapshot.price, true
	}, snapshot.revision
}

func (p *scriptedDetailedPriceProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func completeDetailedPrice(prompt, completion float64) detailedPrice {
	return detailedPrice{
		CanonicalModel: "canonical-model",
		Currency:       "USD",
		Source:         "test-catalog",
		EffectiveFrom:  "2026-09-01T00:00:00Z",
		EffectiveUntil: "2026-10-01T00:00:00Z",
		Prompt:         prompt,
		Completion:     completion,
		CacheRead:      new(0.2),
		CacheWrite:     new(float64(3)),
		CacheWrite5m:   new(2.5),
		CacheWrite1h:   new(float64(4)),
		Status:         costStatusComplete,
		Automatic:      true,
		Effective:      true,
		Reason:         "catalog limitation",
	}
}

func detailedPricingSnapshot(price detailedPrice, revision string) pricingSnapshot {
	return pricingSnapshot{
		revision: revision,
		detailedPriceFn: func(string, time.Time) (detailedPrice, bool) {
			return price, true
		},
	}
}

func TestBuildPricingReceiptCapturesAllCategoriesAndMetadata(t *testing.T) {
	pricedAt := time.Date(2026, 9, 9, 1, 2, 3, 0, time.FixedZone("request", 8*60*60))
	capturedAt := time.Date(2026, 9, 9, 1, 2, 4, 0, time.UTC)
	breakdown := coreusage.TokenBreakdown{
		SchemaVersion: coreusage.TokenAccountingSchemaVersion,
		Quality:       coreusage.TokenAccountingQualityUnclassified,
		TotalTokens:   180,
		Input: coreusage.TokenInputBreakdown{
			TotalTokens:      83,
			UncachedTokens:   11,
			CacheReadTokens:  13,
			CacheWriteTokens: 59,
		},
		Output: coreusage.TokenOutputBreakdown{
			TotalTokens:        60,
			NonReasoningTokens: 29,
			ReasoningTokens:    31,
		},
		UnclassifiedTokens: 37,
	}
	detail := FlatDetail{
		Timestamp:         pricedAt,
		Model:             "model-alias",
		Provider:          "anthropic",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    breakdown,
		Tokens: tokenStats{
			InputTokens:      83,
			OutputTokens:     60,
			ReasoningTokens:  31,
			CacheReadTokens:  13,
			CacheWriteTokens: 59,
			TotalTokens:      180,
		},
		cacheWrite5mTokens: 17,
		cacheWrite1hTokens: 19,
	}

	cost, receipt, err := buildPricingReceipt(
		&detail,
		detailedPricingSnapshot(completeDetailedPrice(1.1, 5), "revision-1"),
		capturedAt,
	)
	if err != nil {
		t.Fatalf("buildPricingReceipt: %v", err)
	}
	if cost.Micro != 502 || cost.Status != costStatusPartial {
		t.Fatalf("cost = %+v, want 502 micro partial", cost)
	}
	if receipt.SchemaVersion != PricingReceiptSchemaVersion ||
		!receipt.CapturedAt.Equal(capturedAt) ||
		!receipt.PricedAt.Equal(pricedAt.UTC()) ||
		receipt.Provider != "anthropic" ||
		receipt.Model != "model-alias" ||
		receipt.CanonicalModel != "canonical-model" ||
		receipt.PriceRevision != "revision-1" ||
		receipt.Source != "test-catalog" ||
		receipt.Currency != "USD" ||
		receipt.EffectiveFrom != "2026-09-01T00:00:00Z" ||
		receipt.EffectiveUntil != "2026-10-01T00:00:00Z" {
		t.Fatalf("receipt metadata = %#v", receipt)
	}
	wantCoverage := pricingCoverage{
		UncachedInputTokens:     11,
		CacheReadTokens:         13,
		CacheWrite5mTokens:      17,
		CacheWrite1hTokens:      19,
		CacheWriteUnknownTokens: 23,
		OutputTokens:            29,
		ReasoningTokens:         31,
		UnclassifiedTokens:      37,
		PricedTokens:            143,
		UnpricedTokens:          37,
		TotalTokens:             180,
	}
	if receipt.Coverage != wantCoverage || receipt.CostMicro != 502 ||
		receipt.CostStatus != costStatusPartial ||
		!strings.Contains(receipt.CostReason, "unclassified") ||
		!strings.Contains(receipt.CostReason, "catalog limitation") {
		t.Fatalf("receipt cost and coverage = %#v", receipt)
	}

	wantItems := map[pricingLineItemCategory]struct {
		tokens     int64
		rateSource string
		cost       int64
		priced     bool
	}{
		pricingLineItemUncachedInput:     {tokens: 11, rateSource: "prompt", cost: 12, priced: true},
		pricingLineItemCacheRead:         {tokens: 13, rateSource: "cache_read", cost: 3, priced: true},
		pricingLineItemCacheWrite5m:      {tokens: 17, rateSource: "cache_write_5m", cost: 42, priced: true},
		pricingLineItemCacheWrite1h:      {tokens: 19, rateSource: "cache_write_1h", cost: 76, priced: true},
		pricingLineItemCacheWriteUnknown: {tokens: 23, rateSource: "cache_write", cost: 69, priced: true},
		pricingLineItemOutput:            {tokens: 29, rateSource: "completion", cost: 145, priced: true},
		pricingLineItemReasoning:         {tokens: 31, rateSource: "completion", cost: 155, priced: true},
		pricingLineItemUnclassified:      {tokens: 37, rateSource: "unclassified", priced: false},
	}
	if len(receipt.LineItems) != len(wantItems) {
		t.Fatalf("line item count = %d, want %d", len(receipt.LineItems), len(wantItems))
	}
	var lineCost int64
	for _, item := range receipt.LineItems {
		want, ok := wantItems[item.Category]
		if !ok || item.Tokens != want.tokens || item.RateSource != want.rateSource ||
			item.CostMicro != want.cost || item.Priced != want.priced {
			t.Fatalf("line item = %#v, want %#v", item, want)
		}
		if item.Priced != (item.RatePerMillion != nil) {
			t.Fatalf("line item priced/rate mismatch: %#v", item)
		}
		lineCost += item.CostMicro
	}
	if lineCost != receipt.CostMicro {
		t.Fatalf("line item cost = %d, receipt cost = %d", lineCost, receipt.CostMicro)
	}
}

func TestBuildPricingReceiptSelectsCacheWriteRates(t *testing.T) {
	tests := []struct {
		name       string
		category   pricingLineItemCategory
		price      detailedPrice
		wantSource string
		wantRate   float64
		wantCost   int64
	}{
		{
			name:       "unknown TTL uses generic rate",
			category:   pricingLineItemCacheWriteUnknown,
			price:      completeDetailedPrice(1, 2),
			wantSource: "cache_write",
			wantRate:   3,
			wantCost:   6,
		},
		{
			name:     "unknown TTL uses highest configured TTL rate",
			category: pricingLineItemCacheWriteUnknown,
			price: func() detailedPrice {
				price := completeDetailedPrice(1, 2)
				price.CacheWrite = nil
				return price
			}(),
			wantSource: "max_ttl_fallback",
			wantRate:   4,
			wantCost:   8,
		},
		{
			name:     "unknown TTL uses prompt rate when write rates are absent",
			category: pricingLineItemCacheWriteUnknown,
			price: func() detailedPrice {
				price := completeDetailedPrice(1, 2)
				price.CacheWrite = nil
				price.CacheWrite5m = nil
				price.CacheWrite1h = nil
				return price
			}(),
			wantSource: "prompt_fallback",
			wantRate:   1,
			wantCost:   2,
		},
		{
			name:     "5-minute write uses generic rate when its rate is absent",
			category: pricingLineItemCacheWrite5m,
			price: func() detailedPrice {
				price := completeDetailedPrice(1, 2)
				price.CacheWrite5m = nil
				return price
			}(),
			wantSource: "cache_write",
			wantRate:   3,
			wantCost:   6,
		},
		{
			name:       "1-hour write uses its specific rate",
			category:   pricingLineItemCacheWrite1h,
			price:      completeDetailedPrice(1, 2),
			wantSource: "cache_write_1h",
			wantRate:   4,
			wantCost:   8,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				detail := FlatDetail{
					Timestamp:         time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
					Model:             "model",
					Provider:          "provider",
					AccountingVersion: coreusage.TokenAccountingSchemaVersion,
					TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(0, 0, 2, 0, 0, 2),
					Tokens: tokenStats{
						CacheWriteTokens: 2,
						TotalTokens:      2,
					},
				}
				switch test.category {
				case pricingLineItemCacheWrite5m:
					detail.cacheWrite5mTokens = 2
				case pricingLineItemCacheWrite1h:
					detail.cacheWrite1hTokens = 2
				}
				_, receipt, err := buildPricingReceipt(
					&detail,
					detailedPricingSnapshot(test.price, "revision"),
					time.Date(2026, 9, 9, 0, 0, 1, 0, time.UTC),
				)
				if err != nil {
					t.Fatalf("buildPricingReceipt: %v", err)
				}
				if len(receipt.LineItems) != 1 {
					t.Fatalf("line items = %#v, want one", receipt.LineItems)
				}
				item := receipt.LineItems[0]
				if item.Category != test.category || item.RateSource != test.wantSource ||
					item.RatePerMillion == nil || *item.RatePerMillion != test.wantRate ||
					item.CostMicro != test.wantCost || receipt.CostMicro != test.wantCost {
					t.Fatalf("line item/receipt = %#v/%#v", item, receipt)
				}
			},
		)
	}
}

func TestBuildPricingReceiptPreservesUnpricedEvidence(t *testing.T) {
	detail := FlatDetail{
		Timestamp:         time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Model:             "missing-model",
		Provider:          "provider",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(1, 1, 1, 1, 1, 5),
		Tokens: tokenStats{
			InputTokens:      1,
			OutputTokens:     1,
			ReasoningTokens:  1,
			CacheReadTokens:  1,
			CacheWriteTokens: 1,
			TotalTokens:      5,
		},
		cacheWrite5mTokens: 1,
	}
	cost, receipt, err := buildPricingReceipt(
		&detail,
		pricingSnapshot{revision: "missing-revision"},
		time.Date(2026, 9, 9, 0, 0, 1, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("buildPricingReceipt: %v", err)
	}
	if cost.Micro != 0 || cost.Status != CostStatusUnpriced ||
		receipt.Source != "unconfigured" || receipt.Currency != "USD" ||
		receipt.PriceRevision != "missing-revision" ||
		receipt.Coverage.PricedTokens != 0 || receipt.Coverage.UnpricedTokens != 5 ||
		!strings.Contains(receipt.CostReason, "not configured") {
		t.Fatalf("unpriced receipt = %#v", receipt)
	}
	for _, item := range receipt.LineItems {
		if item.Priced || item.RatePerMillion != nil || item.CostMicro != 0 || item.RateSource != "unpriced" {
			t.Fatalf("unpriced line item = %#v", item)
		}
	}

	unsupportedCurrency := completeDetailedPrice(1, 2)
	unsupportedCurrency.Currency = "EUR"
	cost, receipt, err = buildPricingReceipt(
		&detail,
		detailedPricingSnapshot(unsupportedCurrency, "eur-revision"),
		time.Date(2026, 9, 9, 0, 0, 2, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("build unsupported-currency pricing receipt: %v", err)
	}
	if cost.Micro != 0 || cost.Status != CostStatusUnpriced || receipt.Currency != "EUR" ||
		receipt.Coverage.PricedTokens != 0 || receipt.Coverage.UnpricedTokens != 5 ||
		!strings.Contains(receipt.CostReason, `pricing currency "EUR" is not supported`) {
		t.Fatalf("unsupported-currency receipt = %#v", receipt)
	}
	for _, item := range receipt.LineItems {
		if item.Priced || item.RatePerMillion != nil || item.CostMicro != 0 || item.RateSource != "unpriced" {
			t.Fatalf("unsupported-currency line item = %#v", item)
		}
	}

	inconsistent := detail
	inconsistent.TokenBreakdown = coreusage.TokenBreakdown{
		SchemaVersion:      coreusage.TokenAccountingSchemaVersion,
		Quality:            coreusage.TokenAccountingQualityInconsistent,
		TotalTokens:        5,
		UnclassifiedTokens: 5,
	}
	inconsistent.cacheWrite5mTokens = 0
	_, receipt, err = buildPricingReceipt(
		&inconsistent,
		detailedPricingSnapshot(completeDetailedPrice(1, 2), "configured-revision"),
		time.Date(2026, 9, 9, 0, 0, 2, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("build inconsistent pricing receipt: %v", err)
	}
	if receipt.Source != "test-catalog" || receipt.CostStatus != CostStatusUnpriced ||
		receipt.Coverage.UnclassifiedTokens != 5 || len(receipt.LineItems) != 1 ||
		receipt.LineItems[0].Category != pricingLineItemUnclassified ||
		!strings.Contains(receipt.CostReason, "inconsistent") {
		t.Fatalf("inconsistent receipt = %#v", receipt)
	}
}

func TestPricingReceiptEncodingCloningAndValidation(t *testing.T) {
	detail := FlatDetail{
		Timestamp:         time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Model:             "model",
		Provider:          "provider",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(2, 0, 0, 0, 0, 2),
		Tokens:            tokenStats{InputTokens: 2, TotalTokens: 2},
	}
	_, receipt, err := buildPricingReceipt(
		&detail,
		detailedPricingSnapshot(completeDetailedPrice(1, 2), "revision"),
		time.Date(2026, 9, 9, 0, 0, 1, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("buildPricingReceipt: %v", err)
	}
	encoded, err := encodePricingReceipt(receipt)
	if err != nil {
		t.Fatalf("encodePricingReceipt: %v", err)
	}
	encodedString, ok := encoded.(string)
	if !ok {
		t.Fatalf("encoded receipt type = %T, want string", encoded)
	}
	decoded, err := decodePricingReceipt(sql.NullString{String: encodedString, Valid: true})
	if err != nil {
		t.Fatalf("decodePricingReceipt: %v", err)
	}
	if !reflect.DeepEqual(decoded, receipt) {
		t.Fatalf("receipt round trip changed value\nwant=%#v\ngot=%#v", receipt, decoded)
	}

	cloned := clonePricingReceipt(receipt)
	*cloned.LineItems[0].RatePerMillion = 99
	if *receipt.LineItems[0].RatePerMillion == 99 {
		t.Fatal("clonePricingReceipt shared a line-item rate pointer")
	}

	tampered := clonePricingReceipt(receipt)
	tampered.CostMicro++
	tamperedJSON, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal tampered receipt: %v", err)
	}
	if _, err = decodePricingReceipt(sql.NullString{String: string(tamperedJSON), Valid: true}); err == nil ||
		!strings.Contains(err.Error(), "do not reconcile") {
		t.Fatalf("decode tampered receipt error = %v", err)
	}
	if _, err = decodePricingReceipt(sql.NullString{String: "{", Valid: true}); err == nil ||
		!strings.Contains(err.Error(), "decode pricing receipt") {
		t.Fatalf("decode malformed receipt error = %v", err)
	}
	if _, err = decodePricingReceipt(
		sql.NullString{
			String: strings.Repeat("x", maxPricingReceiptBytes+1), Valid: true,
		},
	); err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("decode oversized receipt error = %v", err)
	}

	large := &PricingReceipt{
		SchemaVersion:  PricingReceiptSchemaVersion,
		CapturedAt:     time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		PricedAt:       time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Provider:       strings.Repeat("P", maxPricingReceiptStringBytes),
		Model:          strings.Repeat("M", maxPricingReceiptStringBytes),
		CanonicalModel: strings.Repeat("C", maxPricingReceiptStringBytes),
		PriceRevision:  strings.Repeat("R", maxPricingReceiptStringBytes),
		Source:         strings.Repeat("S", maxPricingReceiptStringBytes),
		Currency:       "USD",
		EffectiveFrom:  strings.Repeat("F", maxPricingReceiptStringBytes),
		EffectiveUntil: strings.Repeat("U", maxPricingReceiptStringBytes),
		CostStatus:     costStatusComplete,
		CostReason:     strings.Repeat("E", maxPricingReceiptReasonBytes),
		LineItems:      []pricingLineItem{},
	}
	if _, err = encodePricingReceipt(large); err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("encode oversized receipt error = %v", err)
	}
}

func TestWriterCapturesPricingReceiptBeforeQueueFlush(t *testing.T) {
	firstPrice := completeDetailedPrice(1, 2)
	firstPrice.CanonicalModel = "canonical-v1"
	firstPrice.Source = "catalog-v1"
	secondPrice := completeDetailedPrice(9, 10)
	secondPrice.CanonicalModel = "canonical-v2"
	secondPrice.Source = "catalog-v2"
	provider := &scriptedDetailedPriceProvider{
		snapshots: []detailedPriceSnapshotFixture{
			{price: firstPrice, revision: "revision-v1"},
			{price: secondPrice, revision: "revision-v2"},
		},
	}
	store := openTestStore(t)
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	detail := FlatDetail{
		Timestamp:         time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Model:             "model-alias",
		Provider:          "provider",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(10, 0, 0, 0, 0, 10),
		Tokens:            tokenStats{InputTokens: 10, TotalTokens: 10},
	}
	if !writer.submit(detail) {
		t.Fatal("Submit rejected a valid usage record")
	}
	writer.start(context.Background())
	writer.Stop()
	if provider.CallCount() < 2 {
		t.Fatalf("pricing snapshot calls = %d, want request-time and flush snapshots", provider.CallCount())
	}

	events, total, err := store.queryEventDetails(
		context.Background(),
		detail.Timestamp.Add(-time.Minute),
		detail.Timestamp.Add(time.Minute),
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
		t.Fatalf("events = %d/%d, want 1/1", total, len(events))
	}
	got := events[0]
	if got.PricingReceipt == nil || got.PricingReceipt.PriceRevision != "revision-v1" ||
		got.PricingReceipt.CanonicalModel != "canonical-v1" ||
		got.PricingReceipt.Source != "catalog-v1" ||
		got.PricingReceipt.CostMicro != 10 || got.costMicro != 10 {
		t.Fatalf("persisted request-time pricing = %#v cost=%d", got.PricingReceipt, got.costMicro)
	}
	compactEvents, compactTotal, err := store.queryCompactEventDetails(
		context.Background(),
		detail.Timestamp.Add(-time.Minute),
		detail.Timestamp.Add(time.Minute),
		EventFilters{},
		1,
		10,
		"timestamp",
		false,
	)
	if err != nil {
		t.Fatalf("QueryCompactEvents: %v", err)
	}
	backupEvents, err := store.queryBackupEventSet(
		context.Background(),
		detail.Timestamp.Add(-time.Minute),
		detail.Timestamp.Add(time.Minute),
		EventFilters{},
	)
	if err != nil {
		t.Fatalf("QueryBackupEvents: %v", err)
	}
	if compactTotal != 1 || len(compactEvents) != 1 || len(backupEvents) != 1 ||
		!reflect.DeepEqual(compactEvents[0].PricingReceipt, got.PricingReceipt) ||
		!reflect.DeepEqual(backupEvents[0].PricingReceipt, got.PricingReceipt) {
		t.Fatalf(
			"pricing receipt query mismatch: full=%#v compact=%#v backup=%#v",
			got.PricingReceipt,
			compactEvents,
			backupEvents,
		)
	}

	badPrice := completeDetailedPrice(math.NaN(), 2)
	badProvider := &scriptedDetailedPriceProvider{
		snapshots: []detailedPriceSnapshotFixture{
			{price: badPrice, revision: "invalid-revision"},
		},
	}
	badWriter := newWriterWithPriceProviderAndGate(store, badProvider, nil, nil)
	if badWriter.submit(detail) {
		t.Fatal("Submit accepted invalid request-time pricing data")
	}
	if stats := badWriter.stats(); stats != (writerStats{}) {
		t.Fatalf("invalid pre-accept record changed writer stats: %#v", stats)
	}
}

func TestRepriceModelEventsChangesDerivedCostWithoutChangingReceipt(t *testing.T) {
	firstPrice := completeDetailedPrice(1, 2)
	provider := &scriptedDetailedPriceProvider{
		snapshots: []detailedPriceSnapshotFixture{
			{price: firstPrice, revision: "request-revision"},
		},
	}
	store := openTestStore(t)
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	writer.start(context.Background())
	detail := FlatDetail{
		Timestamp:         time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Model:             "model",
		Provider:          "provider",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(1_000, 0, 0, 0, 0, 1_000),
		Tokens:            tokenStats{InputTokens: 1_000, TotalTokens: 1_000},
	}
	if !writer.submit(detail) {
		t.Fatal("Submit rejected usage record")
	}
	writer.Stop()

	var beforeCost int64
	var beforeReceipt string
	if err := store.db.QueryRow("SELECT cost_micro, pricing_receipt FROM events").Scan(
		&beforeCost,
		&beforeReceipt,
	); err != nil {
		t.Fatalf("query request-time cost: %v", err)
	}
	if beforeCost != 1_000 || beforeReceipt == "" {
		t.Fatalf("request-time cost/receipt = %d/%q", beforeCost, beforeReceipt)
	}
	var model modelRow
	if err := store.db.QueryRow("SELECT id, name FROM dim_model WHERE name = ?", detail.Model).Scan(
		&model.id,
		&model.name,
	); err != nil {
		t.Fatalf("query model: %v", err)
	}
	secondPrice := completeDetailedPrice(2, 4)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin reprice: %v", err)
	}
	if err = repriceModelEvents(
		context.Background(),
		tx,
		model,
		detailedPricingSnapshot(secondPrice, "current-revision"),
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("repriceModelEvents: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit reprice: %v", err)
	}

	var afterCost int64
	var afterReceipt string
	if err = store.db.QueryRow("SELECT cost_micro, pricing_receipt FROM events").Scan(
		&afterCost,
		&afterReceipt,
	); err != nil {
		t.Fatalf("query repriced event: %v", err)
	}
	if afterCost != 2_000 || afterReceipt != beforeReceipt {
		t.Fatalf("repriced cost/receipt = %d/%q, want 2000 and unchanged receipt", afterCost, afterReceipt)
	}
	if _, err = store.db.Exec("UPDATE events SET pricing_receipt = NULL"); err == nil ||
		!strings.Contains(err.Error(), "pricing_receipt is immutable") {
		t.Fatalf("pricing receipt update error = %v", err)
	}
}

func TestOpenStoreMigratesV8PricingReceiptSchemaIdempotently(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("create current database: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close current database: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database for v8 fixture: %v", err)
	}
	if _, err = raw.Exec(
		`
		DROP TRIGGER IF EXISTS usage_events_pricing_receipt_immutable;
		ALTER TABLE events DROP COLUMN pricing_receipt;
		UPDATE meta SET value = '8' WHERE key = 'schema_version';
	`,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("prepare v8 database: %v", err)
	}
	if err = raw.Close(); err != nil {
		t.Fatalf("close v8 database: %v", err)
	}

	for attempt := range 2 {
		migrated, errOpen := openStore(dbPath)
		if errOpen != nil {
			t.Fatalf("OpenStore v8 database attempt %d: %v", attempt+1, errOpen)
		}
		var version string
		if errQuery := migrated.db.QueryRow(
			"SELECT value FROM meta WHERE key = 'schema_version'",
		).Scan(&version); errQuery != nil {
			_ = migrated.Close()
			t.Fatalf("query schema version: %v", errQuery)
		}
		if version != CurrentSchemaVersion {
			_ = migrated.Close()
			t.Fatalf("schema version = %q, want %q", version, CurrentSchemaVersion)
		}
		rows, errQuery := migrated.db.Query("PRAGMA table_info(events)")
		if errQuery != nil {
			_ = migrated.Close()
			t.Fatalf("query events columns: %v", errQuery)
		}
		foundReceipt := false
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if errScan := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); errScan != nil {
				_ = rows.Close()
				_ = migrated.Close()
				t.Fatalf("scan events column: %v", errScan)
			}
			if name == "pricing_receipt" {
				foundReceipt = true
			}
		}
		if errRows := rows.Close(); errRows != nil {
			_ = migrated.Close()
			t.Fatalf("close events columns: %v", errRows)
		}
		var triggerCount int
		if errQuery = migrated.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'usage_events_pricing_receipt_immutable'",
		).Scan(&triggerCount); errQuery != nil {
			_ = migrated.Close()
			t.Fatalf("query pricing receipt trigger: %v", errQuery)
		}
		if !foundReceipt || triggerCount != 1 {
			_ = migrated.Close()
			t.Fatalf("pricing receipt column/trigger = %t/%d, want true/1", foundReceipt, triggerCount)
		}
		if errClose := migrated.Close(); errClose != nil {
			t.Fatalf("close migrated database: %v", errClose)
		}
	}
}
