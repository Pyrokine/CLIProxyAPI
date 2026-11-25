package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func speedPricingDetail() FlatDetail {
	return FlatDetail{
		Timestamp: time.Date(2026, 2, 16, 1, 0, 0, 0, time.UTC),
		Model:     "speed-model", Provider: "openai",
		AccountingVersion: coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(10, 0, 0, 7, 3, 20),
		Tokens:            tokenStats{InputTokens: 10, OutputTokens: 10, ReasoningTokens: 3, TotalTokens: 20},
	}
}

func speedPricingPrice() detailedPrice {
	price := completeDetailedPrice(1, 2)
	price.Reason = ""
	price.SpeedRates = []modelcatalog.SpeedRate{
		{Speed: "fast", Multiplier: 2, Source: "fast-source", EffectiveUntil: "2026-02-17T00:00:00Z"},
		{Speed: "ultrafast", Multiplier: 3, Source: "ultrafast-source"},
		{Speed: "flex", Multiplier: 0.5},
		{Speed: "batch", Multiplier: 0.25},
	}
	return price
}

func TestSpeedPricingDefaultStandardRates(t *testing.T) {
	t.Parallel()
	catalog := modelcatalog.File{Prices: modelcatalog.DefaultPrices()}
	catalog.Sanitize()
	pricing := pricingSnapshot{
		detailedPriceFn: detailedPriceFuncFromCatalog(catalog),
		revision:        catalogPriceRevision(catalog),
	}
	for _, test := range []struct {
		model, provider, speed string
		multiplier             float64
		cost                   int64
	}{
		{"gpt-6.1-sol", "codex", "ultrafast", 8, 960},
		{"gpt-6.1-sol", "openai", "ultrafast", 6, 720},
		{"gpt-6.2-sol", "codex", "ultrafast", 6, 720},
		{"gpt-6.2-sol", "openai", "ultrafast", 6, 720},
		{"gpt-6.1-sol", "codex", "fast", 2.5, 300},
		{"gpt-6.1-sol", "openai", "fast", 2, 240},
		{"gpt-6.1-terra", "codex", "fast", 2.5, 150},
		{"gpt-6.2-sol", "codex", "fast", 2, 240},
		{"gpt-5.5", "codex", "fast", 0, 350},
	} {
		t.Run(
			test.provider+"/"+test.model+"/"+test.speed, func(t *testing.T) {
				for _, at := range []time.Time{
					time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
					time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC),
				} {
					detail := speedPricingDetail()
					detail.Model, detail.Provider, detail.Timestamp = test.model, test.provider, at
					detail.CacheReadReported, detail.CacheWriteReported = true, new(true)
					detail.BillingMetadata = &coreusage.BillingMetadata{
						SchemaVersion: 1, Region: "global", RegionSource: "response", ChargesReported: true,
					}
					detail.ResponseServiceTier = test.speed
					if test.speed == "fast" {
						detail.ResponseServiceTier = "priority"
					}
					cost, receipt, err := buildPricingReceipt(&detail, pricing, at)
					if err != nil {
						t.Fatal(err)
					}
					if cost.Micro != test.cost || receipt.CostMicro != test.cost || receipt.Coverage.TotalTokens != 20 {
						t.Fatalf("default standard rates at %s = %+v / %+v", at, cost, receipt)
					}
					if test.multiplier == 0 {
						if receipt.SpeedMultiplier != nil || cost.Status != costStatusPartial || !strings.Contains(
							cost.Reason, "speed price is not configured",
						) {
							t.Fatalf("unpublished Codex speed rate = %+v / %+v", cost, receipt)
						}
					} else if receipt.SpeedMultiplier == nil || *receipt.SpeedMultiplier != test.multiplier {
						t.Fatalf("default multiplier = %+v, want %v", receipt.SpeedMultiplier, test.multiplier)
					}
				}
			},
		)
	}
}

func TestSpeedPricingUsesActualResponseOnly(t *testing.T) {
	tests := []struct {
		name, provider, requestTier, responseTier, requestSpeed, responseSpeed string
		failed                                                                 bool
		speed, source                                                          string
		cost                                                                   int64
		status                                                                 CostStatus
	}{
		{
			name: "standard", responseTier: "default", speed: "standard", source: "response_service_tier", cost: 30,
			status: costStatusComplete,
		},
		{
			name: "fast", responseTier: "priority", speed: "fast", source: "response_service_tier", cost: 60,
			status: costStatusComplete,
		},
		{
			name: "ultrafast", responseTier: "ultrafast", speed: "ultrafast", source: "response_service_tier", cost: 90,
			status: costStatusComplete,
		},
		{
			name: "flex", responseTier: "flex", speed: "flex", source: "response_service_tier", cost: 15,
			status: costStatusComplete,
		},
		{
			name: "batch", responseTier: "batch", speed: "batch", source: "response_service_tier", cost: 7,
			status: costStatusComplete,
		},
		{
			name: "requested fast returned default", requestTier: "priority", responseTier: "default",
			speed: "standard", source: "response_service_tier", cost: 30, status: costStatusComplete,
		},
		{
			name: "claude fast", provider: "claude", requestSpeed: "fast", responseSpeed: "fast", speed: "fast",
			source: "response_speed", cost: 60, status: costStatusComplete,
		},
		{
			name: "claude requested fast returned standard", provider: "claude", requestSpeed: "fast",
			responseSpeed: "standard", speed: "standard", source: "response_speed", cost: 30,
			status: costStatusComplete,
		},
		{
			name: "requested speed is not evidence", provider: "claude", requestSpeed: "fast", responseTier: "priority",
			speed: "unknown", source: "unknown", cost: 30, status: costStatusPartial,
		},
		{
			name: "request tier is not evidence", requestTier: "priority", speed: "unknown", source: "unknown",
			cost: 30, status: costStatusPartial,
		},
		{name: "missing", speed: "unknown", source: "unknown", cost: 30, status: costStatusPartial},
		{
			name: "unknown", responseTier: "unknown", speed: "unknown", source: "unknown", cost: 30,
			status: costStatusPartial,
		},
		{
			name: "invalid", responseTier: "not-a-speed", speed: "unknown", source: "unknown", cost: 30,
			status: costStatusPartial,
		},
		{
			name: "failed with usage and returned fast", responseTier: "priority", failed: true, speed: "fast",
			source: "response_service_tier", cost: 60, status: costStatusComplete,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				detail := speedPricingDetail()
				if test.provider != "" {
					detail.Provider = test.provider
				}
				detail.RequestServiceTier, detail.ResponseServiceTier = test.requestTier, test.responseTier
				detail.RequestSpeed, detail.ResponseSpeed, detail.Failed = test.requestSpeed, test.responseSpeed, test.failed
				cost, receipt, err := buildPricingReceipt(
					&detail, detailedPricingSnapshot(speedPricingPrice(), "speed-rev"), time.Now(),
				)
				if err != nil {
					t.Fatal(err)
				}
				if cost.Micro != test.cost || cost.Status != test.status || receipt.Speed != test.speed || receipt.SpeedSource != test.source || receipt.Coverage.TotalTokens != 20 {
					t.Fatalf("result = %+v receipt = %+v", cost, receipt)
				}
				if test.status == costStatusPartial && !strings.Contains(cost.Reason, "speed") {
					t.Fatalf("missing partial reason: %+v", cost)
				}
				if test.speed == "fast" && (receipt.SpeedMultiplier == nil || *receipt.SpeedMultiplier != 2 || receipt.SpeedRateSource != "fast-source" || receipt.SpeedEffectiveUntil != "2026-02-17T00:00:00Z") {
					t.Fatalf("speed evidence = %+v", receipt)
				}
			},
		)
	}
	for _, test := range []struct {
		response string
		status   CostStatus
	}{
		{"", costStatusPartial}, {"default", costStatusComplete}, {"priority", costStatusPartial},
		{"ultrafast", costStatusPartial},
	} {
		detail := speedPricingDetail()
		detail.ResponseServiceTier = test.response
		price := speedPricingPrice()
		price.SpeedRates = nil
		cost, receipt, err := buildPricingReceipt(&detail, detailedPricingSnapshot(price, "legacy"), time.Now())
		if err != nil || cost.Micro != 30 || cost.Status != test.status || receipt.SpeedMultiplier != nil {
			t.Fatalf("legacy response %q = %+v/%+v/%v", test.response, cost, receipt, err)
		}
	}
}

func TestSpeedPricingReceiptSelectsProviderAndEventTime(t *testing.T) {
	price := speedPricingPrice()
	price.SpeedRates = []modelcatalog.SpeedRate{
		{Speed: "fast", Multiplier: 2, Source: "promotion", EffectiveUntil: "2026-02-17T00:00:00Z"},
		{Speed: "fast", Multiplier: 3, Source: "regular", EffectiveFrom: "2026-02-17T00:00:00Z"},
		{
			Speed: "fast", Provider: "openai", Multiplier: 4, Source: "provider", EffectiveFrom: "2026-02-16T12:00:00Z",
			EffectiveUntil: "2026-02-16T13:00:00Z",
		},
	}
	for _, test := range []struct {
		provider, at, source string
		cost                 int64
	}{
		{"openai", "2026-02-16T12:00:00Z", "provider", 120},
		{"other", "2026-02-16T12:00:00Z", "promotion", 60},
		{"openai", "2026-02-16T13:00:00Z", "promotion", 60},
		{"openai", "2026-02-16T23:59:59.999999999Z", "promotion", 60},
		{"openai", "2026-02-17T00:00:00Z", "regular", 90},
	} {
		detail := speedPricingDetail()
		detail.Provider, detail.ResponseServiceTier = test.provider, "priority"
		var err error
		detail.Timestamp, err = time.Parse(time.RFC3339Nano, test.at)
		if err != nil {
			t.Fatal(err)
		}
		cost, receipt, err := buildPricingReceipt(&detail, detailedPricingSnapshot(price, "dates"), time.Now())
		if err != nil || cost.Micro != test.cost || receipt.SpeedRateSource != test.source {
			t.Fatalf("provider/date selection = %+v/%+v/%v", cost, receipt, err)
		}
	}
	price.SpeedRates = []modelcatalog.SpeedRate{{Speed: "fast", Provider: "claude", Multiplier: 9}}
	detail := speedPricingDetail()
	detail.ResponseServiceTier = "priority"
	cost, receipt, err := buildPricingReceipt(&detail, detailedPricingSnapshot(price, "mismatch"), time.Now())
	if err != nil || cost.Micro != 30 || cost.Status != costStatusPartial || receipt.SpeedMultiplier != nil {
		t.Fatalf("provider mismatch = %+v/%v", cost, err)
	}
}

func TestSpeedPricingClassifiedRatesAndFallbacks(t *testing.T) {
	detail := speedPricingDetail()
	detail.ResponseServiceTier = "priority"
	detail.TokenBreakdown = coreusage.NewIndependentTokenBreakdown(11, 13, 59, 29, 31, 143)
	detail.Tokens = tokenStats{
		InputTokens: 83, CacheReadTokens: 13, CacheWriteTokens: 59, OutputTokens: 60, ReasoningTokens: 31,
		TotalTokens: 143,
	}
	detail.cacheWrite5mTokens, detail.cacheWrite1hTokens = 17, 19
	price := speedPricingPrice()
	price.Prompt, price.Completion, price.CacheRead = 2, 3, new(0.1)
	price.CacheWrite, price.CacheWrite5m, price.CacheWrite1h = new(float64(4)), nil, new(float64(8))
	price.SpeedRates = []modelcatalog.SpeedRate{
		{
			Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{
				"prompt": 3, "completion": 4, "cache_read": 0, "cache_write": 0.5, "cache_write_5m": 5, "cache_write_1h": 6,
			},
		},
	}
	cost, receipt, err := buildPricingReceipt(&detail, detailedPricingSnapshot(price, "buckets"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cost.Micro != 2084 || cost.Status != costStatusComplete || receipt.Coverage.TotalTokens != 143 {
		t.Fatalf("classified cost = %+v receipt=%+v", cost, receipt)
	}
	want := map[pricingLineItemCategory][3]float64{
		pricingLineItemUncachedInput: {2, 3, 6}, pricingLineItemCacheRead: {0.1, 0, 0},
		pricingLineItemCacheWrite5m: {4, 5, 20}, pricingLineItemCacheWrite1h: {8, 6, 48},
		pricingLineItemCacheWriteUnknown: {4, 0.5, 2}, pricingLineItemOutput: {3, 4, 12},
		pricingLineItemReasoning: {3, 4, 12},
	}
	for _, item := range receipt.LineItems {
		rates := want[item.Category]
		if item.BaseRatePerMillion == nil || item.SpeedMultiplier == nil || item.RatePerMillion == nil || *item.BaseRatePerMillion != rates[0] || *item.SpeedMultiplier != rates[1] || *item.RatePerMillion != rates[2] {
			t.Fatalf("classified line = %+v", item)
		}
	}
	for _, test := range []struct {
		name                     string
		write, five, hour        *float64
		overrides                map[string]float64
		wantBase, wantMultiplier float64
		reason                   string
	}{
		{
			"prompt fallback then cache coefficient", nil, nil, nil,
			map[string]float64{"prompt": 9, "cache_write_5m": 3}, 2, 3, "prompt_fallback",
		},
		{"specific explicit zero base", new(float64(4)), new(float64(0)), new(float64(8)), nil, 0, 2, "cache_write_5m"},
		{
			"generic write coefficient", new(float64(4)), nil, nil, map[string]float64{"cache_write": 0}, 4, 0,
			"cache_write",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				detail.TokenBreakdown = coreusage.NewIndependentTokenBreakdown(0, 0, 2, 0, 0, 2)
				detail.Tokens = tokenStats{CacheWriteTokens: 2, TotalTokens: 2}
				detail.cacheWrite5mTokens, detail.cacheWrite1hTokens = 2, 0
				price.CacheWrite, price.CacheWrite5m, price.CacheWrite1h = test.write, test.five, test.hour
				price.SpeedRates[0].Multipliers = test.overrides
				_, receipt, err := buildPricingReceipt(&detail, detailedPricingSnapshot(price, "fallback"), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				item := receipt.LineItems[0]
				if *item.BaseRatePerMillion != test.wantBase || *item.SpeedMultiplier != test.wantMultiplier || *item.RatePerMillion != test.wantBase*test.wantMultiplier || item.RateSource != test.reason {
					t.Fatalf("fallback line = %+v", item)
				}
			},
		)
	}
	price.CacheWrite, price.CacheWrite5m, price.CacheWrite1h = nil, new(float64(4)), new(float64(8))
	price.SpeedRates[0].Multipliers = map[string]float64{"cache_write": 3, "cache_write_5m": 100}
	detail.cacheWrite5mTokens = 0
	_, receipt, err = buildPricingReceipt(&detail, detailedPricingSnapshot(price, "unknown-ttl"), time.Now())
	if err != nil || *receipt.LineItems[0].BaseRatePerMillion != 8 || *receipt.LineItems[0].SpeedMultiplier != 3 || *receipt.LineItems[0].RatePerMillion != 24 {
		t.Fatalf("unknown TTL pricing = %+v/%v", receipt, err)
	}
}

func TestSpeedPricingReceiptRoundtripCloneAndInvalidMoney(t *testing.T) {
	detail := speedPricingDetail()
	detail.ResponseServiceTier = "priority"
	_, receipt, err := buildPricingReceipt(
		&detail, detailedPricingSnapshot(speedPricingPrice(), "speed-revision"), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodePricingReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePricingReceipt(sql.NullString{String: encoded.(string), Valid: true})
	if err != nil || !reflect.DeepEqual(decoded, receipt) {
		t.Fatalf("receipt roundtrip = %+v/%v", decoded, err)
	}
	cloned := clonePricingReceipt(receipt)
	*cloned.SpeedMultiplier = 7
	*cloned.LineItems[0].BaseRatePerMillion = 8
	*cloned.LineItems[0].SpeedMultiplier = 9
	*cloned.LineItems[0].RatePerMillion = 10
	if *receipt.SpeedMultiplier != 2 || *receipt.LineItems[0].BaseRatePerMillion != 1 || *receipt.LineItems[0].SpeedMultiplier != 2 || *receipt.LineItems[0].RatePerMillion != 2 {
		t.Fatal("receipt clone shares speed pricing pointers")
	}
	for _, mutate := range []func(*PricingReceipt){
		func(r *PricingReceipt) { r.Speed = "invalid" },
		func(r *PricingReceipt) { r.SpeedSource = "request_speed" },
		func(r *PricingReceipt) { r.SpeedMultiplier = new(float64(0)) },
		func(r *PricingReceipt) { r.SpeedMultiplier = new(math.NaN()) },
		func(r *PricingReceipt) { r.LineItems[0].SpeedMultiplier = new(float64(-1)) },
		func(r *PricingReceipt) { r.LineItems[0].BaseRatePerMillion = new(math.Inf(1)) },
		func(r *PricingReceipt) { r.LineItems[0].SpeedMultiplier = nil },
		func(r *PricingReceipt) { r.LineItems[0].RatePerMillion = new(float64(1)) },
	} {
		bad := clonePricingReceipt(receipt)
		mutate(bad)
		if err := validatePricingReceipt(bad); err == nil {
			t.Fatalf("accepted bad receipt: %+v", bad)
		}
	}
	legacy := clonePricingReceipt(receipt)
	legacy.Speed, legacy.SpeedSource, legacy.SpeedMultiplier = "", "", nil
	legacy.SpeedRateSource, legacy.SpeedEffectiveFrom, legacy.SpeedEffectiveUntil = "", "", ""
	for i := range legacy.LineItems {
		legacy.LineItems[i].BaseRatePerMillion, legacy.LineItems[i].SpeedMultiplier = nil, nil
	}
	if _, err = encodePricingReceipt(legacy); err != nil {
		t.Fatalf("old receipt unreadable: %v", err)
	}
	for _, test := range []struct{ rate, multiplier float64 }{
		{math.MaxFloat64, 2}, {1, math.Inf(1)}, {1, math.Exp2(63)},
	} {
		price := speedPricingPrice()
		price.Prompt, price.SpeedRates[0].Multiplier = test.rate, test.multiplier
		if _, _, err := buildPricingReceipt(
			&detail, detailedPricingSnapshot(price, "overflow"), time.Now(),
		); err == nil {
			t.Fatalf("accepted out-of-range money %v * %v", test.rate, test.multiplier)
		}
	}
	badAccounting := detail
	badAccounting.TokenBreakdown = coreusage.NewIndependentTokenBreakdown(1000, 0, 0, 0, 0, 1000)
	badAccounting.Tokens = tokenStats{InputTokens: 1, TotalTokens: 1}
	if _, _, err = buildPricingReceipt(
		&badAccounting, detailedPricingSnapshot(speedPricingPrice(), "mismatch"), time.Now(),
	); err == nil || !strings.Contains(err.Error(), "token total does not match event total") {
		t.Fatalf("inconsistent 1/1000 accounting = %v", err)
	}
}

func TestSpeedPricingLegacyCostSharesRulesAndPreservesAccounting(t *testing.T) {
	detail := speedPricingDetail()
	detail.ResponseServiceTier = "priority"
	price := speedPricingPrice()
	for _, test := range []struct {
		name                     string
		tokens                   tokenStats
		breakdown                *coreusage.TokenBreakdown
		prompt, completion, want float64
		status                   CostStatus
	}{
		{
			"raw independent cache",
			tokenStats{InputTokens: 3, OutputTokens: 4, CacheReadTokens: 8, CacheWriteTokens: 2, TotalTokens: 7}, nil,
			2, 10, 112, costStatusPartial,
		},
		{
			"canonical price with raw totals retained", tokenStats{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
			new(coreusage.NewSubsetTokenBreakdown(1000, 200, 100, 300, 100, 1300)), 2, 10, 9600, costStatusComplete,
		},
		{
			"legacy one versus thousand", tokenStats{InputTokens: 1, TotalTokens: 1},
			new(coreusage.NewIndependentTokenBreakdown(1000, 0, 0, 0, 0, 1000)), 2, 0, 4000, costStatusComplete,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				price.Prompt, price.Completion, price.CacheRead, price.CacheWrite = test.prompt, test.completion, new(0.5), new(float64(3))
				cost, err := computeCost(
					test.tokens, test.breakdown, detailedPricingSnapshot(price, "legacy"), detail.Model,
					detail.Timestamp, &detail,
				)
				if err != nil || cost.Micro != int64(test.want) || cost.Status != test.status {
					t.Fatalf("legacy speed reference = %+v/%v", cost, err)
				}
			},
		)
	}
	detail.TokenBreakdown = coreusage.NewIndependentTokenBreakdown(11, 13, 59, 29, 31, 143)
	detail.Tokens = tokenStats{
		InputTokens: 83, CacheReadTokens: 13, CacheWriteTokens: 59, OutputTokens: 60, ReasoningTokens: 31,
		TotalTokens: 143,
	}
	detail.cacheWrite5mTokens, detail.cacheWrite1hTokens = 17, 19
	price.Prompt, price.Completion, price.CacheRead = 2, 3, new(0.1)
	price.CacheWrite, price.CacheWrite5m, price.CacheWrite1h = new(float64(4)), nil, new(float64(8))
	price.SpeedRates[0].Multipliers = map[string]float64{
		"prompt": 3, "completion": 4, "cache_read": 0, "cache_write": 0.5, "cache_write_5m": 5, "cache_write_1h": 6,
	}
	pricing := detailedPricingSnapshot(price, "shared")
	legacy, err := computeCost(detail.Tokens, &detail.TokenBreakdown, pricing, detail.Model, detail.Timestamp, &detail)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := buildPricingReceipt(&detail, pricing, time.Now())
	if err != nil || legacy.Micro != 1628 || current.Micro != 2084 || legacy.Status != current.Status {
		t.Fatalf("legacy and receipt costs = %+v/%+v/%v", legacy, current, err)
	}
}

func TestSpeedPricingReviewLegacyCacheWriteBase(t *testing.T) {
	const model = "claude-sonnet-4-6"
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	basePrice := modelcatalog.DefaultPrices()[model]
	basePrice.RequestScoped, basePrice.Region = false, ""
	basePrice.ConditionalRates, basePrice.ChargeRates, basePrice.CoverageGaps = nil, nil, nil
	if basePrice.Prompt != 3 || basePrice.CacheWrite != nil || basePrice.CacheWrite5m == nil || basePrice.CacheWrite1h == nil {
		t.Fatalf("TTL-only fixture changed: %+v", basePrice)
	}
	for _, entry := range []string{"import", "model", "page"} {
		for _, independent := range []bool{false, true} {
			for _, fast := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/independent=%t/fast=%t", entry, independent, fast), func(t *testing.T) {
						ctx := context.Background()
						store := openTestStore(t)
						price := basePrice.Clone()
						price.SpeedRates = nil
						detail := FlatDetail{
							Timestamp: at, Model: model, Provider: "claude", ResponseSpeed: "standard",
							Tokens: tokenStats{InputTokens: 1000000, CacheWriteTokens: 1000000, TotalTokens: 1000000},
						}
						want := int64(3000000)
						if independent {
							detail.Tokens.CacheWriteTokens, want = 2000000, 9000000
						}
						if fast {
							detail.ResponseSpeed = "fast"
							price.SpeedRates = []modelcatalog.SpeedRate{
								{
									Speed: "fast", Provider: "claude", Multiplier: 2,
								},
							}
							want *= 2
						}
						catalog := modelcatalog.File{Prices: map[string]modelcatalog.Price{model: price}}
						pricing := pricingSnapshot{
							detailedPriceFn: detailedPriceFuncFromCatalog(catalog), revision: "review-legacy",
						}
						if entry == "import" {
							payload, err := json.Marshal([]FlatDetail{detail})
							if err != nil {
								t.Fatal(err)
							}
							p := &Persister{
								store: store, priceProvider: &modelPriceProvider{cached: catalog, loadedAt: time.Now()},
							}
							if result, err := p.Import(payload); err != nil || result.Added != 1 {
								t.Fatalf("legacy import = %+v/%v", result, err)
							}
						} else {
							detail.costTrusted, detail.CostStatus = true, CostStatusUnpriced
							tx, err := store.db.BeginTx(ctx, nil)
							if err != nil {
								t.Fatal(err)
							}
							if _, err := persistEventWithPricing(
								ctx, tx, store, &detail, pricingSnapshot{},
							); err != nil {
								_ = tx.Rollback()
								t.Fatal(err)
							}
							if err := tx.Commit(); err != nil {
								t.Fatal(err)
							}
							if entry == "model" {
								var row modelRow
								if err := store.db.QueryRow("SELECT id, name FROM dim_model").Scan(
									&row.id, &row.name,
								); err != nil {
									t.Fatal(err)
								}
								tx, err := store.db.BeginTx(ctx, nil)
								if err != nil {
									t.Fatal(err)
								}
								if err := repriceModelEvents(ctx, tx, row, pricing); err != nil {
									_ = tx.Rollback()
									t.Fatal(err)
								}
								if err := tx.Commit(); err != nil {
									t.Fatal(err)
								}
							} else {
								p := &Persister{}
								task, err := p.prepareCostRecalculationTask(ctx, store, pricing)
								if err != nil {
									t.Fatal(err)
								}
								if _, err := p.repriceCostRecalculationPage(ctx, store, pricing, &task); err != nil {
									t.Fatal(err)
								}
							}
						}
						var amount int64
						var status CostStatus
						var reason string
						var receipt sql.NullString
						if err := store.db.QueryRow("SELECT cost_micro, cost_status, cost_reason, pricing_receipt FROM events").Scan(
							&amount, &status, &reason, &receipt,
						); err != nil {
							t.Fatal(err)
						}
						if amount != want || status != costStatusPartial || receipt.Valid || !strings.Contains(
							reason, "cache-write reference price uses prompt rate",
						) || strings.Contains(reason, "highest configured TTL rate") {
							t.Fatalf(
								"legacy reference = %d/%s/%s receipt=%+v, want %d", amount, status, reason, receipt,
								want,
							)
						}
						t.Logf(
							"entry=%s input_tokens=1000000 cache_write_tokens=%d speed=%s cost_micro=%d status=%s cache_write_base=prompt=3",
							entry, detail.Tokens.CacheWriteTokens, detail.ResponseSpeed, amount, status,
						)
					},
				)
			}
		}
	}
	price := basePrice.Clone()
	price.SpeedRates = nil
	pricing := pricingSnapshot{detailedPriceFn: detailedPriceFuncFromCatalog(modelcatalog.File{Prices: map[string]modelcatalog.Price{model: price}})}
	for _, test := range []struct {
		name             string
		five, hour, want int64
	}{{"5m", 1000000, 0, 3750000}, {"1h", 0, 1000000, 6000000}, {"unknown", 0, 0, 6000000}} {
		detail := FlatDetail{
			Timestamp: at, Model: model, Provider: "claude", ResponseSpeed: "standard",
			AccountingVersion:  coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:     coreusage.NewIndependentTokenBreakdown(0, 0, 1000000, 0, 0, 1000000),
			Tokens:             tokenStats{InputTokens: 1000000, CacheWriteTokens: 1000000, TotalTokens: 1000000},
			cacheWrite5mTokens: test.five, cacheWrite1hTokens: test.hour,
		}
		cost, _, err := buildPricingReceipt(&detail, pricing, at)
		if err != nil || cost.Micro != test.want || cost.Status != costStatusComplete {
			t.Fatalf("new receipt TTL %s = %+v/%v", test.name, cost, err)
		}
	}
}

func TestSpeedPricingReviewUnknownWithoutRulesRetainsReservation(t *testing.T) {
	const model = "claude-sonnet-4-6"
	price := modelcatalog.DefaultPrices()[model]
	price.RequestScoped, price.Region = false, ""
	price.ConditionalRates, price.ChargeRates, price.CoverageGaps = nil, nil, nil
	price.SpeedRates = nil
	for _, test := range []struct {
		name, requested, returned string
		wantAmount                int64
		status                    CostStatus
	}{
		{"requested fast without return", "fast", "", 1000, costStatusPartial},
		{"missing return and request", "", "", 1000, costStatusPartial},
		{"invalid return", "fast", "invalid", 1000, costStatusPartial},
		{"returned standard", "fast", "standard", 180, costStatusComplete},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ctx := context.Background()
				store := openTestStore(t)
				now := time.Now().UTC()
				reservation, err := store.ReserveAPIKey(
					ctx, apiKeyReservationRequest(
						now, "review-unknown",
						coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1000},
					),
				)
				if err != nil {
					t.Fatal(err)
				}
				catalog := modelcatalog.File{Prices: map[string]modelcatalog.Price{model: price}}
				writer := newWriterWithPriceProviderAndGate(
					store, &modelPriceProvider{cached: catalog, loadedAt: now}, nil, nil,
				)
				plugin := &loggerPlugin{
					persister: &Persister{store: store, writer: writer}, enabled: func() bool { return true },
				}
				plugin.HandleUsage(
					ctx, coreusage.Record{
						Provider: "claude", Model: model, APIKey: "client-key", ReservationID: reservation.ID,
						RequestedAt: now, RequestSpeed: test.requested, ResponseSpeed: test.returned,
						Detail: coreusage.Detail{
							InputTokens: 10, OutputTokens: 10, ReasoningTokens: 3, TotalTokens: 20,
							TokenBreakdown: coreusage.NewIndependentTokenBreakdown(10, 0, 0, 7, 3, 20),
						},
					},
				)
				writer.start(ctx)
				writer.Stop()
				var amount int64
				var status CostStatus
				var reason, originalReceipt string
				if err := store.db.QueryRow("SELECT cost_micro, cost_status, cost_reason, pricing_receipt FROM events").Scan(
					&amount, &status, &reason, &originalReceipt,
				); err != nil {
					t.Fatal(err)
				}
				_, _, _, _, tokens, windowAmount := readAPIKeyReservationWindow(t, store, reservation.ID)
				if amount != 180 || status != test.status || tokens != 20 || windowAmount != test.wantAmount || (status == costStatusPartial && !strings.Contains(
					reason, unconfirmedRequestSpeedReason,
				)) {
					t.Fatalf(
						"no-rule request = %d/%s tokens=%d window=%d reason=%s", amount, status, tokens, windowAmount,
						reason,
					)
				}
				changed := price.Clone()
				changed.Prompt, changed.Completion = 6, 30
				pricing := pricingSnapshot{
					detailedPriceFn: detailedPriceFuncFromCatalog(modelcatalog.File{Prices: map[string]modelcatalog.Price{model: changed}}),
					revision:        "review-unknown-recalculation",
				}
				p := &Persister{}
				task, err := p.prepareCostRecalculationTask(ctx, store, pricing)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.repriceCostRecalculationPage(ctx, store, pricing, &task); err != nil {
					t.Fatal(err)
				}
				var afterReceipt string
				if err := store.db.QueryRow("SELECT cost_micro, cost_status, pricing_receipt FROM events").Scan(
					&amount, &status, &afterReceipt,
				); err != nil {
					t.Fatal(err)
				}
				_, _, _, _, afterTokens, afterAmount := readAPIKeyReservationWindow(t, store, reservation.ID)
				if amount != 360 || status != test.status || afterReceipt != originalReceipt || afterTokens != 20 || afterAmount != windowAmount {
					t.Fatalf(
						"no-rule recalculation = %d/%s tokens=%d window=%d", amount, status, afterTokens, afterAmount,
					)
				}
				t.Logf(
					"request_speed=%q response_speed=%q original_cost_micro=180 recalculated_cost_micro=360 status=%s tokens=20 reservation_cost_micro=%d",
					test.requested, test.returned, status, windowAmount,
				)
			},
		)
	}
	legacy := tokenStats{InputTokens: 10, OutputTokens: 10, ReasoningTokens: 3, TotalTokens: 20}
	pricing := pricingSnapshot{detailedPriceFn: detailedPriceFuncFromCatalog(modelcatalog.File{Prices: map[string]modelcatalog.Price{model: price}})}
	cost, err := computeCurrentCatalogEventCost(
		legacy, nil, pricing, model, "claude", time.Now(),
		sql.NullString{String: `{"request_speed":"fast"}`, Valid: true},
	)
	if err != nil || cost.Micro != 180 || cost.Status != costStatusComplete || strings.Contains(
		cost.Reason, unconfirmedRequestSpeedReason,
	) {
		t.Fatalf("legacy unknown speed changed = %+v/%v", cost, err)
	}
}

func TestSpeedPricingUnpricedReceiptsKeepFailureReason(t *testing.T) {
	base := speedPricingPrice()
	base.SpeedRates = nil
	unsupported := base
	unsupported.Currency = "EUR"
	inactive := base
	inactive.Effective = false
	noRates := detailedPrice{Currency: "USD", Status: costStatusComplete, Effective: true}
	for _, test := range []struct {
		name         string
		pricing      pricingSnapshot
		inconsistent bool
		wantReason   string
	}{
		{"unconfigured model", pricingSnapshot{}, false, "model price is not configured"},
		{
			"unsupported currency", detailedPricingSnapshot(unsupported, "currency"), false,
			`pricing currency "EUR" is not supported by USD usage aggregation`,
		},
		{
			"ineffective price", detailedPricingSnapshot(inactive, "inactive"), false,
			"price is not effective at the event timestamp",
		},
		{
			"missing reference rates", detailedPricingSnapshot(noRates, "no-rates"), false,
			"model reference price is not configured",
		},
		{
			"inconsistent accounting", detailedPricingSnapshot(base, "inconsistent"), true,
			"token accounting is inconsistent",
		},
	} {
		for _, response := range []string{"", "priority"} {
			t.Run(
				test.name+"/"+response, func(t *testing.T) {
					detail := speedPricingDetail()
					detail.ResponseServiceTier = response
					if test.inconsistent {
						detail.TokenBreakdown = coreusage.TokenBreakdown{
							SchemaVersion: coreusage.TokenAccountingSchemaVersion,
							Quality:       coreusage.TokenAccountingQualityInconsistent, TotalTokens: 20,
							UnclassifiedTokens: 20,
						}
					}
					cost, receipt, err := buildPricingReceipt(&detail, test.pricing, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					if cost.Micro != 0 || cost.Status != CostStatusUnpriced || cost.Reason != test.wantReason || receipt.Coverage.PricedTokens != 0 || receipt.Coverage.UnpricedTokens != 20 {
						t.Fatalf("unpriced receipt = %+v/%+v", cost, receipt)
					}
					wantSpeed, wantSource := "unknown", "unknown"
					if response != "" {
						wantSpeed, wantSource = "fast", "response_service_tier"
					}
					if receipt.Speed != wantSpeed || receipt.SpeedSource != wantSource || strings.Contains(
						cost.Reason, "using base reference rates",
					) {
						t.Fatalf("unpriced speed evidence = %+v", receipt)
					}
				},
			)
		}
	}
}

func TestSpeedPricingReviewJSONZeroIsExplicit(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{{"null", false}, {"0", true}} {
		raw := `{"prompt":3,"completion":15,"speed_rates":[{"speed":"fast","provider":"openai","multiplier":2,"multipliers":{"prompt":` + test.value + `}}]}`
		var price modelcatalog.Price
		err := json.Unmarshal([]byte(raw), &price)
		if !test.valid {
			if err == nil {
				t.Fatal("null override was accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		detail := FlatDetail{
			Timestamp: time.Now(), Model: "json-speed-model", Provider: "openai", ResponseServiceTier: "priority",
			AccountingVersion: coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:    coreusage.NewIndependentTokenBreakdown(1000000, 0, 0, 0, 0, 1000000),
			Tokens:            tokenStats{InputTokens: 1000000, TotalTokens: 1000000},
		}
		pricing := pricingSnapshot{detailedPriceFn: detailedPriceFuncFromCatalog(modelcatalog.File{Prices: map[string]modelcatalog.Price{detail.Model: price}})}
		cost, receipt, err := buildPricingReceipt(&detail, pricing, time.Now())
		if err != nil || cost.Micro != 0 || cost.Status != costStatusComplete || receipt.LineItems[0].SpeedMultiplier == nil || *receipt.LineItems[0].SpeedMultiplier != 0 || *receipt.LineItems[0].BaseRatePerMillion != 3 {
			t.Fatalf("explicit JSON zero = %+v/%+v/%v", cost, receipt, err)
		}
	}
}

func TestSpeedPricingHistoricalMetadataUsesPricingFieldsOnly(t *testing.T) {
	detail := speedPricingDetail()
	pricing := detailedPricingSnapshot(speedPricingPrice(), "metadata")
	for _, test := range []struct {
		raw    string
		want   int64
		status CostStatus
		reason string
	}{
		{`{"response_service_tier":"priority","latency_ms":"invalid"}`, 60, costStatusComplete, ""},
		{`{"response_service_tier":"default","request_service_tier":"priority"}`, 30, costStatusComplete, ""},
		{`{"response_service_tier":123}`, 30, costStatusPartial, "metadata could not be decoded"},
		{`{`, 30, costStatusPartial, "metadata could not be decoded"},
	} {
		cost, err := computeCurrentCatalogEventCost(
			detail.Tokens, nil, pricing, detail.Model, detail.Provider, detail.Timestamp,
			sql.NullString{String: test.raw, Valid: true},
		)
		if err != nil || cost.Micro != test.want || cost.Status != test.status || (test.reason != "" && !strings.Contains(
			cost.Reason, test.reason,
		)) {
			t.Fatalf("historical metadata %s = %+v/%v", test.raw, cost, err)
		}
	}
	bad := sql.NullString{String: `{"accounting_version":2,"token_breakdown":{}}`, Valid: true}
	if _, err := computeCurrentCatalogEventCost(
		detail.Tokens, nil, pricing, detail.Model, detail.Provider, detail.Timestamp, bad,
	); err == nil {
		t.Fatal("malformed canonical accounting accepted")
	}
}

func TestSpeedPricingCatalogSnapshotAndRevision(t *testing.T) {
	catalog := modelcatalog.File{
		Prices: map[string]modelcatalog.Price{
			"speed-model": {
				Prompt: 1, SpeedRates: []modelcatalog.SpeedRate{
					{
						Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{"cache_read": 0},
					},
				},
			},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"openai": {{Name: "speed-model", Alias: "speed-alias"}}},
	}
	fn := detailedPriceFuncFromCatalog(catalog)
	revision := catalogPriceRevision(catalog)
	catalog.Prices["speed-model"].SpeedRates[0].Multipliers["cache_read"] = 7
	catalog.Prices["speed-model"].SpeedRates[0].Multiplier = 9
	price, found := fn("speed-alias", time.Now())
	if !found || price.CanonicalModel != "speed-model" || price.SpeedRates[0].Multiplier != 2 || price.SpeedRates[0].Multipliers["cache_read"] != 0 || revision == catalogPriceRevision(catalog) {
		t.Fatalf("immutable price snapshot = %+v/%t", price, found)
	}
	price.SpeedRates[0].Multipliers["cache_read"] = 8
	again, _ := fn("speed-model", time.Now())
	if again.SpeedRates[0].Multipliers["cache_read"] != 0 {
		t.Fatal("returned snapshot shares speed multipliers")
	}
}

func TestSpeedPricingImportUsesDetailedSnapshotAndPreservesExport(t *testing.T) {
	store := openTestStore(t)
	price := speedPricingPrice()
	provider := &scriptedDetailedPriceProvider{
		snapshots: []detailedPriceSnapshotFixture{
			{
				price: price, revision: "import-speed-revision",
			},
		},
	}
	persister := &Persister{store: store, priceProvider: provider}
	detail := speedPricingDetail()
	detail.ResponseServiceTier = "priority"
	missing := speedPricingDetail()
	missing.Timestamp = missing.Timestamp.Add(time.Second)
	payload, err := json.Marshal([]FlatDetail{detail, missing})
	if err != nil {
		t.Fatal(err)
	}
	result, err := persister.Import(payload)
	if err != nil || result.Added != 2 || provider.CallCount() != 1 {
		t.Fatalf("detailed import = %+v/%v calls=%d", result, err, provider.CallCount())
	}
	events, err := store.queryBackupEventSet(context.Background(), time.Time{}, time.Time{}, EventFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("imported events = %+v", events)
	}
	for _, event := range events {
		wantCost, wantStatus := int64(30), costStatusPartial
		if event.ResponseServiceTier == "priority" {
			wantCost, wantStatus = 60, costStatusComplete
		}
		if event.costMicro != wantCost || event.CostStatus != wantStatus || event.PricingReceipt != nil {
			t.Fatalf("imported pricing = %+v", event)
		}
	}
	live := detail
	live.Timestamp = live.Timestamp.Add(2 * time.Second)
	writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
	if !writer.submit(live) {
		t.Fatal("live event rejected")
	}
	writer.start(context.Background())
	writer.Stop()
	events, err = store.queryBackupEventSet(context.Background(), time.Time{}, time.Time{}, EventFilters{})
	if err != nil || len(events) != 3 {
		t.Fatalf("mixed live and legacy events = %d/%v", len(events), err)
	}
	const secret = "synthetic-speed-export-key"
	backup, err := MarshalUsageBackup(events, secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := openTestStore(t)
	changed := speedPricingPrice()
	changed.SpeedRates[0].Multiplier = 9
	imported, err := importBytesWithSecret(
		context.Background(), target, detailedPricingSnapshot(changed, "changed"), backup, secret,
	)
	if err != nil || imported.Added != 3 {
		t.Fatalf("backup import = %+v/%v", imported, err)
	}
	restored, err := target.queryBackupEventSet(context.Background(), time.Time{}, time.Time{}, EventFilters{})
	if err != nil || !reflect.DeepEqual(events, restored) {
		t.Fatalf("backup changed speed pricing: %v", err)
	}
	if err := PrepareUsageEventExport(events, secret); err != nil {
		t.Fatal(err)
	}
	flatExport, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	secondTarget := openTestStore(t)
	if result, err := importBytesWithSecret(
		context.Background(), secondTarget, detailedPricingSnapshot(changed, "changed"), flatExport, secret,
	); err != nil || result.Added != 3 {
		t.Fatalf("signed event import = %+v/%v", result, err)
	}
	secondRestored, err := secondTarget.queryBackupEventSet(
		context.Background(), time.Time{}, time.Time{}, EventFilters{},
	)
	if err != nil {
		t.Fatal(err)
	}
	receipts := 0
	for _, event := range secondRestored {
		if event.costMicro != 30 && event.costMicro != 60 {
			t.Fatalf("signed import repriced original evidence: %+v", event)
		}
		if event.PricingReceipt != nil {
			receipts++
			if event.PricingReceipt.PriceRevision != "import-speed-revision" {
				t.Fatalf("changed request receipt: %+v", event.PricingReceipt)
			}
		}
	}
	if receipts != 1 {
		t.Fatalf("request receipt count = %d, want one original receipt", receipts)
	}
}

func TestSpeedPricingStartupMigrationPreservesObservationAndLegacyContract(t *testing.T) {
	base := t.TempDir()
	details := make([]FlatDetail, 6)
	for index := range details {
		details[index] = speedPricingDetail()
		details[index].Timestamp = details[index].Timestamp.Add(time.Duration(index) * time.Second)
	}
	details[0].ResponseServiceTier = "priority"
	details[1].ResponseServiceTier = "default"
	details[2].RequestServiceTier = "priority"
	details[3].ResponseServiceTier = "priority"
	details[3].Timestamp = time.Date(2026, 2, 17, 0, 0, 0, 0, time.UTC)
	details[4].ResponseServiceTier = "priority"
	details[4].TokenBreakdown = coreusage.NewIndependentTokenBreakdown(1000, 0, 0, 0, 0, 1000)
	details[4].Tokens = tokenStats{InputTokens: 1, TotalTokens: 1}
	details[5].ResponseServiceTier = "priority"
	_, original, err := buildPricingReceipt(
		&details[5], detailedPricingSnapshot(speedPricingPrice(), "original-file"), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	details[5].PricingReceipt = original
	payload, err := json.Marshal(flatDetailsEnvelope{Version: 2, Date: "2026-02-16", Details: details})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "today.json")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedDetailedPriceProvider{
		snapshots: []detailedPriceSnapshotFixture{
			{
				speedPricingPrice(), "migration-speed",
			},
		},
	}
	persister := NewPersister(base, config.UsageRetention{Days: -1})
	persister.SetPriceProvider(provider)
	persister.Start(context.Background())
	defer persister.Stop()
	if !persister.IsReady() {
		t.Fatal("migration persister did not initialize")
	}
	events, err := persister.store.queryBackupEventSet(context.Background(), time.Time{}, time.Time{}, EventFilters{})
	if err != nil || len(events) != len(details) {
		t.Fatalf("migrated events = %d/%v", len(events), err)
	}
	want := map[int64]struct {
		cost   int64
		status CostStatus
	}{
		details[0].Timestamp.UnixNano(): {60, costStatusComplete},
		details[1].Timestamp.UnixNano(): {30, costStatusComplete},
		details[2].Timestamp.UnixNano(): {30, costStatusPartial},
		details[3].Timestamp.UnixNano(): {30, costStatusPartial},
		details[4].Timestamp.UnixNano(): {2000, costStatusComplete},
		details[5].Timestamp.UnixNano(): {60, costStatusComplete},
	}
	for _, event := range events {
		value := want[event.Timestamp.UnixNano()]
		if event.costMicro != value.cost || event.CostStatus != value.status {
			t.Fatalf("startup migration price = %+v, want %+v", event, value)
		}
		if event.Timestamp.Equal(details[5].Timestamp) && !reflect.DeepEqual(event.PricingReceipt, original) {
			t.Fatal("startup migration changed an existing receipt")
		}
		if event.Timestamp.Equal(details[4].Timestamp) && (event.Tokens.TotalTokens != 1 || event.TokenBreakdown.TotalTokens != 1000) {
			t.Fatal("startup migration changed legacy raw/canonical counts")
		}
	}
	if retained, err := os.ReadFile(path); err != nil || string(retained) != string(payload) {
		t.Fatalf("legacy JSON changed: %v", err)
	}
	status, err := checkMigrationStatus(context.Background(), persister.store)
	if err != nil || status.From == "" {
		t.Fatalf("migration marker = %+v/%v", status, err)
	}
	changed := speedPricingPrice()
	changed.SpeedRates[0].Multiplier = 9
	if err := migrateLegacyToSQLite(
		context.Background(), persister.store, detailedPricingSnapshot(changed, "changed"), base,
	); err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := persister.store.db.QueryRow("SELECT SUM(cost_micro) FROM events").Scan(&total); err != nil || total != 2210 {
		t.Fatalf("migration replay changed prices: %d/%v", total, err)
	}
}

func TestSpeedPricingHistoricalEntriesUseStoredObservation(t *testing.T) {
	for _, entry := range []string{"model", "page"} {
		for _, scenario := range []string{
			"old without receipt", "old receipt", "new receipt without raw metadata", "no evidence", "request only",
		} {
			t.Run(
				entry+"/"+scenario, func(t *testing.T) {
					ctx := context.Background()
					store := openTestStore(t)
					detail := speedPricingDetail()
					detail.ResponseServiceTier = "priority"
					detail.costTrusted, detail.costMicro, detail.CostStatus = true, 30, costStatusComplete
					if scenario == "no evidence" || scenario == "request only" {
						detail.ResponseServiceTier = ""
					}
					if scenario == "request only" {
						detail.RequestServiceTier = "priority"
					}
					if scenario == "old receipt" || scenario == "new receipt without raw metadata" {
						price := speedPricingPrice()
						if scenario == "old receipt" {
							price.SpeedRates = nil
						}
						_, detail.PricingReceipt, _ = buildPricingReceipt(
							&detail, detailedPricingSnapshot(price, "original"), time.Now(),
						)
						if detail.PricingReceipt == nil {
							t.Fatal("fixture receipt is nil")
						}
						if scenario == "old receipt" {
							detail.PricingReceipt.Speed, detail.PricingReceipt.SpeedSource = "", ""
						} else {
							detail.ResponseServiceTier = ""
						}
					}
					tx, err := store.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = persistEventWithPricing(ctx, tx, store, &detail, pricingSnapshot{}); err != nil {
						_ = tx.Rollback()
						t.Fatal(err)
					}
					if err = tx.Commit(); err != nil {
						t.Fatal(err)
					}
					var original sql.NullString
					if err = store.db.QueryRow("SELECT pricing_receipt FROM events").Scan(&original); err != nil {
						t.Fatal(err)
					}
					price := speedPricingPrice()
					price.SpeedRates[0].Provider, price.SpeedRates[0].Multiplier = "openai", 4
					pricing := detailedPricingSnapshot(price, "recalculated")
					if entry == "model" {
						var model modelRow
						if err := store.db.QueryRow("SELECT id, name FROM dim_model").Scan(
							&model.id, &model.name,
						); err != nil {
							t.Fatal(err)
						}
						tx, err := store.db.BeginTx(ctx, nil)
						if err != nil {
							t.Fatal(err)
						}
						if err := repriceModelEvents(ctx, tx, model, pricing); err != nil {
							_ = tx.Rollback()
							t.Fatal(err)
						}
						if err := tx.Commit(); err != nil {
							t.Fatal(err)
						}
					} else {
						p := &Persister{}
						task, err := p.prepareCostRecalculationTask(ctx, store, pricing)
						if err != nil {
							t.Fatal(err)
						}
						if changed, err := p.repriceCostRecalculationPage(
							ctx, store, pricing, &task,
						); err != nil || !changed {
							t.Fatalf("reprice page = %t/%v", changed, err)
						}
					}
					var cost int64
					var status CostStatus
					var receipt sql.NullString
					if err := store.db.QueryRow("SELECT cost_micro, cost_status, pricing_receipt FROM events").Scan(
						&cost, &status, &receipt,
					); err != nil {
						t.Fatal(err)
					}
					wantCost, wantStatus := int64(120), costStatusComplete
					if scenario == "no evidence" || scenario == "request only" {
						wantCost, wantStatus = 30, costStatusPartial
					}
					if cost != wantCost || status != wantStatus || receipt != original {
						t.Fatalf("historical cost=%d/%s original=%+v receipt=%+v", cost, status, original, receipt)
					}
				},
			)
		}
	}
}

func TestSpeedPricingReservationConsumesReceiptOnceAndSurvivesRecalculation(t *testing.T) {
	for _, test := range []struct {
		name, response       string
		failed               bool
		wantCost, wantAmount int64
		status               CostStatus
	}{
		{"fast", "priority", false, 60, 60, costStatusComplete},
		{"ultrafast", "ultrafast", false, 90, 90, costStatusComplete},
		{"returned standard", "default", false, 30, 30, costStatusComplete},
		{"unknown preserves amount reservation", "", false, 30, 1000, costStatusPartial},
		{"failed with valid fast usage", "priority", true, 60, 60, costStatusComplete},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ctx := context.Background()
				store := openTestStore(t)
				now := time.Now().UTC()
				reservation, err := store.ReserveAPIKey(
					ctx, apiKeyReservationRequest(
						now, "speed-reservation",
						coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1000},
					),
				)
				if err != nil {
					t.Fatal(err)
				}
				price := speedPricingPrice()
				price.SpeedRates[0].EffectiveUntil = ""
				changed := speedPricingPrice()
				changed.SpeedRates[0].EffectiveUntil, changed.SpeedRates[0].Multiplier = "", 7
				changed.SpeedRates[1].Multiplier = 7
				provider := &scriptedDetailedPriceProvider{
					snapshots: []detailedPriceSnapshotFixture{
						{
							price, "request-speed",
						}, {changed, "later-speed"},
					},
				}
				writer := newWriterWithPriceProviderAndGate(store, provider, nil, nil)
				detail := speedPricingDetail()
				detail.Timestamp, detail.ReservationID, detail.ResponseServiceTier, detail.Failed = now, reservation.ID, test.response, test.failed
				detail.RequestServiceTier = "priority"
				plugin := &loggerPlugin{
					persister: &Persister{store: store, writer: writer}, enabled: func() bool { return true },
				}
				plugin.HandleUsage(
					ctx, coreusage.Record{
						Provider: detail.Provider, Model: detail.Model, APIKey: "client-key",
						ReservationID: reservation.ID,
						RequestedAt:   now, RequestServiceTier: "priority", ResponseServiceTier: test.response,
						Failed: test.failed,
						Detail: coreusage.Detail{
							InputTokens: 10, OutputTokens: 10, ReasoningTokens: 3, TotalTokens: 20,
							TokenBreakdown: detail.TokenBreakdown,
						},
					},
				)
				if writer.stats().EventAccepted != 1 {
					t.Fatal("logger did not submit the priced usage event")
				}
				writer.start(ctx)
				writer.Stop()
				var eventCost, reservationTokens, reservationCost int64
				var status CostStatus
				var rawReceipt string
				if err := store.db.QueryRow("SELECT cost_micro, cost_status, pricing_receipt FROM events").Scan(
					&eventCost, &status, &rawReceipt,
				); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRow("SELECT total_tokens, cost_micro FROM api_key_reservation_event").Scan(
					&reservationTokens, &reservationCost,
				); err != nil {
					t.Fatal(err)
				}
				_, _, _, requests, tokens, amount := readAPIKeyReservationWindow(t, store, reservation.ID)
				if eventCost != test.wantCost || reservationCost != eventCost || status != test.status || reservationTokens != 20 || requests != 1 || tokens != 20 || amount != test.wantAmount {
					t.Fatalf(
						"settlement = event %d/%s reservation %d/%d window %d/%d/%d", eventCost, status,
						reservationTokens, reservationCost, requests, tokens, amount,
					)
				}
				original, err := decodePricingReceipt(sql.NullString{String: rawReceipt, Valid: true})
				if err != nil || original.CostMicro != eventCost || original.PriceRevision != "request-speed" {
					t.Fatalf("request snapshot = %+v/%v", original, err)
				}
				p := &Persister{}
				pricing := detailedPricingSnapshot(changed, "repriced-speed")
				task, err := p.prepareCostRecalculationTask(ctx, store, pricing)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.repriceCostRecalculationPage(ctx, store, pricing, &task); err != nil {
					t.Fatal(err)
				}
				var afterCost int64
				var afterReceipt string
				if err := store.db.QueryRow("SELECT cost_micro, pricing_receipt FROM events").Scan(
					&afterCost, &afterReceipt,
				); err != nil {
					t.Fatal(err)
				}
				wantAfter := test.wantCost
				if test.response == "priority" || test.response == "ultrafast" {
					wantAfter = 210
				}
				_, _, _, afterRequests, afterTokens, afterAmount := readAPIKeyReservationWindow(
					t, store, reservation.ID,
				)
				if err := store.db.QueryRow("SELECT cost_micro FROM api_key_reservation_event").Scan(&reservationCost); err != nil {
					t.Fatal(err)
				}
				if afterCost != wantAfter || rawReceipt != afterReceipt || requests != afterRequests || tokens != afterTokens || amount != afterAmount || reservationCost != test.wantCost {
					t.Fatalf(
						"recalculation changed settlement: event=%d reservation=%d window=%d/%d/%d", afterCost,
						reservationCost, afterRequests, afterTokens, afterAmount,
					)
				}
			},
		)
	}
}
