package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func conditionalPricingDetail(
	model, provider string,
	uncached, read, five, hour, thirty, twelve, output int64,
) FlatDetail {
	writes := five + hour + thirty + twelve
	breakdown := coreusage.NewIndependentTokenBreakdown(uncached, read, writes, output, 0, 0)
	input := breakdown.Input.TotalTokens
	return FlatDetail{
		Timestamp: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), Model: model, Provider: provider,
		ResponseServiceTier: "default", ResponseSpeed: "standard", CacheReadReported: true,
		CacheWriteReported: new(true),
		AccountingVersion:  coreusage.TokenAccountingSchemaVersion, TokenBreakdown: breakdown,
		Tokens: tokenStats{
			InputTokens: input, CacheReadTokens: read, CacheWriteTokens: writes, OutputTokens: output,
			TotalTokens: breakdown.TotalTokens,
		},
		cacheWrite5mTokens: five, cacheWrite1hTokens: hour, cacheWrite30mTokens: thirty, cacheWrite12hTokens: twelve,
		BillingMetadata: &coreusage.BillingMetadata{
			SchemaVersion: 1, ContextInputTokens: &input, Region: "global", RegionSource: "response",
			ChargesReported: true,
		},
	}
}

func defaultConditionalSnapshot() pricingSnapshot {
	return pricingSnapshot{
		detailedPriceFn: detailedPriceFuncFromCatalog(modelcatalog.DefaultFile()), revision: "conditional-defaults",
	}
}

func TestConditionalPublishedBoundaryAmounts(t *testing.T) {
	pricing := defaultConditionalSnapshot()
	for _, test := range []struct {
		name, model, provider         string
		uncached, read, output, micro int64
	}{
		{"GPT short includes cache once", "gpt-6.2-sol", "openai", 172000, 100000, 1000, 364000},
		{"GPT long includes cache once", "gpt-6.2-sol", "openai", 172001, 100000, 1000, 723004},
		{"Haiku at threshold", "claude-haiku-5-5", "claude", 1000, 99000, 1000, 1590},
		{"Haiku above threshold", "claude-haiku-5-5", "claude", 1001, 99000, 1000, 7950},
		{"Grok below threshold", "grok-4.7", "xai", 199999, 0, 1000, 405998},
		{"Grok inclusive threshold", "grok-4.7", "xai", 200000, 0, 1000, 812000},
		{"Ember at threshold", "muse-ember-1.0", "meta", 200000, 0, 1000, 20500},
		{"Ember above threshold", "muse-ember-1.0", "meta", 200001, 0, 1000, 61500},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				detail := conditionalPricingDetail(
					test.model, test.provider, test.uncached, test.read, 0, 0, 0, 0, test.output,
				)
				cost, receipt, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
				if err != nil || cost.Micro != test.micro || cost.Status != costStatusComplete {
					t.Fatalf("cost=%+v receipt=%+v err=%v, want %d complete", cost, receipt, err, test.micro)
				}
			},
		)
	}
}

func TestConditionalFourCacheTTLsAndUnknownRetention(t *testing.T) {
	pricing := defaultConditionalSnapshot()
	detail := conditionalPricingDetail("gpt-6.2-sol", "openai", 3000, 2000, 0, 0, 3000, 4000, 1000)
	cost, receipt, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Micro != 43700 || cost.Status != costStatusComplete || receipt.Coverage.TotalTokens != 13000 || receipt.SchemaVersion != 2 {
		t.Fatalf("30m/12h receipt=%+v cost=%+v err=%v", receipt, cost, err)
	}
	detail = conditionalPricingDetail("claude-sonnet-5-5", "claude", 1000, 2000, 3000, 4000, 0, 0, 1000)
	cost, receipt, err = buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Micro != 35700 || cost.Status != costStatusComplete || receipt.Coverage.TotalTokens != 11000 {
		t.Fatalf("5m/1h receipt=%+v cost=%+v err=%v", receipt, cost, err)
	}
	detail = conditionalPricingDetail("gpt-6.2-sol", "openai", 3000, 2000, 0, 0, 3000, 4000, 1000)
	detail.cacheWrite30mTokens, detail.cacheWrite12hTokens = 0, 0
	cost, receipt, err = buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Micro != 16200 || cost.Status != costStatusPartial || receipt.Coverage.UnpricedTokens != 7000 {
		t.Fatalf("unknown TTL=%+v/%+v/%v", cost, receipt, err)
	}
}

func TestConditionalFinalSpeedRatesAndUnpublishedCombination(t *testing.T) {
	detail := conditionalPricingDetail("gpt-5.4", "openai", 200000, 100000, 0, 0, 0, 0, 1000)
	detail.ResponseSpeed, detail.ResponseServiceTier = "batch", "batch"
	cost, _, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Micro != 536250 || cost.Status != costStatusComplete {
		t.Fatalf("exact long Batch cache row=%+v/%v", cost, err)
	}
	detail.ResponseSpeed, detail.ResponseServiceTier = "fast", "priority"
	cost, _, err = buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Micro != 0 || cost.Status != CostStatusUnpriced {
		t.Fatalf("unpublished Fast long row=%+v/%v", cost, err)
	}
}

func TestConditionalRequestCoverageMissingUsageAndFreeRates(t *testing.T) {
	pricing := defaultConditionalSnapshot()
	detail := conditionalPricingDetail("gpt-6.2-sol", "openai", 1000, 0, 0, 0, 0, 0, 1000)
	cost, _, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Status != costStatusComplete || cost.Micro != 12000 {
		t.Fatalf("unused TTL and tools=%+v/%v", cost, err)
	}
	detail.CacheReadReported = false
	cost, receipt, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Status != costStatusPartial || cost.Micro != 10000 || receipt.Coverage.UnpricedTokens != 1000 || !strings.Contains(
		cost.Reason, "cache-read usage",
	) {
		t.Fatalf("unknown cache usage=%+v/%+v/%v", cost, receipt, err)
	}
	detail = conditionalPricingDetail("glm-4.7-flash", "openai-compatibility", 1000, 0, 0, 0, 0, 0, 1000)
	cost, _, err = buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Status != costStatusComplete || cost.Micro != 0 {
		t.Fatalf("published free rate=%+v/%v", cost, err)
	}
	price := completeDetailedPrice(2, 10)
	price.Status, price.Reason = costStatusPartial, "custom unresolved contract"
	detail = conditionalPricingDetail("custom", "openai", 1000, 0, 0, 0, 0, 0, 1000)
	cost, _, err = buildPricingReceipt(&detail, detailedPricingSnapshot(price, "legacy"), detail.Timestamp)
	if err != nil || cost.Status != costStatusPartial || !strings.Contains(cost.Reason, "custom unresolved contract") {
		t.Fatalf("legacy custom partial=%+v/%v", cost, err)
	}
}

func TestConditionalLegacyCachePresenceEvidence(t *testing.T) {
	price := completeDetailedPrice(2, 10)
	price.CacheRead, price.RequestScoped, price.Region = new(0.2), true, ""
	price.Reason = ""
	pricing := detailedPricingSnapshot(price, "new-request-scoped-catalog")
	detail := conditionalPricingDetail("custom", "openai", 100, 0, 0, 0, 0, 0, 0)
	detail.BillingMetadata, detail.CacheReadReported = nil, false
	cost, receipt, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Status != CostStatusUnpriced || receipt.Coverage.UnpricedTokens != 100 || !strings.Contains(
		cost.Reason, "cache-read usage",
	) {
		t.Fatalf("old zero without presence=%+v/%+v/%v", cost, receipt, err)
	}
	detail = conditionalPricingDetail("custom", "openai", 80, 20, 0, 0, 0, 0, 0)
	detail.BillingMetadata, detail.CacheReadReported = nil, false
	cost, _, err = buildPricingReceipt(&detail, pricing, detail.Timestamp)
	if err != nil || cost.Status != costStatusComplete || cost.Micro != 164 {
		t.Fatalf("positive historical cache evidence=%+v/%v", cost, err)
	}
	price.RequestScoped = false
	detail = conditionalPricingDetail("custom", "openai", 100, 0, 0, 0, 0, 0, 0)
	detail.BillingMetadata, detail.CacheReadReported = nil, false
	cost, _, err = buildPricingReceipt(&detail, detailedPricingSnapshot(price, "legacy-policy"), detail.Timestamp)
	if err != nil || cost.Status != costStatusComplete || cost.Micro != 200 {
		t.Fatalf("old policy changed=%+v/%v", cost, err)
	}
}

func TestConditionalCacheWritePresenceAndPremium(t *testing.T) {
	for _, test := range []struct {
		name             string
		reported         *bool
		writes           int64
		legacy, sameRate bool
		micro            int64
		status           CostStatus
	}{
		{"legacy zero has no total evidence", nil, 0, false, false, 100, costStatusPartial},
		{"explicit unknown", new(false), 0, false, false, 100, costStatusPartial},
		{"explicit zero", new(true), 0, false, false, 300, costStatusComplete},
		{"legacy positive total", nil, 20, false, false, 350, costStatusComplete},
		{"partial TTL is not aggregate evidence", new(false), 20, false, false, 150, costStatusPartial},
		{"same write price", new(false), 0, false, true, 300, costStatusComplete},
		{"legacy contract", new(false), 0, true, false, 300, costStatusComplete},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				price := completeDetailedPrice(2, 10)
				price.RequestScoped, price.Reason = !test.legacy, ""
				price.CacheWrite, price.CacheWrite5m, price.CacheWrite1h = nil, nil, nil
				price.CacheWrite30m, price.CacheWrite12h = new(2.5), new(5.0)
				if test.sameRate {
					price.CacheWrite30m, price.CacheWrite12h = new(2.0), new(2.0)
				}
				detail := conditionalPricingDetail("custom", "openai", 100, 0, 0, 0, test.writes, 0, 10)
				detail.CacheWriteReported = test.reported
				pricing := detailedPricingSnapshot(price, "write-presence")
				cost, receipt, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
				if err != nil || cost.Micro != test.micro || cost.Status != test.status || (test.status == costStatusPartial && !strings.Contains(
					cost.Reason, "total cache-write usage",
				)) {
					t.Fatalf("write evidence=%+v/%+v/%v", cost, receipt, err)
				}
				if test.reported != nil && (receipt.CacheWriteReported == nil || *receipt.CacheWriteReported != *test.reported) {
					t.Fatal("receipt changed explicit write presence")
				}
				current, err := computeCurrentCatalogEventCost(
					detail.Tokens, receipt, pricing, detail.Model, detail.Provider, detail.Timestamp, sql.NullString{},
				)
				if err != nil || current != cost {
					t.Fatalf("write presence repricing=%+v/%v, want %+v", current, err, cost)
				}
			},
		)
	}
}

func TestConditionalCachePresenceUsesApplicableFinalRates(t *testing.T) {
	price := completeDetailedPrice(1, 10)
	price.CacheRead, price.RequestScoped, price.Reason = new(1.0), true, ""
	price.CacheWrite, price.CacheWrite5m, price.CacheWrite1h = nil, nil, nil
	price.ConditionalRates = []modelcatalog.ConditionalRate{
		{
			ID: "long", MinInputTokens: new(int64(100)), RateOverrides: modelcatalog.RateOverrides{
				Prompt: new(2.0), CacheRead: new(0.2), CacheWrite30m: new(2.0), CacheWrite12h: new(2.0),
			},
		},
	}
	detail := conditionalPricingDetail("custom", "openai", 100, 0, 0, 0, 0, 0, 10)
	detail.CacheReadReported, detail.CacheWriteReported = false, new(false)
	cost, _, err := buildPricingReceipt(&detail, detailedPricingSnapshot(price, "effective-presence"), detail.Timestamp)
	if err != nil || cost.Micro != 100 || cost.Status != costStatusPartial || !strings.Contains(
		cost.Reason, "cache-read usage",
	) || strings.Contains(cost.Reason, "cache-write usage") {
		t.Fatalf("effective cache prices=%+v/%v", cost, err)
	}
}

func TestConditionalRegionalAndAdditionalAmounts(t *testing.T) {
	detail := conditionalPricingDetail("claude-sonnet-5-5", "claude", 1000, 0, 0, 0, 0, 0, 1000)
	detail.BillingMetadata.Region = "us"
	detail.BillingMetadata.Charges = []coreusage.BillingQuantity{
		{Kind: "web_search", Unit: "request", Quantity: 1}, {Kind: "web_fetch", Unit: "request", Quantity: 1},
	}
	cost, receipt, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Micro != 23200 || cost.Status != costStatusComplete || len(receipt.AdditionalLineItems) != 2 {
		t.Fatalf("regional tokens plus tool fee=%+v/%+v/%v", cost, receipt, err)
	}
	if !receipt.AdditionalLineItems[1].Priced || *receipt.AdditionalLineItems[1].Price != 0 {
		t.Fatal("published free web fetch was lost")
	}
	detail.BillingMetadata.Charges = append(
		detail.BillingMetadata.Charges, coreusage.BillingQuantity{Kind: "unknown", Unit: "request", Quantity: 1},
	)
	cost, _, err = buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Status != costStatusPartial || cost.Micro != 23200 {
		t.Fatalf("unknown fee=%+v/%v", cost, err)
	}
	detail = conditionalPricingDetail("qwen4-max", "openai-compatibility", 1000, 0, 0, 0, 0, 0, 1000)
	detail.BillingMetadata.Region = "unmatched-region"
	cost, _, err = buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Status != CostStatusUnpriced || cost.Micro != 0 {
		t.Fatalf("mismatched region=%+v/%v", cost, err)
	}
}

func TestConditionalModalityPartitionAndAdditionalPersistenceCompatibility(t *testing.T) {
	detail := conditionalPricingDetail("gpt-image-2.6-flare", "openai", 300, 0, 0, 0, 0, 0, 10)
	detail.BillingMetadata.Modalities = []coreusage.ModalityTokenUsage{
		{Category: "uncached_input", Modality: "text", Tokens: 100},
		{Category: "uncached_input", Modality: "image", Tokens: 200},
		{Category: "output", Modality: "image", Tokens: 10},
	}
	cost, receipt, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Micro != 2400 || cost.Status != costStatusComplete || receipt.SchemaVersion != 2 || receipt.Coverage.PricedTokens != 310 {
		t.Fatalf("modality reference=%+v/%+v/%v", cost, receipt, err)
	}
	store := openTestStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	detail.PricingReceipt = receipt
	detail.costTrusted, detail.costMicro, detail.CostStatus, detail.CostReason = true, cost.Micro, cost.Status, cost.Reason
	if _, err = persistEventWithPricing(ctx, tx, store, &detail, pricingSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var oldColumn, metadata sql.NullString
	var persistedAmount int64
	if err = store.db.QueryRow("SELECT pricing_receipt, metadata, cost_micro FROM events").Scan(
		&oldColumn, &metadata, &persistedAmount,
	); err != nil {
		t.Fatal(err)
	}
	if oldColumn.Valid || persistedAmount != 2400 || !strings.Contains(metadata.String, "pricing_receipt_v2") {
		t.Fatalf("legacy-safe storage=%+v/%d/%s", oldColumn, persistedAmount, metadata.String)
	}
	// P10 decodePricingReceipt returns nil before schema validation when this column is NULL.
	if legacy, err := decodePricingReceipt(oldColumn); err != nil || legacy != nil {
		t.Fatalf("legacy entry=%+v/%v", legacy, err)
	}
	restored, err := decodePricingReceipt(oldColumn, metadata)
	if err != nil || !reflect.DeepEqual(restored, receipt) {
		t.Fatalf("extended read=%+v/%v", restored, err)
	}
	changed := modelcatalog.DefaultFile()
	price := changed.Prices[detail.Model]
	*price.ModalityRates[0].Completion = 60
	changed.Prices[detail.Model] = price
	updated, err := computeCurrentCatalogEventCost(
		detail.Tokens, restored, pricingSnapshot{detailedPriceFn: detailedPriceFuncFromCatalog(changed)}, detail.Model,
		detail.Provider, detail.Timestamp, metadata,
	)
	if err != nil || updated.Micro != 2700 || receipt.CostMicro != 2400 {
		t.Fatalf("repricing changed original receipt: %v", err)
	}
	start, end := detail.Timestamp.Add(-time.Minute), detail.Timestamp.Add(time.Minute)
	full, count, err := store.queryEventDetails(ctx, start, end, EventFilters{}, 1, 10, "timestamp", false)
	if err != nil || count != 1 || len(full) != 1 || !reflect.DeepEqual(full[0].PricingReceipt, receipt) {
		t.Fatalf("full extended query=%+v/%d/%v", full, count, err)
	}
	compact, count, err := store.queryCompactEventDetails(ctx, start, end, EventFilters{}, 1, 10, "timestamp", false)
	if err != nil || count != 1 || len(compact) != 1 || !reflect.DeepEqual(compact[0].PricingReceipt, receipt) {
		t.Fatalf("compact extended query=%+v/%d/%v", compact, count, err)
	}
	backupRows, err := store.queryBackupEventSet(ctx, start, end, EventFilters{})
	if err != nil || len(backupRows) != 1 || !reflect.DeepEqual(backupRows[0].PricingReceipt, receipt) {
		t.Fatalf("backup extended query=%+v/%v", backupRows, err)
	}
	backup, err := MarshalUsageBackup(backupRows, "conditional-test-secret", detail.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ParseUsageBackup(backup, "conditional-test-secret")
	if err != nil || len(imported) != 1 || !reflect.DeepEqual(imported[0].PricingReceipt, receipt) {
		t.Fatalf("encrypted receipt roundtrip=%+v/%v", imported, err)
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var again PricingReceipt
	if err = json.Unmarshal(payload, &again); err != nil || validatePricingReceipt(&again) != nil {
		t.Fatalf("extended JSON roundtrip: %v", err)
	}
}

func TestConditionalSpeedFinalRatesOverrideRegionalBase(t *testing.T) {
	for _, modality := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("modality=%t", modality), func(t *testing.T) {
				var price modelcatalog.Price
				if err := json.Unmarshal(
					[]byte(`{"prompt":1,"completion":1,"cache_read":1,"region":"global","request_scoped":true,"speed_rates":[{"speed":"fast","multiplier":2}],"conditional_rates":[{"id":"us-base","region":"us","prompt":3},{"id":"fast-final","speed":"fast","prompt":4}]}`),
					&price,
				); err != nil {
					t.Fatal(err)
				}
				if modality {
					price.ModalityRates = []modelcatalog.ModalityRate{
						{
							Modality: "text", RateOverrides: modelcatalog.RateOverrides{Prompt: new(1.0)},
						},
					}
				}
				catalog := modelcatalog.File{
					SchemaVersion: modelcatalog.SchemaVersion, DefaultsVersion: modelcatalog.DefaultsVersion,
					Prices: map[string]modelcatalog.Price{"custom": price},
				}
				if err := catalog.Validate(); err != nil {
					t.Fatal(err)
				}
				detail := conditionalPricingDetail("custom", "openai", 1000000, 0, 0, 0, 0, 0, 0)
				detail.ResponseSpeed, detail.ResponseServiceTier, detail.BillingMetadata.Region = "fast", "priority", "us"
				if modality {
					detail.BillingMetadata.Modalities = []coreusage.ModalityTokenUsage{
						{
							Category: "uncached_input", Modality: "text", Tokens: 1000000,
						},
					}
				}
				cost, receipt, err := buildPricingReceipt(
					&detail, pricingSnapshot{detailedPriceFn: detailedPriceFuncFromCatalog(catalog)}, detail.Timestamp,
				)
				if err != nil || cost.Status != costStatusComplete || cost.Micro != 4000000 || *receipt.LineItems[0].SpeedMultiplier != 1 {
					t.Fatalf("final speed rate=%+v/%+v/%v", cost, receipt, err)
				}
			},
		)
	}
}

func TestConditionalLengthEvidenceOnlyForApplicableRules(t *testing.T) {
	for _, test := range []struct {
		name, speed, region string
		status              CostStatus
	}{
		{"standard ignores fast length", "standard", "global", costStatusComplete},
		{"fast requires length", "fast", "global", costStatusPartial},
		{"standard ignores other region", "standard", "us", costStatusComplete},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				price := completeDetailedPrice(1, 2)
				price.RequestScoped, price.Reason = true, ""
				price.ConditionalRates = []modelcatalog.ConditionalRate{
					{
						ID: "fast-long", Speed: "fast", MinInputTokens: new(int64(100)),
						RateOverrides: modelcatalog.RateOverrides{Prompt: new(3.0)},
					},
					{
						ID: "eu-long", Region: "eu", MinInputTokens: new(int64(100)),
						RateOverrides: modelcatalog.RateOverrides{Prompt: new(4.0)},
					},
				}
				detail := conditionalPricingDetail("custom", "gemini", 10, 0, 0, 0, 0, 0, 0)
				detail.ResponseSpeed, detail.ResponseServiceTier = test.speed, ""
				detail.BillingMetadata.ContextInputTokens, detail.BillingMetadata.Region = nil, test.region
				cost, _, err := buildPricingReceipt(
					&detail, detailedPricingSnapshot(price, "applicable-length"), detail.Timestamp,
				)
				if err != nil || cost.Status != test.status || (test.status == costStatusComplete && strings.Contains(
					cost.Reason, "prompt length",
				)) {
					t.Fatalf("applicable condition=%+v/%v", cost, err)
				}
			},
		)
	}
}

func TestConditionalPublishedZeroSurvivesCatalogWriter(t *testing.T) {
	for _, raw := range []string{
		`{"prompt":0,"completion":0,"request_scoped":true}`,
		`{"prompt":1,"completion":2,"request_scoped":true,"utc_rate_bands":[{"weekdays":["friday"],"start":"00:00","end":"23:59","prompt":0,"completion":0}]}`,
	} {
		t.Run(
			raw, func(t *testing.T) {
				var price modelcatalog.Price
				if err := json.Unmarshal([]byte(raw), &price); err != nil {
					t.Fatal(err)
				}
				catalog := modelcatalog.File{
					SchemaVersion: modelcatalog.SchemaVersion, DefaultsVersion: modelcatalog.DefaultsVersion,
					Prices: map[string]modelcatalog.Price{"free": price},
				}
				if err := catalog.Validate(); err != nil {
					t.Fatal(err)
				}
				pricing := pricingSnapshot{detailedPriceFn: detailedPriceFuncFromCatalog(catalog)}
				detail := conditionalPricingDetail("free", "openai", 100, 0, 0, 0, 0, 0, 10)
				store := openTestStore(t)
				tx, err := store.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err = persistEventWithPricing(t.Context(), tx, store, &detail, pricing); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				var micro int64
				var status CostStatus
				if err = store.db.QueryRow("SELECT cost_micro,cost_status FROM events").Scan(
					&micro, &status,
				); err != nil || micro != 0 || status != costStatusComplete {
					t.Fatalf("free writer result=%d/%s/%v", micro, status, err)
				}
			},
		)
	}
}

func TestConditionalPagedRecalculationRetainsExtendedEvidence(t *testing.T) {
	for _, entry := range []string{"page", "model"} {
		for _, projection := range []string{"current", "legacy", "absent"} {
			t.Run(
				entry+"/"+projection, func(t *testing.T) {
					ctx := t.Context()
					catalog := modelcatalog.DefaultFile()
					price := catalog.Prices["gpt-image-2.6-flare"]
					price.ChargeRates = []modelcatalog.ChargeRate{
						{
							Kind: "web_search", Unit: "request", Price: 10, PerUnits: 1000,
						},
					}
					catalog.Prices["gpt-image-2.6-flare"] = price
					pricing := pricingSnapshot{
						detailedPriceFn: detailedPriceFuncFromCatalog(catalog), revision: "original",
					}
					detail := conditionalPricingDetail("gpt-image-2.6-flare", "openai", 300, 0, 0, 0, 0, 0, 10)
					detail.BillingMetadata.Modalities = []coreusage.ModalityTokenUsage{
						{
							Category: "uncached_input", Modality: "text", Tokens: 100,
						}, {Category: "uncached_input", Modality: "image", Tokens: 200},
						{Category: "output", Modality: "image", Tokens: 10},
					}
					detail.BillingMetadata.Charges = []coreusage.BillingQuantity{
						{
							Kind: "web_search", Unit: "request", Quantity: 1,
						},
					}
					cost, receipt, err := buildPricingReceipt(&detail, pricing, detail.Timestamp)
					if err != nil || cost.Micro != 12400 || receipt.SchemaVersion != 2 {
						t.Fatalf("original=%+v/%v", cost, err)
					}
					detail.PricingReceipt = receipt
					store := openTestStore(t)
					tx, err := store.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err = persistEventWithPricing(ctx, tx, store, &detail, pricing); err != nil {
						t.Fatal(err)
					}
					if err = tx.Commit(); err != nil {
						t.Fatal(err)
					}
					var originalMetadata string
					if err = store.db.QueryRow("SELECT metadata FROM events").Scan(&originalMetadata); err != nil {
						t.Fatal(err)
					}
					switch projection {
					case "legacy":
						_, err = store.db.Exec("UPDATE event_fields SET pricing_metadata='{}'")
					case "absent":
						_, err = store.db.Exec("DELETE FROM event_fields")
					default:
						var projected string
						err = store.db.QueryRow("SELECT pricing_metadata FROM event_fields").Scan(&projected)
						if err == nil && (!strings.HasPrefix(
							projected, pricingMetadataProjectionPrefix,
						) || !strings.Contains(projected, "pricing_receipt_v2") || !strings.Contains(
							projected, "billing_metadata",
						)) {
							t.Fatalf("new projection lost evidence: %s", projected)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					price = price.Clone()
					*price.ModalityRates[0].Completion, price.ChargeRates[0].Price = 60, 20
					catalog.Prices[detail.Model] = price
					pricing = pricingSnapshot{
						detailedPriceFn: detailedPriceFuncFromCatalog(catalog), revision: "changed",
					}
					if entry == "page" {
						persister := &Persister{}
						task, err := persister.prepareCostRecalculationTask(ctx, store, pricing)
						if err != nil {
							t.Fatal(err)
						}
						if _, err = persister.repriceCostRecalculationPage(ctx, store, pricing, &task); err != nil {
							t.Fatal(err)
						}
					} else {
						var model modelRow
						if err = store.db.QueryRow("SELECT id,name FROM dim_model").Scan(
							&model.id, &model.name,
						); err != nil {
							t.Fatal(err)
						}
						tx, err = store.db.BeginTx(ctx, nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if err = repriceModelEvents(ctx, tx, model, pricing); err != nil {
							t.Fatal(err)
						}
						if err = tx.Commit(); err != nil {
							t.Fatal(err)
						}
					}
					var amount int64
					var status CostStatus
					var oldColumn sql.NullString
					var currentMetadata string
					if err = store.db.QueryRow("SELECT cost_micro,cost_status,pricing_receipt,metadata FROM events").Scan(
						&amount, &status, &oldColumn, &currentMetadata,
					); err != nil || amount != 22700 || status != costStatusComplete || oldColumn.Valid || currentMetadata != originalMetadata || receipt.CostMicro != 12400 {
						t.Fatalf(
							"paged result=%d/%s immutable=%t/%v", amount, status, currentMetadata == originalMetadata,
							err,
						)
					}
				},
			)
		}
	}
}

func TestConditionalPublishedGoogleModesRegionsAndCalendar(t *testing.T) {
	for _, test := range []struct {
		name, model, provider, speed, at, region, regionSource string
		uncached, read, output, micro                          int64
		status                                                 CostStatus
	}{
		{
			"Developer Pro long Batch", "gemini-3.1-pro-preview", "gemini", "batch", "2026-11-20T12:00:00Z", "global",
			"response", 200000, 100000, 1000, 449000, costStatusComplete,
		},
		{
			"Developer Pro long Priority", "gemini-3.1-pro-preview", "gemini", "fast", "2026-11-20T12:00:00Z", "global",
			"response", 200000, 100000, 1000, 1544400, costStatusComplete,
		},
		{
			"Developer 2.5 short Batch", "gemini-2.5-pro", "gemini", "batch", "2026-11-20T12:00:00Z", "global",
			"response", 100000, 100000, 1000, 80000, costStatusComplete,
		},
		{
			"Developer Flash Lite Flex cache", "gemini-3.5-flash-lite", "gemini", "flex", "2026-11-20T12:00:00Z",
			"global", "response", 1000, 5000, 100, 375, costStatusComplete,
		},
		{
			"Developer Flash Lite Batch cache", "gemini-3.5-flash-lite", "gemini", "batch", "2026-11-20T12:00:00Z",
			"global", "response", 1000, 5000, 100, 350, costStatusComplete,
		},
		{
			"Flash last introductory second", "gemini-3.8-flash", "gemini", "standard", "2026-12-31T23:59:59Z",
			"global", "response", 1000, 1000, 1000, 4575, costStatusComplete,
		},
		{
			"Flash first regular second", "gemini-3.8-flash", "gemini", "standard", "2027-01-01T00:00:00Z", "global",
			"response", 1000, 1000, 1000, 9150, costStatusComplete,
		},
		{
			"Flash Batch before boundary", "gemini-3.6-flash", "gemini", "batch", "2026-12-31T23:59:59Z", "global",
			"response", 1000, 1000, 1000, 2287, costStatusComplete,
		},
		{
			"Flash Batch after boundary", "gemini-3.6-flash", "gemini", "batch", "2027-01-01T00:00:00Z", "global",
			"response", 1000, 1000, 1000, 4575, costStatusComplete,
		},
		{
			"Carbon last introductory second", "gemini-4-carbon", "gemini", "standard", "2026-11-30T23:59:59Z",
			"global", "response", 1000, 1000, 1000, 12100, costStatusComplete,
		},
		{
			"Carbon first regular second", "gemini-4-carbon", "gemini", "standard", "2026-12-01T00:00:00Z", "global",
			"response", 1000, 1000, 1000, 24200, costStatusComplete,
		},
		{
			"Vertex 3.7 exact published table", "gemini-3.7-flash", "vertex", "standard", "2026-11-20T12:00:00Z",
			"global", "endpoint.locations", 1000, 1000, 1000, 4575, costStatusComplete,
		},
		{
			"Developer 3.7 remains unpublished", "gemini-3.7-flash", "gemini", "standard", "2026-11-20T12:00:00Z",
			"global", "response", 1000, 1000, 1000, 0, CostStatusUnpriced,
		},
		{
			"Vertex Pro cache unavailable in Batch", "gemini-3.1-pro-preview", "vertex", "batch",
			"2026-11-20T12:00:00Z", "global", "endpoint.locations", 200000, 100000, 1000, 409000, costStatusPartial,
		},
		{
			"Vertex non-global current", "gemini-4-carbon", "vertex", "standard", "2026-11-20T12:00:00Z", "us-central1",
			"endpoint.locations", 1000, 1000, 1000, 13310, costStatusComplete,
		},
		{
			"Vertex non-global future", "gemini-4-carbon", "vertex", "standard", "2026-12-01T00:00:00Z", "us-central1",
			"endpoint.locations", 1000, 1000, 1000, 26620, costStatusComplete,
		},
		{
			"Vertex proxy region is unknown", "gemini-4-carbon", "vertex", "standard", "2026-11-20T12:00:00Z",
			"us-central1", "proxy", 1000, 1000, 1000, 0, CostStatusUnpriced,
		},
		{
			"Kimi changed Coding identity", "kimi-k2.7-code", "kimi", "standard", "2026-11-20T12:00:00Z", "global",
			"response", 1000, 1000, 1000, 0, CostStatusUnpriced,
		},
		{
			"Kimi exact API reference", "kimi-k2.7-code", "openai-compatibility", "standard", "2026-11-20T12:00:00Z",
			"global", "response", 1000, 1000, 1000, 5140, costStatusComplete,
		},
		{
			"Antigravity has separate contract", "gemini-3.8-flash", "antigravity", "standard", "2026-11-20T12:00:00Z",
			"global", "response", 1000, 1000, 1000, 0, CostStatusUnpriced,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				detail := conditionalPricingDetail(
					test.model, test.provider, test.uncached, test.read, 0, 0, 0, 0, test.output,
				)
				var err error
				detail.Timestamp, err = time.Parse(time.RFC3339, test.at)
				if err != nil {
					t.Fatal(err)
				}
				detail.ResponseSpeed, detail.ResponseServiceTier = test.speed, ""
				detail.BillingMetadata.Region, detail.BillingMetadata.RegionSource = test.region, test.regionSource
				cost, receipt, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
				if err != nil || cost.Micro != test.micro || cost.Status != test.status {
					t.Fatalf(
						"published Google/identity result=%+v/%+v/%v, want %d/%s", cost, receipt, err, test.micro,
						test.status,
					)
				}
			},
		)
	}
}

func TestConditionalVertexCacheWriteFutureBoundary(t *testing.T) {
	for _, test := range []struct {
		at     string
		micro  int64
		status CostStatus
	}{
		{"2027-01-11T23:59:59Z", 9000, costStatusPartial},
		{"2027-01-12T00:00:00Z", 10875, costStatusComplete},
	} {
		detail := conditionalPricingDetail("gemini-4-hydrogen", "vertex", 1000, 0, 1000, 0, 0, 0, 1000)
		detail.cacheWrite5mTokens = 0
		detail.BillingMetadata.RegionSource = "endpoint.locations"
		var err error
		detail.Timestamp, err = time.Parse(time.RFC3339, test.at)
		if err != nil {
			t.Fatal(err)
		}
		cost, _, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
		if err != nil || cost.Micro != test.micro || cost.Status != test.status {
			t.Fatalf("Vertex write %s=%+v/%v", test.at, cost, err)
		}
	}
}

func TestConditionalGoogleAudioAndMissingModalityEvidence(t *testing.T) {
	for _, test := range []struct {
		provider string
		micro    int64
		status   CostStatus
	}{{"gemini", 2100, costStatusComplete}, {"vertex", 2000, costStatusPartial}} {
		detail := conditionalPricingDetail("gemini-3-flash-preview", test.provider, 1000, 1000, 0, 0, 0, 0, 1000)
		detail.ResponseSpeed, detail.ResponseServiceTier = "batch", "batch"
		detail.BillingMetadata.RegionSource = "endpoint.locations"
		detail.BillingMetadata.Modalities = []coreusage.ModalityTokenUsage{
			{
				Category: "uncached_input", Modality: "audio", Tokens: 1000,
			}, {Category: "cache_read", Modality: "audio", Tokens: 1000},
			{Category: "output", Modality: "text", Tokens: 1000},
		}
		cost, _, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
		if err != nil || cost.Micro != test.micro || cost.Status != test.status {
			t.Fatalf("audio %s=%+v/%v", test.provider, cost, err)
		}
		detail.BillingMetadata.Modalities = nil
		cost, receipt, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
		if err != nil || cost.Micro != 0 || receipt.Coverage.UnpricedTokens != 3000 || cost.Status != CostStatusUnpriced {
			t.Fatalf("unreported modality %s=%+v/%v", test.provider, cost, err)
		}
	}
}

func TestConditionalAdditionalChargeModesAreFinalPrices(t *testing.T) {
	for _, test := range []struct {
		speed  string
		micro  int64
		priced bool
	}{
		{"standard", 39000, true}, {"batch", 19500, true}, {"flex", 19500, true}, {"fast", 70200, true},
		{"unknown", 0, false},
	} {
		t.Run(
			test.speed, func(t *testing.T) {
				detail := conditionalPricingDetail("gemini-2.5-flash-image", "gemini", 0, 0, 0, 0, 0, 0, 0)
				detail.ResponseSpeed, detail.ResponseServiceTier = test.speed, ""
				detail.BillingMetadata.Charges = []coreusage.BillingQuantity{
					{
						Kind: "generated_image", Unit: "image", Quantity: 1,
					},
				}
				cost, receipt, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
				if err != nil || cost.Micro != test.micro || len(receipt.AdditionalLineItems) != 1 || receipt.AdditionalLineItems[0].Priced != test.priced || (test.priced && cost.Status != costStatusComplete) || (!test.priced && cost.Status == costStatusComplete) {
					t.Fatalf("final charge=%+v/%+v/%v", cost, receipt, err)
				}
			},
		)
	}
}

func TestConditionalWriterFreezesBillingEvidenceBeforeEnqueue(t *testing.T) {
	store := openTestStore(t)
	writer := newWriter(store, func(string) (float64, float64, float64, bool) { return 2, 10, 0.2, true })
	detail := conditionalPricingDetail("custom", "openai", 100, 0, 0, 0, 0, 0, 10)
	detail.CacheWriteReported = new(false)
	if !writer.submit(detail) {
		t.Fatal("writer rejected valid detail")
	}
	*detail.CacheWriteReported = true
	detail.BillingMetadata.Region = "mutated"
	queued := <-writer.ch
	if queued.CacheWriteReported == detail.CacheWriteReported || queued.CacheWriteReported == nil || *queued.CacheWriteReported || queued.PricingReceipt.CacheWriteReported == nil || *queued.PricingReceipt.CacheWriteReported || queued.BillingMetadata.Region != "global" {
		t.Fatal("queued billing evidence aliases caller memory")
	}
	if err := writer.flushTransactionWithPricing(t.Context(), []FlatDetail{queued}, pricingSnapshot{}); err != nil {
		t.Fatal(err)
	}
	var raw, receiptColumn sql.NullString
	if err := store.db.QueryRow("SELECT metadata,pricing_receipt FROM events").Scan(&raw, &receiptColumn); err != nil {
		t.Fatal(err)
	}
	var restored FlatDetail
	if err := decodeEventMetadata(
		&restored, raw,
	); err != nil || restored.CacheWriteReported == nil || *restored.CacheWriteReported || restored.BillingMetadata.Region != "global" {
		t.Fatalf("stored evidence changed=%+v/%v", restored, err)
	}
	receipt, err := decodePricingReceipt(receiptColumn, raw)
	if err != nil || receipt.CacheWriteReported == nil || *receipt.CacheWriteReported {
		t.Fatalf("stored receipt changed=%+v/%v", receipt, err)
	}
}

func TestConditionalGeminiPromptLengthExcludesSeparateToolPrompts(t *testing.T) {
	detail := conditionalPricingDetail("gemini-3.1-pro-preview", "gemini", 201000, 0, 0, 0, 0, 0, 1000)
	prompt := int64(199000)
	detail.BillingMetadata.ContextInputTokens = &prompt
	cost, _, err := buildPricingReceipt(&detail, defaultConditionalSnapshot(), detail.Timestamp)
	if err != nil || cost.Micro != 414000 || cost.Status != costStatusComplete {
		t.Fatalf("prompt versus total input=%+v/%v", cost, err)
	}
}
