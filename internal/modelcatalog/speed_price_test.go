package modelcatalog

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSpeedPricingRuleValidation(t *testing.T) {
	tests := []struct {
		name  string
		rules []SpeedRate
		valid bool
	}{
		{"missing multiplier", []SpeedRate{{Speed: "fast"}}, false},
		{"negative multiplier", []SpeedRate{{Speed: "fast", Multiplier: -1}}, false},
		{"nonfinite multiplier", []SpeedRate{{Speed: "fast", Multiplier: math.Inf(1)}}, false},
		{"nan multiplier", []SpeedRate{{Speed: "fast", Multiplier: math.NaN()}}, false},
		{"unknown speed", []SpeedRate{{Speed: "unknown", Multiplier: 2}}, false},
		{"invalid speed", []SpeedRate{{Speed: "priority", Multiplier: 2}}, false},
		{
			"zero override",
			[]SpeedRate{{Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{"cache_read": 0}}}, true,
		},
		{
			"negative override",
			[]SpeedRate{{Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{"prompt": -1}}}, false,
		},
		{
			"nonfinite override",
			[]SpeedRate{{Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{"completion": math.Inf(1)}}},
			false,
		},
		{
			"nan override",
			[]SpeedRate{{Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{"cache_write": math.NaN()}}},
			false,
		},
		{
			"unknown bucket",
			[]SpeedRate{{Speed: "fast", Multiplier: 2, Multipliers: map[string]float64{"reasoning": 1}}}, false,
		},
		{"invalid start", []SpeedRate{{Speed: "fast", Multiplier: 2, EffectiveFrom: "invalid"}}, false},
		{"invalid end", []SpeedRate{{Speed: "fast", Multiplier: 2, EffectiveUntil: "invalid"}}, false},
		{
			"empty interval", []SpeedRate{
				{
					Speed: "fast", Multiplier: 2, EffectiveFrom: "2026-02-17T00:00:00Z",
					EffectiveUntil: "2026-02-17T00:00:00Z",
				},
			}, false,
		},
		{
			"reversed interval",
			[]SpeedRate{{Speed: "fast", Multiplier: 2, EffectiveFrom: "2026-02-18", EffectiveUntil: "2026-02-16"}},
			false,
		},
		{"duplicate open interval", []SpeedRate{{Speed: "fast", Multiplier: 2}, {Speed: "fast", Multiplier: 3}}, false},
		{
			"normalized overlap", []SpeedRate{
				{Speed: " FAST ", Provider: " claude ", Multiplier: 2}, {Speed: "fast", Provider: "claude", Multiplier: 3},
			}, false,
		},
		{
			"inclusive date overlap", []SpeedRate{
				{Speed: "fast", Multiplier: 2, EffectiveUntil: "2026-02-16"},
				{Speed: "fast", Multiplier: 3, EffectiveFrom: "2026-02-16T23:59:59Z"},
			}, false,
		},
		{
			"adjacent intervals", []SpeedRate{
				{Speed: "fast", Multiplier: 3, EffectiveFrom: "2026-02-17T00:00:00Z"},
				{Speed: "fast", Multiplier: 2, EffectiveUntil: "2026-02-16"},
			}, true,
		},
		{
			"different providers", []SpeedRate{
				{Speed: "fast", Multiplier: 2}, {Speed: "fast", Provider: "claude", Multiplier: 3},
				{Speed: "fast", Provider: "openai", Multiplier: 4},
			}, true,
		},
		{
			"different speeds", []SpeedRate{
				{Speed: "standard", Multiplier: 1}, {Speed: "fast", Multiplier: 2}, {Speed: "ultrafast", Multiplier: 3},
				{Speed: "flex", Multiplier: 0.5}, {Speed: "batch", Multiplier: 0.5},
			}, true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				err := (Price{Prompt: 1, SpeedRates: test.rules}).validate()
				if (err == nil) != test.valid {
					t.Fatalf("validation = %v, valid = %t", err, test.valid)
				}
			},
		)
	}
}

func TestSpeedPricingJSONOverridesRejectNull(t *testing.T) {
	for _, bucket := range []string{
		"prompt", "completion", "cache_read", "cache_write", "cache_write_5m", "cache_write_1h",
	} {
		t.Run(
			bucket, func(t *testing.T) {
				for _, value := range []string{"null", "0"} {
					raw := `{"prompt":3,"completion":15,"speed_rates":[{"speed":"fast","multiplier":2,"multipliers":{"` + bucket + `":` + value + `}}]}`
					var price Price
					err := json.Unmarshal([]byte(raw), &price)
					if value == "null" {
						if err == nil || !strings.Contains(err.Error(), "must be a number") {
							t.Fatalf("null %s override decode = %v", bucket, err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if multiplier, found := price.SpeedRates[0].Multipliers[bucket]; !found || multiplier != 0 {
							t.Fatalf("explicit zero %s override = %+v", bucket, price)
						}
					}
					base := t.TempDir()
					configPath := filepath.Join(base, "config.yaml")
					catalog := `{"schema_version":3,"defaults_version":"test","prices":{"json-speed-model":` + raw + `},"models":[]}`
					if err := os.WriteFile(filepath.Join(base, FileName), []byte(catalog), 0600); err != nil {
						t.Fatal(err)
					}
					loaded, err := Load(configPath)
					if value == "null" {
						if err == nil || !strings.Contains(err.Error(), "must be a number") {
							t.Fatalf("configuration accepted null %s: %v", bucket, err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if multiplier, found := loaded.Prices["json-speed-model"].SpeedRates[0].Multipliers[bucket]; !found || multiplier != 0 {
							t.Fatalf("configuration lost explicit zero %s: %+v", bucket, loaded.Prices)
						}
					}
				}
			},
		)
	}
	for _, raw := range []string{
		`{"speed":"fast","multiplier":2,"multipliers":null}`,
		`{"speed":"fast","multiplier":2,"multipliers":{"prompt":"0"}}`,
		`{"speed":"fast","multiplier":2,"multipliers":{"prompt":false}}`,
		`{"speed":"fast","multiplier":2,"unexpected":1}`,
	} {
		var rule SpeedRate
		if err := json.Unmarshal([]byte(raw), &rule); err == nil {
			t.Fatalf("invalid speed JSON accepted: %s", raw)
		}
	}
}

func TestSpeedPricingSelectionBoundariesAndProvider(t *testing.T) {
	price := Price{
		SpeedRates: []SpeedRate{
			{Speed: "fast", Multiplier: 2, EffectiveUntil: "2026-02-17T00:00:00Z", Source: "promotion"},
			{
				Speed: "fast", Provider: "claude", Multiplier: 4, EffectiveFrom: "2026-02-16T00:00:00Z",
				EffectiveUntil: "2026-02-16T12:00:00Z", Source: "provider",
			},
			{Speed: "fast", Multiplier: 3, EffectiveFrom: "2026-02-17T00:00:00Z", Source: "regular"},
			{Speed: "ultrafast", Provider: "openai", Multiplier: 5, Source: "ultrafast"},
		},
	}
	if err := price.validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, speed, provider, at, source string
		multiplier                        float64
	}{
		{"generic", "fast", "other", "2026-02-16T01:00:00Z", "promotion", 2},
		{"provider wins", "fast", "claude", "2026-02-16T01:00:00Z", "provider", 4},
		{"exact provider", "fast", "Claude", "2026-02-16T01:00:00Z", "promotion", 2},
		{"provider expires", "fast", "claude", "2026-02-16T12:00:00Z", "promotion", 2},
		{"before UTC cutoff", "fast", "other", "2026-02-16T23:59:59.999999999Z", "promotion", 2},
		{"at UTC cutoff", "fast", "other", "2026-02-17T00:00:00Z", "regular", 3},
		{"equivalent offset", "fast", "other", "2026-02-17T08:00:00+08:00", "regular", 3},
		{"ultrafast", "ultrafast", "openai", "2026-02-16T01:00:00Z", "ultrafast", 5},
		{"provider mismatch", "ultrafast", "claude", "2026-02-16T01:00:00Z", "", 0},
		{"speed mismatch", "standard", "openai", "2026-02-16T01:00:00Z", "", 0},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				at, err := time.Parse(time.RFC3339Nano, test.at)
				if err != nil {
					t.Fatal(err)
				}
				rule, found := price.SpeedRateAt(test.speed, test.provider, at)
				if found != (test.multiplier > 0) || rule.Multiplier != test.multiplier || rule.Source != test.source {
					t.Fatalf("selected = %+v/%t", rule, found)
				}
			},
		)
	}
	dateOnly := Price{SpeedRates: []SpeedRate{{Speed: "fast", Multiplier: 2, EffectiveUntil: "2026-02-16"}}}
	for _, test := range []struct {
		at    time.Time
		found bool
	}{
		{time.Date(2026, 2, 16, 23, 59, 59, 999999999, time.UTC), true},
		{time.Date(2026, 2, 17, 0, 0, 0, 0, time.UTC), false},
	} {
		if _, found := dateOnly.SpeedRateAt("fast", "claude", test.at); found != test.found {
			t.Fatalf("date boundary %s = %t", test.at, found)
		}
	}
}

func TestSpeedPricingSaveAndClone(t *testing.T) {
	price := Price{
		Prompt: 1, CacheRead: new(float64(0)), SpeedRates: []SpeedRate{
			{
				Speed: " FAST ", Provider: " claude ", Multiplier: 2,
				Multipliers: map[string]float64{"cache_read": 0}, Source: " source ",
				EffectiveFrom: " 2026-02-01 ", EffectiveUntil: " 2026-02-16 ",
			},
		},
	}
	doc := File{SchemaVersion: SchemaVersion, DefaultsVersion: "test"}
	doc.SetPrice("speed-model", price)
	price.SpeedRates[0].Multipliers["cache_read"] = 9
	price.SpeedRates[0].Source = "changed"
	saved := doc.Prices["speed-model"]
	if saved.SpeedRates[0].Source != "source" || saved.SpeedRates[0].Speed != "fast" || saved.SpeedRates[0].Provider != "claude" || saved.SpeedRates[0].Multipliers["cache_read"] != 0 {
		t.Fatalf("saved rule = %+v", saved)
	}
	cloned := saved.Clone()
	cloned.SpeedRates[0].Multipliers["cache_read"] = 8
	*cloned.CacheRead = 7
	if saved.SpeedRates[0].Multipliers["cache_read"] != 0 || *saved.CacheRead != 0 {
		t.Fatal("price clone shares mutable data")
	}
	selected, found := saved.SpeedRateAt("fast", "claude", time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
	if !found {
		t.Fatal("rule missing")
	}
	selected.Multipliers["cache_read"] = 7
	if saved.SpeedRates[0].Multipliers["cache_read"] != 0 {
		t.Fatal("selection shares mutable multipliers")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, doc); err != nil {
		t.Fatal(err)
	}
	restored, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Prices["speed-model"], saved) {
		t.Fatalf("restored price = %+v", restored.Prices["speed-model"])
	}
}
