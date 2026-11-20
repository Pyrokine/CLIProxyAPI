package modelcatalog

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

var tokenRateBuckets = []string{
	"prompt", "completion", "cache_read", "cache_write", "cache_write_5m", "cache_write_1h",
	"cache_write_30m", "cache_write_12h",
}

func (r RateOverrides) Rate(bucket string) *float64 {
	switch bucket {
	case "prompt":
		return r.Prompt
	case "completion":
		return r.Completion
	case "cache_read":
		return r.CacheRead
	case "cache_write":
		return r.CacheWrite
	case "cache_write_5m":
		return r.CacheWrite5m
	case "cache_write_1h":
		return r.CacheWrite1h
	case "cache_write_30m":
		return r.CacheWrite30m
	case "cache_write_12h":
		return r.CacheWrite12h
	default:
		return nil
	}
}

func (r RateOverrides) Clone() RateOverrides {
	r.Prompt = cloneFloat64(r.Prompt)
	r.Completion = cloneFloat64(r.Completion)
	r.CacheRead = cloneFloat64(r.CacheRead)
	r.CacheWrite = cloneFloat64(r.CacheWrite)
	r.CacheWrite5m = cloneFloat64(r.CacheWrite5m)
	r.CacheWrite1h = cloneFloat64(r.CacheWrite1h)
	r.CacheWrite30m = cloneFloat64(r.CacheWrite30m)
	r.CacheWrite12h = cloneFloat64(r.CacheWrite12h)
	return r
}

func (r ConditionalRate) Clone() ConditionalRate {
	r.RateOverrides = r.RateOverrides.Clone()
	r.Multiplier = cloneFloat64(r.Multiplier)
	if r.MinInputTokens != nil {
		value := *r.MinInputTokens
		r.MinInputTokens = &value
	}
	if r.MaxInputTokens != nil {
		value := *r.MaxInputTokens
		r.MaxInputTokens = &value
	}
	r.UnpricedBuckets = slices.Clone(r.UnpricedBuckets)
	return r
}

func (r ConditionalRate) EffectiveAt(at time.Time) bool {
	return (Price{EffectiveFrom: r.EffectiveFrom, EffectiveUntil: r.EffectiveUntil}).EffectiveAt(at)
}

// Specificity gives channel, geography and speed evidence precedence over a generic length row.
func (r ConditionalRate) Specificity() int {
	value := 0
	if r.Provider != "" {
		value += 16
	}
	if r.Region != "" {
		value += 8
	}
	if r.Speed != "" {
		value += 4
	}
	if r.MinInputTokens != nil {
		value++
	}
	if r.MaxInputTokens != nil {
		value++
	}
	return value
}

func validateRateOverrides(r RateOverrides) error {
	for _, bucket := range tokenRateBuckets {
		if rate := r.Rate(bucket); rate != nil && (math.IsNaN(*rate) || math.IsInf(*rate, 0) || *rate < 0) {
			return fmt.Errorf("invalid %s price", bucket)
		}
	}
	return nil
}

func validateConditionalRates(rules []ConditionalRate) error {
	ids := make(map[string]struct{})
	for index, rule := range rules {
		if rule.ID != "" {
			if _, exists := ids[rule.ID]; exists {
				return fmt.Errorf("conditional_rates[%d]: duplicate id %q", index, rule.ID)
			}
			ids[rule.ID] = struct{}{}
		}
		if rule.MinInputTokens != nil && *rule.MinInputTokens < 0 || rule.MaxInputTokens != nil && *rule.MaxInputTokens < 0 {
			return fmt.Errorf("conditional_rates[%d]: input token bounds must be non-negative", index)
		}
		if rule.MinInputTokens != nil && rule.MaxInputTokens != nil && *rule.MinInputTokens > *rule.MaxInputTokens {
			return fmt.Errorf("conditional_rates[%d]: input token bounds are reversed", index)
		}
		if rule.Speed != "" && !slices.Contains(
			[]string{"standard", "fast", "ultrafast", "flex", "batch"}, rule.Speed,
		) {
			return fmt.Errorf("conditional_rates[%d]: invalid speed", index)
		}
		if err := validateRateOverrides(rule.RateOverrides); err != nil {
			return fmt.Errorf("conditional_rates[%d]: %w", index, err)
		}
		from, hasFrom := parsePriceBoundary(rule.EffectiveFrom, false)
		until, hasUntil := parsePriceBoundary(rule.EffectiveUntil, true)
		if rule.EffectiveFrom != "" && !hasFrom || rule.EffectiveUntil != "" && !hasUntil || hasFrom && hasUntil && !from.Before(until) {
			return fmt.Errorf("conditional_rates[%d]: invalid effective interval", index)
		}
		hasRate := false
		for _, bucket := range tokenRateBuckets {
			hasRate = hasRate || rule.Rate(bucket) != nil
		}
		if rule.Multiplier != nil {
			if rule.Region == "" || math.IsNaN(*rule.Multiplier) || math.IsInf(
				*rule.Multiplier, 0,
			) || *rule.Multiplier <= 0 {
				return fmt.Errorf("conditional_rates[%d]: regional multiplier must be finite and positive", index)
			}
			if hasRate || len(rule.UnpricedBuckets) > 0 {
				return fmt.Errorf("conditional_rates[%d]: multiplier and absolute rates require separate rows", index)
			}
		}
		for _, bucket := range rule.UnpricedBuckets {
			if !slices.Contains(tokenRateBuckets, bucket) || rule.Rate(bucket) != nil {
				return fmt.Errorf("conditional_rates[%d]: invalid unpriced bucket %q", index, bucket)
			}
		}
		if !hasRate && rule.Multiplier == nil && len(rule.UnpricedBuckets) == 0 {
			return fmt.Errorf("conditional_rates[%d]: a rate, multiplier or unpriced bucket is required", index)
		}
		for earlier := 0; earlier < index; earlier++ {
			other := rules[earlier]
			if rule.Specificity() != other.Specificity() || !conditionalRatesOverlap(rule, other) {
				continue
			}
			if rule.Multiplier != nil && other.Multiplier != nil {
				return fmt.Errorf("conditional_rates[%d]: ambiguous regional multiplier", index)
			}
			for _, bucket := range tokenRateBuckets {
				if (rule.Rate(bucket) != nil || slices.Contains(rule.UnpricedBuckets, bucket)) &&
					(other.Rate(bucket) != nil || slices.Contains(other.UnpricedBuckets, bucket)) {
					return fmt.Errorf("conditional_rates[%d]: overlapping %s rates", index, bucket)
				}
			}
		}
	}
	return nil
}

func conditionalRatesOverlap(left, right ConditionalRate) bool {
	for _, pair := range [][2]string{
		{left.Provider, right.Provider}, {left.Region, right.Region}, {left.Speed, right.Speed},
	} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return false
		}
	}
	if left.MaxInputTokens != nil && right.MinInputTokens != nil && *left.MaxInputTokens < *right.MinInputTokens ||
		right.MaxInputTokens != nil && left.MinInputTokens != nil && *right.MaxInputTokens < *left.MinInputTokens {
		return false
	}
	leftFrom, lf := parsePriceBoundary(left.EffectiveFrom, false)
	leftUntil, lu := parsePriceBoundary(left.EffectiveUntil, true)
	rightFrom, rf := parsePriceBoundary(right.EffectiveFrom, false)
	rightUntil, ru := parsePriceBoundary(right.EffectiveUntil, true)
	return !(lu && rf && !rightFrom.Before(leftUntil) || ru && lf && !leftFrom.Before(rightUntil))
}

func (p Price) validateConditionalPricing() error {
	if err := validateConditionalRates(p.ConditionalRates); err != nil {
		return err
	}
	modalities := make(map[string]struct{})
	for index, rule := range p.ModalityRates {
		if !slices.Contains([]string{"text", "image", "audio", "video"}, rule.Modality) {
			return fmt.Errorf("modality_rates[%d]: invalid modality", index)
		}
		if _, exists := modalities[rule.Modality]; exists {
			return fmt.Errorf("modality_rates[%d]: duplicate modality", index)
		}
		modalities[rule.Modality] = struct{}{}
		if err := validateRateOverrides(rule.RateOverrides); err != nil {
			return fmt.Errorf("modality_rates[%d]: %w", index, err)
		}
	}
	seenCharges := make(map[string]struct{})
	for index, rate := range p.ChargeRates {
		if strings.TrimSpace(rate.Kind) == "" || strings.TrimSpace(rate.Unit) == "" ||
			math.IsNaN(rate.Price) || math.IsInf(rate.Price, 0) || rate.Price < 0 ||
			math.IsNaN(rate.PerUnits) || math.IsInf(rate.PerUnits, 0) || rate.PerUnits <= 0 {
			return fmt.Errorf("charge_rates[%d]: invalid charge rate", index)
		}
		if rate.Speed != "" && !slices.Contains(
			[]string{"standard", "fast", "ultrafast", "flex", "batch"}, rate.Speed,
		) {
			return fmt.Errorf("charge_rates[%d]: invalid speed", index)
		}
		key := strings.Join(
			[]string{rate.Kind, rate.Unit, rate.Provider, rate.Region, rate.Resolution, rate.Speed}, "\x00",
		)
		if _, exists := seenCharges[key]; exists {
			return fmt.Errorf("charge_rates[%d]: duplicate charge rate", index)
		}
		seenCharges[key] = struct{}{}
	}
	for index, gap := range p.CoverageGaps {
		if !slices.Contains(
			[]string{
				"input_length", "cache_write_ttl", "region", "modality", "tools", "calendar", "unconditional",
			}, gap.Dimension,
		) || strings.TrimSpace(gap.Reason) == "" {
			return fmt.Errorf("coverage_gaps[%d]: invalid coverage gap", index)
		}
	}
	return nil
}
