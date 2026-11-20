package modelcatalog

import (
	"testing"
	"time"
)

func TestDefaultSpeedRatesMatchOfficialReferences(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fast := map[string]float64{
		"gpt-6.2-astra": 2, "gpt-6.2-sol": 2,
		"gpt-6-astra": 2, "gpt-6.1-sol": 2, "gpt-6.1-terra": 2,
		"gpt-6-luna": 2, "gpt-6-sol": 2,
		"gpt-5.6-sol": 2, "gpt-5.6-terra": 2, "gpt-5.6-luna": 2,
		"gpt-5.5": 2.5, "gpt-5.4": 2, "gpt-5.4-mini": 2,
		"gpt-5.2": 2, "gpt-5.1": 2, "gpt-5": 2, "gpt-5-mini": 1.8,
		"gpt-4.1": 1.75, "gpt-4.1-mini": 1.75,
		"gpt-4o": 1.7, "gpt-4o-mini": 5.0 / 3, "o3": 1.75,
		"gpt-5.3-codex": 2,
	}
	prices := DefaultPrices()
	for model, multiplier := range fast {
		assertDefaultSpeedMultiplier(t, prices, model, "openai", "fast", at, multiplier)
	}
	codexFast := map[string]float64{
		"gpt-6.2-astra": 2, "gpt-6.2-sol": 2,
		"gpt-6-astra": 2.5, "gpt-6.1-sol": 2.5, "gpt-6.1-terra": 2.5,
		"gpt-6-luna": 2.5, "gpt-6-sol": 2.5,
	}
	for model, multiplier := range codexFast {
		assertDefaultSpeedMultiplier(t, prices, model, "codex", "fast", at, multiplier)
	}
	for model := range fast {
		if _, found := codexFast[model]; !found {
			assertDefaultSpeedMultiplier(t, prices, model, "codex", "fast", at, 0)
		}
	}
	for _, model := range []string{
		"claude-fable-5-2", "claude-mythos-5-2", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8",
	} {
		assertDefaultSpeedMultiplier(t, prices, model, "claude", "fast", at, 2)
	}
	for model, price := range prices {
		if err := price.validate(); err != nil {
			t.Errorf("default price %q: %v", model, err)
		}
		for _, rule := range price.SpeedRates {
			if rule.Source == "" || rule.EffectiveFrom == "" || rule.Provider == "" {
				t.Errorf("default speed rule %q lacks its source, local start, or provider: %+v", model, rule)
			}
		}
	}
}

func TestDefaultUltrafastRatesUseStandardPrices(t *testing.T) {
	t.Parallel()
	prices := DefaultPrices()
	boundary := time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{
		time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		boundary.Add(-time.Nanosecond), boundary, boundary.Add(time.Hour),
	} {
		for _, provider := range []string{"openai", "codex"} {
			for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
				assertDefaultSpeedMultiplier(t, prices, model, provider, "ultrafast", at, 6)
			}
			for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol"} {
				multiplier := 6.0
				if provider == "codex" {
					multiplier = 8
				}
				assertDefaultSpeedMultiplier(t, prices, model, provider, "ultrafast", at, multiplier)
			}
		}
	}
	for model, price := range prices {
		for _, rule := range price.SpeedRates {
			if rule.Speed == "ultrafast" && (rule.Multiplier == 3 || rule.EffectiveUntil != "" || rule.EffectiveFrom != "2026-11-18T00:00:00Z") {
				t.Errorf("default ultrafast %q retains a promotional segment: %+v", model, rule)
			}
			if rule.Provider == "codex" {
				if rule.Source != "https://learn.chatgpt.com/docs/pricing" || rule.Speed == "flex" || rule.Speed == "batch" {
					t.Errorf("default Codex rule %q lacks a published Codex basis: %+v", model, rule)
				}
			}
		}
	}
	assertDefaultSpeedMultiplier(
		t, prices, "claude-opus-4-8", "claude", "fast",
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), 0,
	)
	assertDefaultSpeedMultiplier(t, prices, "gpt-6.2-astra", "vertex", "fast", boundary, 0)
	assertDefaultSpeedMultiplier(
		t, prices, "gpt-6.2-astra", "codex", "fast",
		time.Date(2026, 11, 17, 23, 59, 59, 0, time.UTC), 0,
	)
	if len(prices["kimi-k3.1-pro-highspeed"].SpeedRates) != 0 {
		t.Fatal("the separately priced Kimi Highspeed model must retain its own base price")
	}
}

func TestDefaultFastReferencesIncludeAllSupportedBasePrices(t *testing.T) {
	t.Parallel()
	prices := DefaultPrices()
	for model, rates := range map[string][3]float64{
		"gpt-5-mini": {0.25, 2, 0.025},
		"gpt-4.1":    {2, 8, 0.5}, "gpt-4.1-mini": {0.4, 1.6, 0.1},
		"gpt-4o": {2.5, 10, 1.25}, "gpt-4o-mini": {0.15, 0.6, 0.075}, "o3": {2, 8, 0.5},
	} {
		price, found := prices[model]
		if !found || price.Prompt != rates[0] || price.Completion != rates[1] || price.CacheRead == nil || *price.CacheRead != rates[2] {
			t.Errorf("default price %q = %+v, want input/output/cache-read %v", model, price, rates)
		}
		if price.EffectiveStatus() != PriceStatusPartial || price.CacheWrite != nil || price.CacheWrite5m != nil || price.CacheWrite1h != nil {
			t.Errorf("default price %q must retain unknown cache-write coverage: %+v", model, price)
		}
	}
}

func assertDefaultSpeedMultiplier(
	t *testing.T, prices map[string]Price, model, provider, speed string, at time.Time, expected float64,
) {
	t.Helper()
	price, found := prices[model]
	if !found {
		t.Fatalf("default price %q missing", model)
	}
	matches := 0
	for _, rule := range price.SpeedRates {
		if rule.Provider != provider || rule.Speed != speed {
			continue
		}
		if from, ok := parsePriceBoundary(rule.EffectiveFrom, false); ok && at.Before(from) {
			continue
		}
		if until, ok := parsePriceBoundary(rule.EffectiveUntil, true); ok && !at.Before(until) {
			continue
		}
		matches++
		if rule.Multiplier != expected {
			t.Errorf("%s %s %s at %s multiplier = %v, want %v", provider, model, speed, at, rule.Multiplier, expected)
		}
	}
	if expected == 0 && matches != 0 || expected != 0 && matches != 1 {
		t.Errorf(
			"%s %s %s at %s matched %d rules, expected multiplier %v", provider, model, speed, at, matches, expected,
		)
	}
}
