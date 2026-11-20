package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

const (
	PricingReceiptSchemaVersion         = 1
	extendedPricingReceiptSchemaVersion = 2
	maxPricingReceiptBytes              = 16 << 10
	maxPricingReceiptStringBytes        = 2048
	maxPricingReceiptReasonBytes        = 4096
	maxPricingReceiptLineItems          = 64
	unconfirmedRequestSpeedReason       = "actual request speed is unknown; using base reference rates"
)

type pricingLineItemCategory string

const (
	pricingLineItemUncachedInput     pricingLineItemCategory = "uncached_input"
	pricingLineItemCacheRead         pricingLineItemCategory = "cache_read"
	pricingLineItemCacheWrite5m      pricingLineItemCategory = "cache_write_5m"
	pricingLineItemCacheWrite1h      pricingLineItemCategory = "cache_write_1h"
	pricingLineItemCacheWrite30m     pricingLineItemCategory = "cache_write_30m"
	pricingLineItemCacheWrite12h     pricingLineItemCategory = "cache_write_12h"
	pricingLineItemCacheWriteUnknown pricingLineItemCategory = "cache_write_unknown"
	pricingLineItemOutput            pricingLineItemCategory = "output"
	pricingLineItemReasoning         pricingLineItemCategory = "reasoning"
	pricingLineItemUnclassified      pricingLineItemCategory = "unclassified"
)

type pricingCoverage struct {
	UncachedInputTokens     int64 `json:"uncached_input_tokens,omitempty,string"`
	CacheReadTokens         int64 `json:"cache_read_tokens,omitempty,string"`
	CacheWrite5mTokens      int64 `json:"cache_write_5m_tokens,omitempty,string"`
	CacheWrite1hTokens      int64 `json:"cache_write_1h_tokens,omitempty,string"`
	CacheWrite30mTokens     int64 `json:"cache_write_30m_tokens,omitempty,string"`
	CacheWrite12hTokens     int64 `json:"cache_write_12h_tokens,omitempty,string"`
	CacheWriteUnknownTokens int64 `json:"cache_write_unknown_tokens,omitempty,string"`
	OutputTokens            int64 `json:"output_tokens,omitempty,string"`
	ReasoningTokens         int64 `json:"reasoning_tokens,omitempty,string"`
	UnclassifiedTokens      int64 `json:"unclassified_tokens,omitempty,string"`
	PricedTokens            int64 `json:"priced_tokens,string"`
	UnpricedTokens          int64 `json:"unpriced_tokens,string"`
	TotalTokens             int64 `json:"total_tokens,string"`
}

type pricingLineItem struct {
	Category           pricingLineItemCategory `json:"category"`
	Modality           string                  `json:"modality,omitempty"`
	Tokens             int64                   `json:"tokens,string"`
	RatePerMillion     *float64                `json:"rate_per_million,omitempty"`
	BaseRatePerMillion *float64                `json:"base_rate_per_million,omitempty"`
	SpeedMultiplier    *float64                `json:"speed_multiplier,omitempty"`
	RateSource         string                  `json:"rate_source"`
	CostMicro          int64                   `json:"cost_micro,string"`
	Priced             bool                    `json:"priced"`
}

type pricingAdditionalLineItem struct {
	Kind       string   `json:"kind"`
	Unit       string   `json:"unit"`
	Quantity   float64  `json:"quantity"`
	Resolution string   `json:"resolution,omitempty"`
	Price      *float64 `json:"price,omitempty"`
	PerUnits   *float64 `json:"per_units,omitempty"`
	RateSource string   `json:"rate_source"`
	CostMicro  int64    `json:"cost_micro,string"`
	Priced     bool     `json:"priced"`
}

type PricingReceipt struct {
	SchemaVersion       int                         `json:"schema_version"`
	CapturedAt          time.Time                   `json:"captured_at"`
	PricedAt            time.Time                   `json:"priced_at"`
	Provider            string                      `json:"provider"`
	Model               string                      `json:"model"`
	CanonicalModel      string                      `json:"canonical_model"`
	PriceRevision       string                      `json:"price_revision,omitempty"`
	Source              string                      `json:"source"`
	Currency            string                      `json:"currency"`
	EffectiveFrom       string                      `json:"effective_from,omitempty"`
	EffectiveUntil      string                      `json:"effective_until,omitempty"`
	Speed               string                      `json:"speed,omitempty"`
	SpeedSource         string                      `json:"speed_source,omitempty"`
	SpeedMultiplier     *float64                    `json:"speed_multiplier,omitempty"`
	SpeedRateSource     string                      `json:"speed_rate_source,omitempty"`
	SpeedEffectiveFrom  string                      `json:"speed_effective_from,omitempty"`
	SpeedEffectiveUntil string                      `json:"speed_effective_until,omitempty"`
	CostMicro           int64                       `json:"cost_micro,string"`
	CostStatus          CostStatus                  `json:"cost_status"`
	CostReason          string                      `json:"cost_reason,omitempty"`
	Coverage            pricingCoverage             `json:"coverage"`
	LineItems           []pricingLineItem           `json:"line_items"`
	BillingMetadata     *coreusage.BillingMetadata  `json:"billing_metadata,omitempty"`
	CacheReadReported   *bool                       `json:"cache_read_reported,omitempty"`
	CacheWriteReported  *bool                       `json:"cache_write_reported,omitempty"`
	ConditionIDs        []string                    `json:"condition_ids,omitempty"`
	AdditionalLineItems []pricingAdditionalLineItem `json:"additional_line_items,omitempty"`
}

type pricingTokenAmounts struct {
	uncachedInput     int64
	cacheRead         int64
	cacheWrite5m      int64
	cacheWrite1h      int64
	cacheWrite30m     int64
	cacheWrite12h     int64
	cacheWriteUnknown int64
	output            int64
	reasoning         int64
	unclassified      int64
	total             int64
}

type pricingLineSpec struct {
	category        pricingLineItemCategory
	modality        string
	tokens          int64
	rate            *float64
	rateSource      string
	baseRate        *float64
	speedMultiplier *float64
	finalRate       bool
}

func buildPricingReceipt(
	detail *FlatDetail,
	pricing pricingSnapshot,
	capturedAt time.Time,
) (costResult, *PricingReceipt, error) {
	if detail == nil {
		return costResult{}, nil, errors.New("usage: cannot price a nil event")
	}
	tokens, err := validatedTokenStats(detail.Tokens)
	if err != nil {
		return costResult{}, nil, err
	}
	breakdown, err := validEventTokenBreakdown(detail)
	if err != nil {
		return costResult{}, nil, err
	}
	amounts, accountingReason, err := pricingTokenAmountsForReceipt(
		tokens,
		breakdown,
		detail.cacheWrite5mTokens,
		detail.cacheWrite1hTokens,
		detail.cacheWrite30mTokens,
		detail.cacheWrite12hTokens,
	)
	if err != nil {
		return costResult{}, nil, err
	}
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	} else {
		capturedAt = capturedAt.UTC()
	}
	pricedAt := detail.Timestamp
	if pricedAt.IsZero() {
		pricedAt = capturedAt
	} else {
		pricedAt = pricedAt.UTC()
	}
	model := strings.TrimSpace(detail.Model)
	if model == "" {
		model = "unknown"
	}
	provider := strings.TrimSpace(detail.Provider)
	speed, speedSource := ObservedRequestSpeed(detail)
	if original := detail.PricingReceipt; original != nil && original.Speed != "" && speed == "unknown" {
		if err := validatePricingReceipt(original); err != nil {
			return costResult{}, nil, err
		}
		speed, speedSource = original.Speed, original.SpeedSource
	}
	price, found := lookupDetailedPrice(pricing, model, pricedAt)
	speedRule, hasSpeedRule, speedReason := selectSpeedPrice(price, speed, provider, pricedAt)
	if speed == "unknown" {
		speedReason = unconfirmedRequestSpeedReason
	}
	receipt := &PricingReceipt{
		SchemaVersion:      PricingReceiptSchemaVersion,
		CapturedAt:         capturedAt,
		PricedAt:           pricedAt,
		Provider:           provider,
		Speed:              speed,
		SpeedSource:        speedSource,
		Model:              model,
		CanonicalModel:     model,
		PriceRevision:      strings.TrimSpace(pricing.revision),
		Source:             "unconfigured",
		Currency:           "USD",
		CostStatus:         CostStatusUnpriced,
		Coverage:           pricingCoverageFromAmounts(amounts),
		BillingMetadata:    coreusage.CloneBillingMetadata(detail.BillingMetadata),
		CacheWriteReported: coreusage.NormalizeCacheWriteReported(detail.CacheWriteReported, tokens.CacheWriteTokens),
	}
	if detail.CacheReadReported || detail.BillingMetadata != nil {
		reported := detail.CacheReadReported
		receipt.CacheReadReported = &reported
	}
	if found {
		if canonicalModel := strings.TrimSpace(price.CanonicalModel); canonicalModel != "" {
			receipt.CanonicalModel = canonicalModel
		}
		if source := strings.TrimSpace(price.Source); source != "" {
			receipt.Source = source
		} else {
			receipt.Source = "model-catalog"
		}
		if currency := strings.ToUpper(strings.TrimSpace(price.Currency)); currency != "" {
			receipt.Currency = currency
		}
		receipt.EffectiveFrom = strings.TrimSpace(price.EffectiveFrom)
		receipt.EffectiveUntil = strings.TrimSpace(price.EffectiveUntil)
	}

	if hasSpeedRule {
		receipt.SpeedMultiplier = &speedRule.Multiplier
		receipt.SpeedRateSource = speedRule.Source
		receipt.SpeedEffectiveFrom = speedRule.EffectiveFrom
		receipt.SpeedEffectiveUntil = speedRule.EffectiveUntil
	}
	reasons := make([]string, 0, 7)
	if accountingReason != "" {
		reasons = append(reasons, accountingReason)
	}
	if !found {
		reasons = append(reasons, "model price is not configured")
		setUnpricedPricingReceipt(receipt, amounts, strings.Join(reasons, "; "))
		return pricingReceiptResult(receipt), receipt, validateBuiltPricingReceipt(receipt)
	}
	if receipt.Currency != "USD" {
		reasons = append(
			reasons, fmt.Sprintf(
				"pricing currency %q is not supported by USD usage aggregation", receipt.Currency,
			),
		)
		setUnpricedPricingReceipt(receipt, amounts, strings.Join(reasons, "; "))
		return pricingReceiptResult(receipt), receipt, validateBuiltPricingReceipt(receipt)
	}
	if !price.Effective {
		reasons = append(reasons, "price is not effective at the event timestamp")
		setUnpricedPricingReceipt(receipt, amounts, strings.Join(reasons, "; "))
		return pricingReceiptResult(receipt), receipt, validateBuiltPricingReceipt(receipt)
	}
	if !hasReferenceTokenRates(price) && len(price.ChargeRates) == 0 {
		reason := strings.TrimSpace(price.Reason)
		if reason == "" {
			reason = "model reference price is not configured"
		}
		reasons = append(reasons, reason)
		setUnpricedPricingReceipt(receipt, amounts, strings.Join(reasons, "; "))
		return pricingReceiptResult(receipt), receipt, validateBuiltPricingReceipt(receipt)
	}
	if accountingReason == "token accounting is inconsistent" {
		setUnpricedPricingReceipt(receipt, amounts, strings.Join(reasons, "; "))
		return pricingReceiptResult(receipt), receipt, validateBuiltPricingReceipt(receipt)
	}
	if !validPricingCostStatus(price.Status) {
		return costResult{}, nil, fmt.Errorf("usage: invalid price status %q for model %q", price.Status, model)
	}
	if err := validateReceiptPriceRates(price, model); err != nil {
		return costResult{}, nil, err
	}

	lineSpecs := make([]pricingLineSpec, 0, maxPricingReceiptLineItems)
	promptRate := price.Prompt
	completionRate := price.Completion
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemUncachedInput,
		amounts.uncachedInput,
		&promptRate,
		"prompt",
	)
	cacheReadRate, cacheReadSource := promptRate, "prompt_fallback"
	if price.CacheRead != nil {
		cacheReadRate, cacheReadSource = *price.CacheRead, "cache_read"
	} else if amounts.cacheRead > 0 {
		reasons = append(reasons, "cache-read reference price uses prompt rate")
	}
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemCacheRead,
		amounts.cacheRead,
		&cacheReadRate,
		cacheReadSource,
	)
	cacheWrite5mRate, cacheWrite5mSource := receiptCacheWriteRate(price, pricingLineItemCacheWrite5m)
	if amounts.cacheWrite5m > 0 && cacheWrite5mSource == "prompt_fallback" {
		reasons = append(reasons, "5-minute cache-write reference price uses prompt rate")
	}
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemCacheWrite5m,
		amounts.cacheWrite5m,
		&cacheWrite5mRate,
		cacheWrite5mSource,
	)
	cacheWrite1hRate, cacheWrite1hSource := receiptCacheWriteRate(price, pricingLineItemCacheWrite1h)
	if amounts.cacheWrite1h > 0 && cacheWrite1hSource == "prompt_fallback" {
		reasons = append(reasons, "1-hour cache-write reference price uses prompt rate")
	}
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemCacheWrite1h,
		amounts.cacheWrite1h,
		&cacheWrite1hRate,
		cacheWrite1hSource,
	)
	for _, item := range []struct {
		category pricingLineItemCategory
		tokens   int64
	}{
		{pricingLineItemCacheWrite30m, amounts.cacheWrite30m},
		{pricingLineItemCacheWrite12h, amounts.cacheWrite12h},
	} {
		rate, source := receiptCacheWriteRate(price, item.category)
		lineSpecs = appendPricingLineSpec(lineSpecs, item.category, item.tokens, &rate, source)
	}
	cacheWriteUnknownRate, cacheWriteUnknownSource := receiptCacheWriteRate(
		price,
		pricingLineItemCacheWriteUnknown,
	)
	if amounts.cacheWriteUnknown > 0 {
		switch cacheWriteUnknownSource {
		case "max_ttl_fallback":
			reasons = append(reasons, "unknown cache-write TTL uses the highest configured TTL rate")
		case "prompt_fallback":
			reasons = append(reasons, "cache-write reference price uses prompt rate")
		}
	}
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemCacheWriteUnknown,
		amounts.cacheWriteUnknown,
		&cacheWriteUnknownRate,
		cacheWriteUnknownSource,
	)
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemOutput,
		amounts.output,
		&completionRate,
		"completion",
	)
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemReasoning,
		amounts.reasoning,
		&completionRate,
		"completion",
	)
	lineSpecs = appendPricingLineSpec(
		lineSpecs,
		pricingLineItemUnclassified,
		amounts.unclassified,
		nil,
		"unclassified",
	)
	lineSpecs, conditionReasons, conditionIDs, allFinal, err := resolvePricingLineSpecs(
		lineSpecs, price, detail, speed, pricedAt,
	)
	if err != nil {
		return costResult{}, nil, err
	}
	reasons = append(reasons, conditionReasons...)
	receipt.ConditionIDs = conditionIDs
	if allFinal || len(lineSpecs) == 0 {
		speedReason = ""
	}
	if price.RequestScoped && speed == "unknown" && len(price.SpeedRates) == 0 && strings.TrimSpace(detail.RequestSpeed) == "" {
		hasSpeedCondition := false
		for _, rule := range price.ConditionalRates {
			hasSpeedCondition = hasSpeedCondition || rule.Speed != ""
		}
		if !hasSpeedCondition && (detail.RequestServiceTier == "" || detail.RequestServiceTier == "default") {
			speedReason = ""
		}
	}
	if speedReason != "" {
		reasons = append(reasons, speedReason)
	}
	if hasSpeedRule {
		for index := range lineSpecs {
			spec := &lineSpecs[index]
			if spec.rate == nil {
				continue
			}
			multiplier := pricingSpeedMultiplier(speedRule, spec.category)
			if spec.finalRate {
				multiplier = 1
			}
			rate := *spec.rate * multiplier
			spec.baseRate, spec.rate, spec.speedMultiplier = spec.rate, &rate, &multiplier
		}
	}
	lineItems, totalCost, pricedTokens, unpricedTokens, err := priceReceiptLineItems(lineSpecs, model)
	if err != nil {
		return costResult{}, nil, err
	}
	additional, additionalCost, additionalIncomplete, err := priceAdditionalLineItems(
		price, detail.BillingMetadata, provider, speed,
	)
	if err != nil {
		return costResult{}, nil, err
	}
	combinedCost, ok := addPricingReceiptValues(totalCost, additionalCost)
	if !ok {
		return costResult{}, nil, errors.New("usage: total request cost exceeds int64 range")
	}
	receipt.LineItems = lineItems
	receipt.AdditionalLineItems = additional
	receipt.CostMicro = combinedCost
	receipt.Coverage.PricedTokens = pricedTokens
	receipt.Coverage.UnpricedTokens = unpricedTokens
	receipt.CostStatus = price.Status
	if price.RequestScoped {
		receipt.CostStatus = costStatusComplete
	}
	if additionalIncomplete {
		reasons = append(reasons, "additional billing quantities or rates are incomplete")
	}
	if speedReason != "" || len(conditionReasons) > 0 || additionalIncomplete || strings.Contains(
		accountingReason, "legacy cache overlap is ambiguous",
	) {
		receipt.CostStatus = mostConservativeCostStatus(receipt.CostStatus, costStatusPartial)
	}
	if unpricedTokens > 0 {
		if pricedTokens > 0 {
			receipt.CostStatus = mostConservativeCostStatus(receipt.CostStatus, costStatusPartial)
		} else if additionalCost == 0 {
			receipt.CostStatus = CostStatusUnpriced
		} else {
			receipt.CostStatus = costStatusPartial
		}
	}
	if reason := strings.TrimSpace(price.Reason); reason != "" && !price.RequestScoped {
		reasons = append(reasons, reason)
	}
	receipt.CostReason = strings.Join(reasons, "; ")
	if err := validateBuiltPricingReceipt(receipt); err != nil {
		return costResult{}, nil, err
	}
	return pricingReceiptResult(receipt), receipt, nil
}

func computeCurrentCatalogEventCost(
	tokens tokenStats,
	receipt *PricingReceipt,
	pricing pricingSnapshot,
	model, provider string,
	at time.Time,
	metadata sql.NullString,
) (costResult, error) {
	detail := FlatDetail{Timestamp: at, Model: model, Provider: provider, Tokens: tokens}
	breakdown, err := decodeEventTokenBreakdown(metadata)
	if err != nil {
		return costResult{}, err
	}
	if breakdown != nil {
		detail.AccountingVersion, detail.TokenBreakdown = coreusage.TokenAccountingSchemaVersion, *breakdown
	}
	metadataReason := ""
	if metadata.Valid && strings.TrimSpace(metadata.String) != "" {
		raw := []byte(metadata.String)
		if !json.Valid(raw) {
			metadataReason = "stored response metadata could not be decoded"
		} else {
			var observed struct {
				RequestSpeed        string                     `json:"request_speed"`
				ResponseSpeed       string                     `json:"response_speed"`
				ResponseServiceTier string                     `json:"response_service_tier"`
				ExecutorType        string                     `json:"executor_type"`
				BillingMetadata     *coreusage.BillingMetadata `json:"billing_metadata"`
				CacheReadReported   bool                       `json:"cache_read_reported"`
				CacheWriteReported  *bool                      `json:"cache_write_reported"`
			}
			if err := json.Unmarshal(raw, &observed); err != nil {
				metadataReason = "stored response speed metadata could not be decoded"
			} else {
				detail.RequestSpeed, detail.ResponseSpeed = observed.RequestSpeed, observed.ResponseSpeed
				detail.ResponseServiceTier, detail.ExecutorType = observed.ResponseServiceTier, observed.ExecutorType
				detail.BillingMetadata, detail.CacheReadReported = observed.BillingMetadata, observed.CacheReadReported
				detail.CacheWriteReported = coreusage.NormalizeCacheWriteReported(
					observed.CacheWriteReported, tokens.CacheWriteTokens,
				)
			}
		}
	}
	var cost costResult
	if receipt != nil {
		cost, err = computeCurrentCatalogCostFromPricingReceipt(tokens, receipt, pricing, at, detail)
	} else {
		cost, err = computeCost(tokens, breakdown, pricing, model, at, &detail)
	}
	if metadataReason != "" {
		if cost.Reason != "" {
			cost.Reason += "; "
		}
		cost.Reason += metadataReason
	}
	return cost, err
}

func computeCurrentCatalogCostFromPricingReceipt(
	tokens tokenStats,
	receipt *PricingReceipt,
	pricing pricingSnapshot,
	at time.Time,
	observations FlatDetail,
) (costResult, error) {
	if err := validatePricingReceipt(receipt); err != nil {
		return costResult{}, err
	}
	if receipt == nil {
		return costResult{}, errors.New("usage: current-catalog pricing requires a receipt")
	}
	tokens, err := validatedTokenStats(tokens)
	if err != nil {
		return costResult{}, err
	}
	coverage := receipt.Coverage
	if coverage.TotalTokens != tokens.TotalTokens {
		return costResult{}, errors.New("usage: pricing receipt total does not match stored event tokens")
	}
	cacheWriteTokens, ok := addPricingReceiptValues(
		coverage.CacheWrite5mTokens,
		coverage.CacheWrite1hTokens,
		coverage.CacheWrite30mTokens,
		coverage.CacheWrite12hTokens,
		coverage.CacheWriteUnknownTokens,
	)
	if !ok {
		return costResult{}, errors.New("usage: pricing receipt cache-write total overflows int64")
	}
	inputTotal, ok := addPricingReceiptValues(
		coverage.UncachedInputTokens,
		coverage.CacheReadTokens,
		cacheWriteTokens,
	)
	if !ok {
		return costResult{}, errors.New("usage: pricing receipt input total overflows int64")
	}
	outputTotal, ok := addPricingReceiptValues(coverage.OutputTokens, coverage.ReasoningTokens)
	if !ok {
		return costResult{}, errors.New("usage: pricing receipt output total overflows int64")
	}
	quality := coreusage.TokenAccountingQualityComplete
	if coverage.UnclassifiedTokens > 0 {
		quality = coreusage.TokenAccountingQualityUnclassified
	}
	provider := receipt.Provider
	if provider == "" {
		provider = observations.Provider
	}
	detail := FlatDetail{
		Timestamp:           at,
		Model:               receipt.Model,
		Provider:            provider,
		RequestSpeed:        observations.RequestSpeed,
		ResponseSpeed:       observations.ResponseSpeed,
		ResponseServiceTier: observations.ResponseServiceTier,
		ExecutorType:        observations.ExecutorType,
		PricingReceipt:      receipt,
		AccountingVersion:   coreusage.TokenAccountingSchemaVersion,
		TokenBreakdown: coreusage.TokenBreakdown{
			SchemaVersion: coreusage.TokenAccountingSchemaVersion,
			Quality:       quality,
			TotalTokens:   coverage.TotalTokens,
			Input: coreusage.TokenInputBreakdown{
				TotalTokens:      inputTotal,
				UncachedTokens:   coverage.UncachedInputTokens,
				CacheReadTokens:  coverage.CacheReadTokens,
				CacheWriteTokens: cacheWriteTokens,
			},
			Output: coreusage.TokenOutputBreakdown{
				TotalTokens:        outputTotal,
				NonReasoningTokens: coverage.OutputTokens,
				ReasoningTokens:    coverage.ReasoningTokens,
			},
			UnclassifiedTokens: coverage.UnclassifiedTokens,
		},
		Tokens:              tokens,
		cacheWrite5mTokens:  coverage.CacheWrite5mTokens,
		cacheWrite1hTokens:  coverage.CacheWrite1hTokens,
		cacheWrite30mTokens: coverage.CacheWrite30mTokens,
		cacheWrite12hTokens: coverage.CacheWrite12hTokens,
		BillingMetadata:     coreusage.CloneBillingMetadata(receipt.BillingMetadata),
		CacheWriteReported:  coreusage.NormalizeCacheWriteReported(receipt.CacheWriteReported, cacheWriteTokens),
	}
	if receipt.CacheReadReported != nil {
		detail.CacheReadReported = *receipt.CacheReadReported
	}
	cost, _, err := buildPricingReceipt(&detail, pricing, time.Now().UTC())
	return cost, err
}

func pricingTokenAmountsForReceipt(
	tokens tokenStats,
	breakdown *coreusage.TokenBreakdown,
	cacheWrite5mTokens int64,
	cacheWrite1hTokens int64,
	extraTTLs ...int64,
) (pricingTokenAmounts, string, error) {
	var cacheWrite30mTokens, cacheWrite12hTokens int64
	if len(extraTTLs) != 0 {
		if len(extraTTLs) != 2 {
			return pricingTokenAmounts{}, "", errors.New("usage: invalid cache-write TTL arguments")
		}
		cacheWrite30mTokens, cacheWrite12hTokens = extraTTLs[0], extraTTLs[1]
	}
	if cacheWrite5mTokens < 0 || cacheWrite1hTokens < 0 || cacheWrite30mTokens < 0 || cacheWrite12hTokens < 0 {
		return pricingTokenAmounts{}, "", errors.New("usage: cache-write TTL tokens must be non-negative")
	}
	if breakdown == nil {
		cacheReadTokens := tokens.CacheReadTokens
		cacheWriteTokens := tokens.CacheWriteTokens
		if cacheReadTokens == 0 && cacheWriteTokens == 0 && tokens.CachedTokens > 0 {
			cacheReadTokens = tokens.CachedTokens
		}
		cacheTokens, ok := addPricingReceiptValues(cacheReadTokens, cacheWriteTokens)
		if !ok || cacheTokens > tokens.InputTokens || tokens.ReasoningTokens > tokens.OutputTokens {
			return pricingTokenAmounts{unclassified: tokens.TotalTokens, total: tokens.TotalTokens},
				"token accounting is unavailable", nil
		}
		knownCacheWriteTokens, ok := addPricingReceiptValues(
			cacheWrite5mTokens, cacheWrite1hTokens, cacheWrite30mTokens, cacheWrite12hTokens,
		)
		if !ok || knownCacheWriteTokens > cacheWriteTokens {
			return pricingTokenAmounts{}, "", errors.New(
				"usage: cache-write TTL detail exceeds aggregate cache-write tokens",
			)
		}
		amounts := pricingTokenAmounts{
			uncachedInput:     tokens.InputTokens - cacheTokens,
			cacheRead:         cacheReadTokens,
			cacheWrite5m:      cacheWrite5mTokens,
			cacheWrite1h:      cacheWrite1hTokens,
			cacheWrite30m:     cacheWrite30mTokens,
			cacheWrite12h:     cacheWrite12hTokens,
			cacheWriteUnknown: cacheWriteTokens - knownCacheWriteTokens,
			output:            tokens.OutputTokens - tokens.ReasoningTokens,
			reasoning:         tokens.ReasoningTokens,
			total:             tokens.TotalTokens,
		}
		classified, ok := addPricingReceiptValues(
			amounts.uncachedInput,
			amounts.cacheRead,
			amounts.cacheWrite5m,
			amounts.cacheWrite1h,
			amounts.cacheWrite30m,
			amounts.cacheWrite12h,
			amounts.cacheWriteUnknown,
			amounts.output,
			amounts.reasoning,
		)
		if !ok || classified > amounts.total {
			return pricingTokenAmounts{unclassified: tokens.TotalTokens, total: tokens.TotalTokens},
				"token accounting is unavailable", nil
		}
		amounts.unclassified = amounts.total - classified
		reasons := make([]string, 0, 2)
		if cacheTokens > 0 {
			reasons = append(reasons, "legacy cache overlap is ambiguous; applied subset assumption")
		}
		if amounts.unclassified > 0 {
			reasons = append(reasons, "token accounting contains unclassified tokens")
		}
		return amounts, strings.Join(reasons, "; "), nil
	}
	if !breakdown.Valid() {
		return pricingTokenAmounts{}, "", errors.New("usage: invalid token breakdown")
	}
	if breakdown.TotalTokens != tokens.TotalTokens {
		return pricingTokenAmounts{}, "", errors.New("usage: pricing receipt token total does not match event total")
	}
	if breakdown.Quality == coreusage.TokenAccountingQualityInconsistent {
		return pricingTokenAmounts{unclassified: breakdown.TotalTokens, total: breakdown.TotalTokens},
			"token accounting is inconsistent", nil
	}
	knownCacheWriteTokens, ok := addPricingReceiptValues(
		cacheWrite5mTokens, cacheWrite1hTokens, cacheWrite30mTokens, cacheWrite12hTokens,
	)
	if !ok || knownCacheWriteTokens > breakdown.Input.CacheWriteTokens {
		return pricingTokenAmounts{}, "", errors.New(
			"usage: cache-write TTL detail exceeds aggregate cache-write tokens",
		)
	}
	amounts := pricingTokenAmounts{
		uncachedInput:     breakdown.Input.UncachedTokens,
		cacheRead:         breakdown.Input.CacheReadTokens,
		cacheWrite5m:      cacheWrite5mTokens,
		cacheWrite1h:      cacheWrite1hTokens,
		cacheWrite30m:     cacheWrite30mTokens,
		cacheWrite12h:     cacheWrite12hTokens,
		cacheWriteUnknown: breakdown.Input.CacheWriteTokens - knownCacheWriteTokens,
		output:            breakdown.Output.NonReasoningTokens,
		reasoning:         breakdown.Output.ReasoningTokens,
		unclassified:      breakdown.UnclassifiedTokens,
		total:             breakdown.TotalTokens,
	}
	if breakdown.Quality == coreusage.TokenAccountingQualityUnclassified {
		return amounts, "token accounting contains unclassified tokens", nil
	}
	return amounts, "", nil
}

func pricingCoverageFromAmounts(amounts pricingTokenAmounts) pricingCoverage {
	return pricingCoverage{
		UncachedInputTokens:     amounts.uncachedInput,
		CacheReadTokens:         amounts.cacheRead,
		CacheWrite5mTokens:      amounts.cacheWrite5m,
		CacheWrite1hTokens:      amounts.cacheWrite1h,
		CacheWrite30mTokens:     amounts.cacheWrite30m,
		CacheWrite12hTokens:     amounts.cacheWrite12h,
		CacheWriteUnknownTokens: amounts.cacheWriteUnknown,
		OutputTokens:            amounts.output,
		ReasoningTokens:         amounts.reasoning,
		UnclassifiedTokens:      amounts.unclassified,
		TotalTokens:             amounts.total,
	}
}

func appendPricingLineSpec(
	items []pricingLineSpec,
	category pricingLineItemCategory,
	tokens int64,
	rate *float64,
	rateSource string,
) []pricingLineSpec {
	if tokens <= 0 {
		return items
	}
	return append(
		items, pricingLineSpec{
			category: category, tokens: tokens, rate: rate, rateSource: rateSource,
		},
	)
}

func selectSpeedPrice(price detailedPrice, speed, provider string, at time.Time) (
	modelcatalog.SpeedRate,
	bool,
	string,
) {
	rule, found := (modelcatalog.Price{SpeedRates: price.SpeedRates}).SpeedRateAt(speed, provider, at)
	if speed == "unknown" && len(price.SpeedRates) > 0 {
		return rule, found, unconfirmedRequestSpeedReason
	}
	if speed != "unknown" && speed != "standard" && !found {
		return rule, false, fmt.Sprintf(
			"%s speed price is not configured for provider %q at the event timestamp; using base reference rates",
			speed, provider,
		)
	}
	return rule, found, ""
}

func pricingSpeedMultiplier(rule modelcatalog.SpeedRate, category pricingLineItemCategory) float64 {
	var bucket string
	switch category {
	case pricingLineItemUncachedInput:
		bucket = "prompt"
	case pricingLineItemOutput, pricingLineItemReasoning:
		bucket = "completion"
	case pricingLineItemCacheRead:
		bucket = "cache_read"
	case pricingLineItemCacheWrite5m:
		bucket = "cache_write_5m"
	case pricingLineItemCacheWrite1h:
		bucket = "cache_write_1h"
	case pricingLineItemCacheWrite30m:
		bucket = "cache_write_30m"
	case pricingLineItemCacheWrite12h:
		bucket = "cache_write_12h"
	case pricingLineItemCacheWriteUnknown:
		bucket = "cache_write"
	}
	if multiplier, found := rule.Multipliers[bucket]; found {
		return multiplier
	}
	if category == pricingLineItemCacheWrite5m || category == pricingLineItemCacheWrite1h || category == pricingLineItemCacheWrite30m || category == pricingLineItemCacheWrite12h {
		if multiplier, found := rule.Multipliers["cache_write"]; found {
			return multiplier
		}
	}
	return rule.Multiplier
}

func receiptCacheWriteRate(price detailedPrice, category pricingLineItemCategory) (float64, string) {
	switch category {
	case pricingLineItemCacheWrite5m:
		if price.CacheWrite5m != nil {
			return *price.CacheWrite5m, "cache_write_5m"
		}
	case pricingLineItemCacheWrite1h:
		if price.CacheWrite1h != nil {
			return *price.CacheWrite1h, "cache_write_1h"
		}
	case pricingLineItemCacheWrite30m:
		if price.CacheWrite30m != nil {
			return *price.CacheWrite30m, "cache_write_30m"
		}
	case pricingLineItemCacheWrite12h:
		if price.CacheWrite12h != nil {
			return *price.CacheWrite12h, "cache_write_12h"
		}
	case pricingLineItemCacheWriteUnknown:
		if price.CacheWrite != nil {
			return *price.CacheWrite, "cache_write"
		}
		var highest float64
		found := false
		for _, rate := range []*float64{
			price.CacheWrite5m, price.CacheWrite1h, price.CacheWrite30m, price.CacheWrite12h,
		} {
			if rate != nil && (!found || *rate > highest) {
				highest = *rate
				found = true
			}
		}
		if found {
			return highest, "max_ttl_fallback"
		}
		return price.Prompt, "prompt_fallback"
	}
	if price.CacheWrite != nil {
		return *price.CacheWrite, "cache_write"
	}
	return price.Prompt, "prompt_fallback"
}

func validateReceiptPriceRates(price detailedPrice, model string) error {
	for _, item := range []struct {
		name string
		rate *float64
	}{
		{name: "prompt", rate: &price.Prompt},
		{name: "completion", rate: &price.Completion},
		{name: "cache_read", rate: price.CacheRead},
		{name: "cache_write", rate: price.CacheWrite},
		{name: "cache_write_5m", rate: price.CacheWrite5m},
		{name: "cache_write_1h", rate: price.CacheWrite1h},
		{name: "cache_write_30m", rate: price.CacheWrite30m},
		{name: "cache_write_12h", rate: price.CacheWrite12h},
	} {
		if item.rate != nil {
			if err := validateCostRate(item.name, *item.rate, model); err != nil {
				return err
			}
		}
	}
	return nil
}

func priceReceiptLineItems(
	specs []pricingLineSpec,
	model string,
) ([]pricingLineItem, int64, int64, int64, error) {
	items := make([]pricingLineItem, 0, len(specs))
	fractions := make([]float64, 0, len(specs))
	var rawTotal float64
	var pricedTokens, unpricedTokens, itemCost int64
	for _, spec := range specs {
		item := pricingLineItem{
			Category: spec.category, Modality: spec.modality, Tokens: spec.tokens, RateSource: spec.rateSource,
			BaseRatePerMillion: spec.baseRate, SpeedMultiplier: spec.speedMultiplier,
		}
		if spec.rate == nil {
			var ok bool
			unpricedTokens, ok = addPricingReceiptValues(unpricedTokens, spec.tokens)
			if !ok {
				return nil, 0, 0, 0, errors.New("usage: pricing receipt unpriced token total overflows int64")
			}
			items = append(items, item)
			fractions = append(fractions, -1)
			continue
		}
		rate := *spec.rate
		if err := validateCostRate(string(spec.category), rate, model); err != nil {
			return nil, 0, 0, 0, err
		}
		raw := float64(spec.tokens) * rate
		if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw >= math.Exp2(63) {
			return nil, 0, 0, 0, fmt.Errorf("usage: cost_micro out of int64 range for model %q", model)
		}
		cost := int64(raw)
		rateCopy := rate
		item.RatePerMillion = &rateCopy
		item.CostMicro = cost
		item.Priced = true
		items = append(items, item)
		fractions = append(fractions, raw-float64(cost))
		rawTotal += raw
		var ok bool
		pricedTokens, ok = addPricingReceiptValues(pricedTokens, spec.tokens)
		if !ok {
			return nil, 0, 0, 0, errors.New("usage: pricing receipt priced token total overflows int64")
		}
		itemCost, ok = addPricingReceiptValues(itemCost, cost)
		if !ok {
			return nil, 0, 0, 0, errors.New("usage: pricing receipt cost total overflows int64")
		}
	}
	if math.IsNaN(rawTotal) || math.IsInf(rawTotal, 0) || rawTotal < 0 || rawTotal >= math.Exp2(63) {
		return nil, 0, 0, 0, fmt.Errorf("usage: cost_micro out of int64 range for model %q", model)
	}
	totalCost := int64(rawTotal)
	for remainder := totalCost - itemCost; remainder > 0; remainder-- {
		best := -1
		for index, fraction := range fractions {
			if fraction < 0 {
				continue
			}
			if best < 0 || fraction > fractions[best] {
				best = index
			}
		}
		if best < 0 {
			return nil, 0, 0, 0, errors.New("usage: pricing receipt cost remainder has no priced line item")
		}
		items[best].CostMicro++
		fractions[best] = -1
	}
	return items, totalCost, pricedTokens, unpricedTokens, nil
}

func setUnpricedPricingReceipt(receipt *PricingReceipt, amounts pricingTokenAmounts, reason string) {
	if receipt == nil {
		return
	}
	receipt.CostStatus = CostStatusUnpriced
	receipt.CostReason = strings.TrimSpace(reason)
	receipt.Coverage.PricedTokens = 0
	receipt.Coverage.UnpricedTokens = amounts.total
	receipt.LineItems = make([]pricingLineItem, 0, maxPricingReceiptLineItems)
	for _, item := range []struct {
		category pricingLineItemCategory
		tokens   int64
	}{
		{category: pricingLineItemUncachedInput, tokens: amounts.uncachedInput},
		{category: pricingLineItemCacheRead, tokens: amounts.cacheRead},
		{category: pricingLineItemCacheWrite5m, tokens: amounts.cacheWrite5m},
		{category: pricingLineItemCacheWrite1h, tokens: amounts.cacheWrite1h},
		{category: pricingLineItemCacheWrite30m, tokens: amounts.cacheWrite30m},
		{category: pricingLineItemCacheWrite12h, tokens: amounts.cacheWrite12h},
		{category: pricingLineItemCacheWriteUnknown, tokens: amounts.cacheWriteUnknown},
		{category: pricingLineItemOutput, tokens: amounts.output},
		{category: pricingLineItemReasoning, tokens: amounts.reasoning},
		{category: pricingLineItemUnclassified, tokens: amounts.unclassified},
	} {
		if item.tokens > 0 {
			receipt.LineItems = append(
				receipt.LineItems, pricingLineItem{
					Category: item.category, Tokens: item.tokens, RateSource: "unpriced",
				},
			)
		}
	}
}

func pricingReceiptResult(receipt *PricingReceipt) costResult {
	if receipt == nil {
		return costResult{}
	}
	return costResult{
		Micro: receipt.CostMicro, Status: receipt.CostStatus, Reason: receipt.CostReason,
	}
}

func validateBuiltPricingReceipt(receipt *PricingReceipt) error {
	if requiresExtendedPricingReceipt(receipt) {
		receipt.SchemaVersion = extendedPricingReceiptSchemaVersion
	}
	if err := validatePricingReceipt(receipt); err != nil {
		return fmt.Errorf("usage: build pricing receipt: %w", err)
	}
	return nil
}

func validPricingLineItemCategory(category pricingLineItemCategory) bool {
	switch category {
	case pricingLineItemUncachedInput,
		pricingLineItemCacheRead,
		pricingLineItemCacheWrite5m,
		pricingLineItemCacheWrite1h,
		pricingLineItemCacheWrite30m,
		pricingLineItemCacheWrite12h,
		pricingLineItemCacheWriteUnknown,
		pricingLineItemOutput,
		pricingLineItemReasoning,
		pricingLineItemUnclassified:
		return true
	default:
		return false
	}
}

func validPricingCostStatus(status CostStatus) bool {
	switch status {
	case costStatusComplete, costStatusPartial, CostStatusUnpriced:
		return true
	default:
		return false
	}
}

func pricingCoverageTokensByCategory(coverage pricingCoverage, category pricingLineItemCategory) int64 {
	switch category {
	case pricingLineItemUncachedInput:
		return coverage.UncachedInputTokens
	case pricingLineItemCacheRead:
		return coverage.CacheReadTokens
	case pricingLineItemCacheWrite5m:
		return coverage.CacheWrite5mTokens
	case pricingLineItemCacheWrite1h:
		return coverage.CacheWrite1hTokens
	case pricingLineItemCacheWrite30m:
		return coverage.CacheWrite30mTokens
	case pricingLineItemCacheWrite12h:
		return coverage.CacheWrite12hTokens
	case pricingLineItemCacheWriteUnknown:
		return coverage.CacheWriteUnknownTokens
	case pricingLineItemOutput:
		return coverage.OutputTokens
	case pricingLineItemReasoning:
		return coverage.ReasoningTokens
	case pricingLineItemUnclassified:
		return coverage.UnclassifiedTokens
	default:
		return 0
	}
}

func validatePricingReceipt(receipt *PricingReceipt) error {
	if receipt == nil {
		return nil
	}
	if receipt.SchemaVersion != PricingReceiptSchemaVersion && receipt.SchemaVersion != extendedPricingReceiptSchemaVersion {
		return fmt.Errorf("usage: unsupported pricing receipt schema version %d", receipt.SchemaVersion)
	}
	if receipt.SchemaVersion == PricingReceiptSchemaVersion && requiresExtendedPricingReceipt(receipt) {
		return errors.New("usage: extended pricing evidence requires receipt schema version 2")
	}
	if receipt.CapturedAt.IsZero() || receipt.PricedAt.IsZero() {
		return errors.New("usage: pricing receipt timestamps must be set")
	}
	for _, field := range []struct {
		name     string
		value    string
		required bool
		limit    int
	}{
		{name: "provider", value: receipt.Provider, limit: maxPricingReceiptStringBytes},
		{name: "model", value: receipt.Model, required: true, limit: maxPricingReceiptStringBytes},
		{name: "canonical_model", value: receipt.CanonicalModel, required: true, limit: maxPricingReceiptStringBytes},
		{name: "price_revision", value: receipt.PriceRevision, limit: maxPricingReceiptStringBytes},
		{name: "source", value: receipt.Source, required: true, limit: maxPricingReceiptStringBytes},
		{name: "currency", value: receipt.Currency, required: true, limit: maxPricingReceiptStringBytes},
		{name: "effective_from", value: receipt.EffectiveFrom, limit: maxPricingReceiptStringBytes},
		{name: "effective_until", value: receipt.EffectiveUntil, limit: maxPricingReceiptStringBytes},
		{name: "speed_rate_source", value: receipt.SpeedRateSource, limit: maxPricingReceiptStringBytes},
		{name: "speed_effective_from", value: receipt.SpeedEffectiveFrom, limit: maxPricingReceiptStringBytes},
		{name: "speed_effective_until", value: receipt.SpeedEffectiveUntil, limit: maxPricingReceiptStringBytes},
		{name: "cost_reason", value: receipt.CostReason, limit: maxPricingReceiptReasonBytes},
	} {
		if field.required && strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("usage: pricing receipt %s must be set", field.name)
		}
		if field.value != strings.TrimSpace(field.value) {
			return fmt.Errorf("usage: pricing receipt %s must be trimmed", field.name)
		}
		if len(field.value) > field.limit {
			return fmt.Errorf("usage: pricing receipt %s exceeds size limit", field.name)
		}
	}
	switch receipt.Speed {
	case "":
		if receipt.SpeedSource != "" || receipt.SpeedMultiplier != nil {
			return errors.New("usage: pricing receipt speed evidence is incomplete")
		}
	case "unknown":
		if receipt.SpeedSource != "unknown" || receipt.SpeedMultiplier != nil {
			return errors.New("usage: pricing receipt unknown speed cannot have a multiplier")
		}
	case "standard", "fast", "ultrafast", "flex", "batch":
		if receipt.SpeedSource != "response_speed" && receipt.SpeedSource != "response_service_tier" {
			return errors.New("usage: pricing receipt speed requires response evidence")
		}
	default:
		return errors.New("usage: pricing receipt speed is invalid")
	}
	if receipt.SpeedMultiplier != nil && (math.IsNaN(*receipt.SpeedMultiplier) ||
		math.IsInf(*receipt.SpeedMultiplier, 0) || *receipt.SpeedMultiplier <= 0) {
		return errors.New("usage: pricing receipt speed multiplier must be finite and positive")
	}
	if receipt.Currency != strings.ToUpper(receipt.Currency) {
		return errors.New("usage: pricing receipt currency must be uppercase")
	}
	if receipt.CostMicro < 0 || !validPricingCostStatus(receipt.CostStatus) {
		return errors.New("usage: pricing receipt has invalid cost")
	}
	coverage := receipt.Coverage
	coverageValues := []int64{
		coverage.UncachedInputTokens,
		coverage.CacheReadTokens,
		coverage.CacheWrite5mTokens,
		coverage.CacheWrite1hTokens,
		coverage.CacheWrite30mTokens,
		coverage.CacheWrite12hTokens,
		coverage.CacheWriteUnknownTokens,
		coverage.OutputTokens,
		coverage.ReasoningTokens,
		coverage.UnclassifiedTokens,
		coverage.PricedTokens,
		coverage.UnpricedTokens,
		coverage.TotalTokens,
	}
	for _, value := range coverageValues {
		if value < 0 {
			return errors.New("usage: pricing receipt coverage must be non-negative")
		}
	}
	classified, ok := addPricingReceiptValues(coverageValues[:10]...)
	if !ok || classified != coverage.TotalTokens {
		return errors.New("usage: pricing receipt category coverage does not match total tokens")
	}
	covered, ok := addPricingReceiptValues(coverage.PricedTokens, coverage.UnpricedTokens)
	if !ok || covered != coverage.TotalTokens {
		return errors.New("usage: pricing receipt priced coverage does not match total tokens")
	}
	if len(receipt.LineItems) > maxPricingReceiptLineItems {
		return errors.New("usage: pricing receipt has too many line items")
	}
	seen := make(map[string]struct{}, len(receipt.LineItems))
	categoryTokens := make(map[pricingLineItemCategory]int64)
	var lineTokens, pricedTokens, unpricedTokens, lineCost int64
	for index := range receipt.LineItems {
		item := receipt.LineItems[index]
		if !validPricingLineItemCategory(item.Category) {
			return fmt.Errorf("usage: pricing receipt line item %d has invalid category", index)
		}
		key := string(item.Category) + "\x00" + item.Modality
		if _, exists := seen[key]; exists {
			return fmt.Errorf("usage: pricing receipt line item %d repeats category %q", index, item.Category)
		}
		seen[key] = struct{}{}
		if item.Tokens <= 0 || item.CostMicro < 0 || strings.TrimSpace(item.RateSource) == "" ||
			item.RateSource != strings.TrimSpace(
				item.RateSource,
			) || len(item.RateSource) > maxPricingReceiptStringBytes {
			return fmt.Errorf("usage: pricing receipt line item %d is invalid", index)
		}
		if item.Modality != "" && item.Modality != "text" && item.Modality != "image" && item.Modality != "audio" && item.Modality != "video" {
			return fmt.Errorf("usage: pricing receipt line item %d has invalid modality", index)
		}
		categoryTotal, categoryOK := addPricingReceiptValues(categoryTokens[item.Category], item.Tokens)
		if !categoryOK || categoryTotal > pricingCoverageTokensByCategory(coverage, item.Category) {
			return fmt.Errorf("usage: pricing receipt line item %d does not match coverage", index)
		}
		categoryTokens[item.Category] = categoryTotal
		if item.BaseRatePerMillion != nil || item.SpeedMultiplier != nil {
			if !item.Priced || item.BaseRatePerMillion == nil || item.SpeedMultiplier == nil ||
				receipt.SpeedMultiplier == nil || item.RatePerMillion == nil {
				return fmt.Errorf("usage: pricing receipt line item %d has incomplete speed pricing", index)
			}
			base, multiplier := *item.BaseRatePerMillion, *item.SpeedMultiplier
			if math.IsNaN(base) || math.IsInf(base, 0) || base < 0 ||
				math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier < 0 ||
				base*multiplier != *item.RatePerMillion {
				return fmt.Errorf("usage: pricing receipt line item %d has invalid speed pricing", index)
			}
		} else if item.Priced && receipt.SpeedMultiplier != nil {
			return fmt.Errorf("usage: pricing receipt line item %d is missing speed pricing", index)
		}
		if item.Priced {
			if item.RatePerMillion == nil || math.IsNaN(*item.RatePerMillion) ||
				math.IsInf(*item.RatePerMillion, 0) || *item.RatePerMillion < 0 {
				return fmt.Errorf("usage: pricing receipt line item %d has invalid rate", index)
			}
			pricedTokens, ok = addPricingReceiptValues(pricedTokens, item.Tokens)
		} else {
			if item.RatePerMillion != nil || item.CostMicro != 0 {
				return fmt.Errorf("usage: pricing receipt line item %d has an unpriced amount", index)
			}
			unpricedTokens, ok = addPricingReceiptValues(unpricedTokens, item.Tokens)
		}
		if !ok {
			return errors.New("usage: pricing receipt line item token total overflows int64")
		}
		lineTokens, ok = addPricingReceiptValues(lineTokens, item.Tokens)
		if !ok {
			return errors.New("usage: pricing receipt line item token total overflows int64")
		}
		lineCost, ok = addPricingReceiptValues(lineCost, item.CostMicro)
		if !ok {
			return errors.New("usage: pricing receipt line item cost total overflows int64")
		}
	}
	additionalCost, err := validateAdditionalLineItems(receipt.AdditionalLineItems)
	if err != nil {
		return err
	}
	if err := validatePricingBillingMetadata(receipt.BillingMetadata); err != nil {
		return err
	}
	lineCost, ok = addPricingReceiptValues(lineCost, additionalCost)
	if !ok {
		return errors.New("usage: pricing receipt total cost overflows int64")
	}
	if len(receipt.ConditionIDs) > maxPricingReceiptLineItems {
		return errors.New("usage: pricing receipt has too many condition ids")
	}
	for _, id := range receipt.ConditionIDs {
		if strings.TrimSpace(id) == "" || len(id) > maxPricingReceiptStringBytes {
			return errors.New("usage: invalid pricing condition id")
		}
	}
	if lineTokens != coverage.TotalTokens || pricedTokens != coverage.PricedTokens ||
		unpricedTokens != coverage.UnpricedTokens || lineCost != receipt.CostMicro {
		return errors.New("usage: pricing receipt line items do not reconcile")
	}
	return nil
}

func addPricingReceiptValues(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func clonePricingReceipt(receipt *PricingReceipt) *PricingReceipt {
	if receipt == nil {
		return nil
	}
	cloned := *receipt
	cloned.BillingMetadata = coreusage.CloneBillingMetadata(receipt.BillingMetadata)
	cloned.ConditionIDs = append([]string(nil), receipt.ConditionIDs...)
	cloned.CacheWriteReported = coreusage.NormalizeCacheWriteReported(receipt.CacheWriteReported, 0)
	if receipt.CacheReadReported != nil {
		reported := *receipt.CacheReadReported
		cloned.CacheReadReported = &reported
	}
	cloned.AdditionalLineItems = append([]pricingAdditionalLineItem(nil), receipt.AdditionalLineItems...)
	for index := range cloned.AdditionalLineItems {
		item := &cloned.AdditionalLineItems[index]
		for _, field := range []**float64{&item.Price, &item.PerUnits} {
			if *field != nil {
				value := **field
				*field = &value
			}
		}
	}
	if receipt.SpeedMultiplier != nil {
		multiplier := *receipt.SpeedMultiplier
		cloned.SpeedMultiplier = &multiplier
	}
	cloned.LineItems = make([]pricingLineItem, len(receipt.LineItems))
	copy(cloned.LineItems, receipt.LineItems)
	for index := range cloned.LineItems {
		item := &cloned.LineItems[index]
		for _, field := range []**float64{&item.RatePerMillion, &item.BaseRatePerMillion, &item.SpeedMultiplier} {
			if *field != nil {
				rate := **field
				*field = &rate
			}
		}
	}
	return &cloned
}

func requiresExtendedPricingReceipt(receipt *PricingReceipt) bool {
	if receipt == nil {
		return false
	}
	if receipt.SchemaVersion == extendedPricingReceiptSchemaVersion || len(receipt.LineItems) > 8 || len(receipt.AdditionalLineItems) > 0 {
		return true
	}
	for _, item := range receipt.LineItems {
		if item.Modality != "" || item.Category == pricingLineItemCacheWrite30m || item.Category == pricingLineItemCacheWrite12h {
			return true
		}
	}
	return false
}

// Schema 2 lives in extensible metadata so older readers can still read the event and its stored amount.
func encodeStoredPricingReceipt(receipt *PricingReceipt, metadata any) (any, any, error) {
	encoded, err := encodePricingReceipt(receipt)
	if err != nil || receipt == nil || !requiresExtendedPricingReceipt(receipt) {
		return encoded, metadata, err
	}
	fields := make(map[string]json.RawMessage)
	if metadata != nil {
		raw, ok := metadata.(string)
		if !ok {
			return nil, nil, errors.New("usage: event metadata must be encoded JSON")
		}
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil, nil, fmt.Errorf("usage: preserve extended pricing metadata: %w", err)
		}
	}
	fields["pricing_receipt_v2"] = json.RawMessage(encoded.(string))
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	return nil, string(raw), nil
}

func encodePricingReceipt(receipt *PricingReceipt) (any, error) {
	if receipt == nil {
		return nil, nil
	}
	if err := validatePricingReceipt(receipt); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("usage: encode pricing receipt: %w", err)
	}
	if len(encoded) > maxPricingReceiptBytes {
		return nil, errors.New("usage: pricing receipt exceeds size limit")
	}
	return string(encoded), nil
}

func decodePricingReceipt(raw sql.NullString, metadata ...sql.NullString) (*PricingReceipt, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		if len(metadata) == 0 || !metadata[0].Valid || strings.TrimSpace(metadata[0].String) == "" {
			return nil, nil
		}
		storedMetadata := strings.TrimSpace(metadata[0].String)
		if !json.Valid([]byte(storedMetadata)) || storedMetadata[0] != '{' {
			return nil, nil
		}
		var extended struct {
			Receipt json.RawMessage `json:"pricing_receipt_v2"`
		}
		if err := json.Unmarshal([]byte(storedMetadata), &extended); err != nil {
			return nil, fmt.Errorf("usage: decode extended pricing metadata: %w", err)
		}
		if len(extended.Receipt) == 0 || string(extended.Receipt) == "null" {
			return nil, nil
		}
		raw = sql.NullString{String: string(extended.Receipt), Valid: true}
	}
	if len(raw.String) > maxPricingReceiptBytes {
		return nil, errors.New("usage: stored pricing receipt exceeds size limit")
	}
	var receipt PricingReceipt
	if err := json.Unmarshal([]byte(raw.String), &receipt); err != nil {
		return nil, fmt.Errorf("usage: decode pricing receipt: %w", err)
	}
	if err := validatePricingReceipt(&receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}
