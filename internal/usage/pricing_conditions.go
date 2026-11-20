package usage

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func pricingBucket(category pricingLineItemCategory) string {
	switch category {
	case pricingLineItemUncachedInput:
		return "prompt"
	case pricingLineItemOutput, pricingLineItemReasoning:
		return "completion"
	case pricingLineItemCacheWriteUnknown:
		return "cache_write"
	default:
		return string(category)
	}
}

func pricingRegionMatches(region string, detail *FlatDetail) bool {
	if region == "" {
		return true
	}
	metadata := detail.BillingMetadata
	if metadata == nil {
		return false
	}
	actual := strings.TrimSpace(metadata.Region)
	return region == actual || region == "non-global" && detail.Provider == "vertex" && metadata.RegionSource == "endpoint.locations" && actual != "" && actual != "global"
}

func conditionalRatePriority(rule modelcatalog.ConditionalRate, detail *FlatDetail) int {
	priority := rule.Specificity() * 2
	if rule.Region != "" && detail.BillingMetadata != nil && rule.Region == strings.TrimSpace(detail.BillingMetadata.Region) {
		priority++
	}
	return priority
}

func conditionalRateMatches(
	rule modelcatalog.ConditionalRate,
	detail *FlatDetail,
	speed string,
	input *int64,
	at time.Time,
) bool {
	if rule.Provider != "" && rule.Provider != detail.Provider || !pricingRegionMatches(rule.Region, detail) ||
		rule.Speed != "" && rule.Speed != speed || !rule.EffectiveAt(at) {
		return false
	}
	if rule.MinInputTokens != nil || rule.MaxInputTokens != nil {
		if input == nil || rule.MinInputTokens != nil && *input < *rule.MinInputTokens ||
			rule.MaxInputTokens != nil && *input > *rule.MaxInputTokens {
			return false
		}
	}
	return true
}

func selectConditionalTokenRate(
	price detailedPrice,
	bucket string,
	detail *FlatDetail,
	speed string,
	input *int64,
	at time.Time,
) *modelcatalog.ConditionalRate {
	var base, final *modelcatalog.ConditionalRate
	for index := range price.ConditionalRates {
		rule := &price.ConditionalRates[index]
		if rule.Multiplier != nil || !conditionalRateMatches(*rule, detail, speed, input, at) ||
			rule.Rate(bucket) == nil && !slices.Contains(rule.UnpricedBuckets, bucket) {
			continue
		}
		if rule.Speed != "" {
			if final == nil || conditionalRatePriority(*rule, detail) > conditionalRatePriority(*final, detail) {
				final = rule
			}
		} else if base == nil || conditionalRatePriority(*rule, detail) > conditionalRatePriority(*base, detail) {
			base = rule
		}
	}
	if final != nil {
		return final
	}
	return base
}

func unreportedCacheChangesInputRate(
	price detailedPrice,
	detail *FlatDetail,
	speed string,
	at time.Time,
	modality string,
	write bool,
) bool {
	input := pricingContextInput(detail)
	prompt, promptKnown, promptFinal := price.Prompt, price.PromptKnown == nil || *price.PromptKnown, false
	if modality != "" {
		promptKnown = false
		for _, rate := range price.ModalityRates {
			if rate.Modality == modality && rate.Prompt != nil {
				prompt, promptKnown = *rate.Prompt, true
			}
		}
	}
	if rule := selectConditionalTokenRate(price, "prompt", detail, speed, input, at); rule != nil {
		promptKnown, promptFinal = rule.Prompt != nil, rule.Speed != ""
		if promptKnown {
			prompt = *rule.Prompt
		}
	}
	speedRule, hasSpeedRule, _ := selectSpeedPrice(price, speed, detail.Provider, at)
	if hasSpeedRule && !promptFinal {
		prompt *= pricingSpeedMultiplier(speedRule, pricingLineItemUncachedInput)
	}
	categories := []pricingLineItemCategory{pricingLineItemCacheRead}
	if write {
		categories = []pricingLineItemCategory{
			pricingLineItemCacheWriteUnknown, pricingLineItemCacheWrite5m, pricingLineItemCacheWrite1h,
			pricingLineItemCacheWrite30m, pricingLineItemCacheWrite12h,
		}
	}
	for _, category := range categories {
		rate, known, final := price.Prompt, false, false
		if write {
			var source string
			rate, source = receiptCacheWriteRate(price, category)
			known = source != "prompt_fallback"
		} else if price.CacheRead != nil {
			rate, known = *price.CacheRead, true
		}
		if modality != "" {
			known = false
			for _, item := range price.ModalityRates {
				if item.Modality == modality {
					if value := item.Rate(pricingBucket(category)); value != nil {
						rate, known = *value, true
					}
				}
			}
		}
		if rule := selectConditionalTokenRate(price, pricingBucket(category), detail, speed, input, at); rule != nil {
			if value := rule.Rate(pricingBucket(category)); value != nil {
				rate, known, final = *value, true, rule.Speed != ""
			} else {
				return true
			}
		}
		if write && !known {
			continue
		}
		if hasSpeedRule && !final {
			rate *= pricingSpeedMultiplier(speedRule, category)
		}
		if !known || !promptKnown || rate != prompt {
			return true
		}
	}
	return false
}

func validatePricingBillingMetadata(metadata *coreusage.BillingMetadata) error {
	if metadata == nil {
		return nil
	}
	if metadata.SchemaVersion != 1 || metadata.ContextInputTokens != nil && *metadata.ContextInputTokens < 0 {
		return errors.New("usage: invalid billing metadata version or context input tokens")
	}
	if len(metadata.Modalities) > maxPricingReceiptLineItems || len(metadata.Charges) > maxPricingReceiptLineItems ||
		len(metadata.Region) > maxPricingReceiptStringBytes || len(metadata.RegionSource) > maxPricingReceiptStringBytes {
		return errors.New("usage: billing metadata exceeds size limits")
	}
	for _, item := range metadata.Modalities {
		if !validPricingLineItemCategory(pricingLineItemCategory(item.Category)) || item.Tokens < 0 ||
			!slices.Contains([]string{"text", "image", "audio", "video"}, item.Modality) {
			return errors.New("usage: invalid modality token usage")
		}
	}
	for _, item := range metadata.Charges {
		if strings.TrimSpace(item.Kind) == "" || strings.TrimSpace(item.Unit) == "" ||
			math.IsNaN(item.Quantity) || math.IsInf(item.Quantity, 0) || item.Quantity < 0 ||
			len(item.Kind) > maxPricingReceiptStringBytes || len(item.Unit) > maxPricingReceiptStringBytes ||
			len(item.Resolution) > maxPricingReceiptStringBytes {
			return errors.New("usage: invalid additional billing quantity")
		}
	}
	return nil
}

func pricingContextInput(detail *FlatDetail) *int64 {
	if detail.BillingMetadata != nil {
		return detail.BillingMetadata.ContextInputTokens
	}
	// Gemini's canonical input also contains tool-use prompts, which are separate from the priced prompt length.
	provider := strings.ToLower(detail.Provider + " " + detail.ExecutorType)
	if strings.Contains(provider, "gemini") || strings.Contains(provider, "vertex") || strings.Contains(
		provider, "antigravity",
	) || strings.Contains(provider, "aistudio") {
		return nil
	}
	if detail.TokenBreakdown.Valid() && detail.TokenBreakdown.Quality != coreusage.TokenAccountingQualityInconsistent {
		value := detail.TokenBreakdown.Input.TotalTokens
		return &value
	}
	return nil
}

func resolvePricingLineSpecs(
	specs []pricingLineSpec,
	price detailedPrice,
	detail *FlatDetail,
	speed string,
	at time.Time,
) ([]pricingLineSpec, []string, []string, bool, error) {
	if err := validatePricingBillingMetadata(detail.BillingMetadata); err != nil {
		return nil, nil, nil, false, err
	}
	metadata := detail.BillingMetadata
	region := ""
	if metadata != nil {
		region = strings.TrimSpace(metadata.Region)
	}
	input := pricingContextInput(detail)
	inputTokens, readTokens, writeTokens := detail.Tokens.InputTokens, detail.Tokens.CacheReadTokens, detail.Tokens.CacheWriteTokens
	if detail.TokenBreakdown.Valid() {
		inputTokens, readTokens, writeTokens = detail.TokenBreakdown.Input.TotalTokens, detail.TokenBreakdown.Input.CacheReadTokens, detail.TokenBreakdown.Input.CacheWriteTokens
	}
	readUnreported := price.RequestScoped && !detail.CacheReadReported && readTokens == 0 && inputTokens > 0
	writeReported := coreusage.NormalizeCacheWriteReported(detail.CacheWriteReported, writeTokens)
	writeUnreported := price.RequestScoped && (writeReported == nil || !*writeReported) && inputTokens > 0
	unknownCacheRead := readUnreported && unreportedCacheChangesInputRate(price, detail, speed, at, "", false)
	unknownCacheWrite := writeUnreported && unreportedCacheChangesInputRate(price, detail, speed, at, "", true)
	reasons := make([]string, 0, 4)
	var ids []string
	addID := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	var regional *modelcatalog.ConditionalRate
	hasRegionalRules, hasLengthRules := false, false
	for index := range price.ConditionalRates {
		rule := &price.ConditionalRates[index]
		if rule.Provider != "" && rule.Provider != detail.Provider || rule.Speed != "" && rule.Speed != speed || !rule.EffectiveAt(at) {
			continue
		}
		hasRegionalRules = hasRegionalRules || rule.Region != ""
		if pricingRegionMatches(rule.Region, detail) {
			hasLengthRules = hasLengthRules || rule.MinInputTokens != nil || rule.MaxInputTokens != nil
		}
		if rule.Multiplier != nil && conditionalRateMatches(*rule, detail, speed, input, at) &&
			(regional == nil || conditionalRatePriority(*rule, detail) > conditionalRatePriority(*regional, detail)) {
			regional = rule
		}
	}
	if hasLengthRules && input == nil {
		reasons = append(reasons, "prompt length required for conditional pricing was not recorded")
	}
	regionMismatch := false
	if price.RequestScoped && price.Region != "" && price.Region != "unspecified" {
		if region == "" && (hasRegionalRules || price.Region != "global") {
			reasons = append(reasons, "billing region was not reported")
			regionMismatch = price.Region != "global"
		} else if region != "" && region != price.Region && regional == nil {
			regionMatched := false
			for _, rule := range price.ConditionalRates {
				regionMatched = regionMatched || rule.Region != "" && conditionalRateMatches(
					rule, detail, speed, input, at,
				)
			}
			if !regionMatched {
				reasons = append(reasons, fmt.Sprintf("no reference rates match billing region %q", region))
				regionMismatch = true
			}
		}
	}
	resolved := make([]pricingLineSpec, 0, len(specs))
	allFinal := len(specs) > 0
	for _, original := range specs {
		parts := []pricingLineSpec{original}
		if len(price.ModalityRates) > 0 && original.category != pricingLineItemUnclassified {
			parts = nil
			var accounted int64
			if metadata != nil {
				seen := make(map[string]struct{})
				for _, amount := range metadata.Modalities {
					if amount.Category != string(original.category) || amount.Tokens == 0 {
						continue
					}
					if _, exists := seen[amount.Modality]; exists {
						return nil, nil, nil, false, errors.New("usage: duplicate modality token bucket")
					}
					seen[amount.Modality] = struct{}{}
					var ok bool
					accounted, ok = addPricingReceiptValues(accounted, amount.Tokens)
					if !ok || accounted > original.tokens {
						return nil, nil, nil, false, errors.New("usage: modality tokens exceed canonical token bucket")
					}
					part := original
					part.modality, part.tokens, part.rate = amount.Modality, amount.Tokens, nil
					part.rateSource = "modality_unpriced"
					for _, rate := range price.ModalityRates {
						if rate.Modality == amount.Modality {
							part.rate = rate.Rate(pricingBucket(part.category))
							part.rateSource = "modality:" + amount.Modality
						}
					}
					parts = append(parts, part)
				}
			}
			if accounted < original.tokens {
				part := original
				part.tokens, part.rate, part.rateSource = original.tokens-accounted, nil, "modality_unrecorded"
				parts = append(parts, part)
				reasons = append(reasons, "modality token counts are incomplete")
			}
		}
		for _, part := range parts {
			modalityUnrecorded := part.rateSource == "modality_unrecorded"
			bucket := pricingBucket(part.category)
			selected := selectConditionalTokenRate(price, bucket, detail, speed, input, at)
			if selected == nil && part.modality == "" {
				if bucket == "prompt" && price.PromptKnown != nil && !*price.PromptKnown ||
					bucket == "completion" && price.CompletionKnown != nil && !*price.CompletionKnown {
					part.rate, part.rateSource = nil, "rate_unconfigured"
				}
			}
			if selected != nil {
				part.rate = selected.Rate(bucket)
				part.rateSource = "conditional:" + selected.ID
				if selected.ID == "" {
					part.rateSource = "conditional:" + selected.Source
				}
				part.finalRate = selected.Speed != ""
				if part.rate == nil {
					reasons = append(reasons, "conditional price "+selected.ID+" is unresolved for "+bucket)
				}
				addID(selected.ID)
			}
			if modalityUnrecorded {
				part.rate, part.rateSource = nil, "modality_unrecorded"
			}
			if price.RequestScoped && (part.rateSource == "prompt_fallback" || part.rateSource == "max_ttl_fallback") {
				reasons = append(reasons, "a token rate is unavailable for this request")
				part.rate, part.rateSource = nil, "rate_unavailable"
			}
			if price.RequestScoped && part.category == pricingLineItemCacheWriteUnknown {
				for _, gap := range price.CoverageGaps {
					if gap.Dimension == "cache_write_ttl" {
						part.rate, part.rateSource = nil, "cache_write_ttl_unreported"
					}
				}
			}
			if !price.HasBaseRates && price.Prompt == 0 && price.Completion == 0 && price.CacheRead == nil &&
				selected == nil && part.modality == "" {
				part.rate, part.rateSource = nil, "rate_unavailable"
			}
			if regionMismatch {
				part.rate, part.rateSource = nil, "region_unavailable"
			}
			if part.category == pricingLineItemUncachedInput {
				if readUnreported && unreportedCacheChangesInputRate(price, detail, speed, at, part.modality, false) {
					unknownCacheRead = true
					part.rate, part.rateSource = nil, "cache_read_usage_unreported"
				}
				if writeUnreported && unreportedCacheChangesInputRate(price, detail, speed, at, part.modality, true) {
					unknownCacheWrite = true
					part.rate, part.rateSource = nil, "cache_write_usage_unreported"
				}
			}
			if regional != nil && part.rate != nil {
				rate := *part.rate * *regional.Multiplier
				part.rate = &rate
				part.rateSource += "; regional:" + regional.ID
				addID(regional.ID)
			}
			allFinal = allFinal && part.finalRate
			resolved = append(resolved, part)
		}
	}
	if unknownCacheRead {
		reasons = append(reasons, "cache-read usage was not reported")
	}
	if unknownCacheWrite {
		reasons = append(reasons, "total cache-write usage was not reported")
	}
	for _, gap := range price.CoverageGaps {
		applies := false
		switch gap.Dimension {
		case "unconditional":
			applies = true
		case "input_length":
			applies = input == nil
		case "cache_write_ttl":
			for _, spec := range specs {
				applies = applies || spec.category == pricingLineItemCacheWriteUnknown && spec.tokens > 0
			}
		case "region":
			applies = region == "" || regionMismatch
		case "modality":
			applies = len(specs) > 0 && len(price.ModalityRates) > 0 && (metadata == nil || len(metadata.Modalities) == 0)
		case "tools":
			applies = metadata == nil || !metadata.ChargesReported
		case "calendar":
			applies = price.CalendarRateActive
		}
		if applies {
			reasons = append(reasons, gap.Reason)
		}
	}
	return resolved, reasons, ids, allFinal, nil
}

func priceAdditionalLineItems(
	price detailedPrice,
	metadata *coreusage.BillingMetadata,
	provider, speed string,
) ([]pricingAdditionalLineItem, int64, bool, error) {
	if err := validatePricingBillingMetadata(metadata); err != nil {
		return nil, 0, false, err
	}
	if metadata == nil {
		return nil, 0, len(price.ChargeRates) > 0 && price.RequestScoped, nil
	}
	var items []pricingAdditionalLineItem
	incomplete := !metadata.ChargesReported && (price.RequestScoped || len(price.ChargeRates) > 0)
	var total int64
	seen := make(map[string]struct{})
	for _, quantity := range metadata.Charges {
		key := strings.Join([]string{quantity.Kind, quantity.Unit, quantity.Resolution}, "\x00")
		if _, exists := seen[key]; exists {
			return nil, 0, false, errors.New("usage: duplicate additional billing quantity")
		}
		seen[key] = struct{}{}
		item := pricingAdditionalLineItem{
			Kind: quantity.Kind, Unit: quantity.Unit, Quantity: quantity.Quantity, Resolution: quantity.Resolution,
			RateSource: "unpriced",
		}
		selected := selectChargeRate(price, quantity, provider, metadata.Region, speed)
		if speed == "" || speed == "unknown" {
			selected = nil
			for _, candidateSpeed := range []string{"standard", "fast", "ultrafast", "flex", "batch"} {
				candidate := selectChargeRate(price, quantity, provider, metadata.Region, candidateSpeed)
				if candidate == nil || selected != nil && candidate.Price/candidate.PerUnits != selected.Price/selected.PerUnits {
					selected = nil
					break
				}
				selected = candidate
			}
		}
		if selected == nil {
			incomplete = incomplete || quantity.Quantity > 0
		} else {
			raw := quantity.Quantity * selected.Price / selected.PerUnits * 1_000_000
			if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw >= math.Exp2(63) {
				return nil, 0, false, errors.New("usage: additional cost exceeds int64 range")
			}
			item.Price, item.PerUnits, item.Priced, item.CostMicro = &selected.Price, &selected.PerUnits, true, int64(raw)
			item.RateSource = selected.Source
			if item.RateSource == "" {
				item.RateSource = "model-catalog"
			}
			var ok bool
			total, ok = addPricingReceiptValues(total, item.CostMicro)
			if !ok {
				return nil, 0, false, errors.New("usage: additional cost total exceeds int64 range")
			}
		}
		items = append(items, item)
	}
	return items, total, incomplete, nil
}

func validateAdditionalLineItems(items []pricingAdditionalLineItem) (int64, error) {
	if len(items) > maxPricingReceiptLineItems {
		return 0, errors.New("usage: too many additional pricing line items")
	}
	seen := make(map[string]struct{})
	var total int64
	for _, item := range items {
		key := strings.Join([]string{item.Kind, item.Unit, item.Resolution}, "\x00")
		if _, exists := seen[key]; exists {
			return 0, errors.New("usage: duplicate additional pricing line item")
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(item.Kind) == "" || strings.TrimSpace(item.Unit) == "" || strings.TrimSpace(item.RateSource) == "" ||
			len(item.Kind) > maxPricingReceiptStringBytes || len(item.Unit) > maxPricingReceiptStringBytes ||
			len(item.Resolution) > maxPricingReceiptStringBytes || len(item.RateSource) > maxPricingReceiptStringBytes ||
			math.IsNaN(item.Quantity) || math.IsInf(item.Quantity, 0) || item.Quantity < 0 || item.CostMicro < 0 {
			return 0, errors.New("usage: invalid additional pricing line item")
		}
		if item.Priced {
			if item.Price == nil || item.PerUnits == nil || math.IsNaN(*item.Price) || math.IsInf(
				*item.Price, 0,
			) || *item.Price < 0 ||
				math.IsNaN(*item.PerUnits) || math.IsInf(*item.PerUnits, 0) || *item.PerUnits <= 0 {
				return 0, errors.New("usage: invalid additional pricing rate")
			}
			raw := item.Quantity * *item.Price / *item.PerUnits * 1_000_000
			if math.IsNaN(raw) || math.IsInf(raw, 0) || raw >= math.Exp2(63) || int64(raw) != item.CostMicro {
				return 0, errors.New("usage: additional pricing amount does not match quantity and rate")
			}
		} else if item.Price != nil || item.PerUnits != nil || item.CostMicro != 0 {
			return 0, errors.New("usage: unpriced additional item has an amount")
		}
		var ok bool
		total, ok = addPricingReceiptValues(total, item.CostMicro)
		if !ok {
			return 0, errors.New("usage: additional pricing total exceeds int64 range")
		}
	}
	return total, nil
}

func selectChargeRate(
	price detailedPrice,
	quantity coreusage.BillingQuantity,
	provider, region, speed string,
) *modelcatalog.ChargeRate {
	var selected *modelcatalog.ChargeRate
	for index := range price.ChargeRates {
		rate := &price.ChargeRates[index]
		if rate.Kind != quantity.Kind || rate.Unit != quantity.Unit || rate.Provider != "" && rate.Provider != provider ||
			rate.Region != "" && rate.Region != region || rate.Resolution != "" && rate.Resolution != quantity.Resolution ||
			rate.Speed != "" && rate.Speed != speed {
			continue
		}
		if selected == nil || chargeRateSpecificity(*rate) > chargeRateSpecificity(*selected) {
			selected = rate
		}
	}
	return selected
}

func chargeRateSpecificity(rate modelcatalog.ChargeRate) int {
	specificity := 0
	if rate.Speed != "" {
		specificity += 8
	}
	if rate.Provider != "" {
		specificity += 4
	}
	if rate.Region != "" {
		specificity += 2
	}
	if rate.Resolution != "" {
		specificity++
	}
	return specificity
}
