package modelcatalog

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPriceJSONPreservesUnknownAndPublishedZeroRates(t *testing.T) {
	for _, raw := range []string{
		`{"prompt":1,"request_scoped":true}`, `{"prompt":0,"completion":0,"cache_read":0,"request_scoped":true}`,
	} {
		var price Price
		if err := json.Unmarshal([]byte(raw), &price); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		doc := File{SchemaVersion: SchemaVersion, DefaultsVersion: DefaultsVersion}
		doc.SetPrice("price-only", price)
		if err := Save(path, doc); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(loaded.Prices["price-only"])
		if err != nil {
			t.Fatal(err)
		}
		var actual, expected map[string]any
		if err := json.Unmarshal(encoded, &actual); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("rates changed: %s -> %s", raw, encoded)
		}
	}
}

func TestConditionalPriceValidationAndDeepCopy(t *testing.T) {
	minimum := int64(272001)
	price := Price{
		Prompt: 2, Completion: 10, RequestScoped: true, ConditionalRates: []ConditionalRate{
			{
				ID: "long", MinInputTokens: &minimum, RateOverrides: RateOverrides{
					Prompt: new(4.0), Completion: new(15.0), CacheWrite30m: new(5.0), CacheWrite12h: new(10.0),
				},
			},
		}, ModalityRates: []ModalityRate{{Modality: "text", RateOverrides: RateOverrides{Prompt: new(0.0)}}},
		ChargeRates: []ChargeRate{{Kind: "web_fetch", Unit: "request", Price: 0, PerUnits: 1}},
	}
	if err := price.validate(); err != nil {
		t.Fatal(err)
	}
	cloned := price.Clone()
	*cloned.ConditionalRates[0].Prompt = 99
	*cloned.ConditionalRates[0].MinInputTokens = 1
	*cloned.ModalityRates[0].Prompt = 99
	if *price.ConditionalRates[0].Prompt != 4 || *price.ConditionalRates[0].MinInputTokens != 272001 || *price.ModalityRates[0].Prompt != 0 {
		t.Fatal("conditional clone shares pointers")
	}
	for name, mutate := range map[string]func(*Price){
		"negative bound":  func(p *Price) { p.ConditionalRates[0].MinInputTokens = new(int64(-1)) },
		"reversed bounds": func(p *Price) { p.ConditionalRates[0].MaxInputTokens = new(int64(2)) },
		"nonfinite rate":  func(p *Price) { p.ConditionalRates[0].Completion = new(math.Inf(1)) },
		"zero multiplier": func(p *Price) {
			p.ConditionalRates = []ConditionalRate{
				{
					Region: "us", Multiplier: new(0.0),
				},
			}
		},
		"unscoped multiplier": func(p *Price) { p.ConditionalRates = []ConditionalRate{{Multiplier: new(1.1)}} },
		"mixed multiplier":    func(p *Price) { p.ConditionalRates[0].Region = "us"; p.ConditionalRates[0].Multiplier = new(1.1) },
		"conflicting unknown": func(p *Price) { p.ConditionalRates[0].UnpricedBuckets = []string{"prompt"} },
		"overlapping rates": func(p *Price) {
			p.ConditionalRates = append(
				p.ConditionalRates, ConditionalRate{
					ID: "duplicate-rate", MinInputTokens: new(int64(272001)),
					RateOverrides: RateOverrides{Prompt: new(5.0)},
				},
			)
		},
		"invalid charge divisor": func(p *Price) { p.ChargeRates[0].PerUnits = 0 },
		"duplicate charge":       func(p *Price) { p.ChargeRates = append(p.ChargeRates, p.ChargeRates[0]) },
	} {
		t.Run(
			name, func(t *testing.T) {
				candidate := price.Clone()
				mutate(&candidate)
				if candidate.validate() == nil {
					t.Fatal("invalid conditional price accepted")
				}
			},
		)
	}
}

func TestChargeSpeedNormalizationAndValidation(t *testing.T) {
	var price Price
	if err := json.Unmarshal(
		[]byte(`{"charge_rates":[{"kind":"image","unit":"image","price":0,"per_units":1},{"kind":"image","unit":"image","price":0.5,"per_units":1,"speed":" BATCH "}]}`),
		&price,
	); err != nil {
		t.Fatal(err)
	}
	sanitizePrice(&price)
	if err := price.validate(); err != nil || price.ChargeRates[1].Speed != "batch" {
		t.Fatalf("valid speed rates=%+v/%v", price.ChargeRates, err)
	}
	encoded, err := json.Marshal(price)
	if err != nil {
		t.Fatal(err)
	}
	var restored Price
	if err = json.Unmarshal(encoded, &restored); err != nil || !reflect.DeepEqual(restored, price) {
		t.Fatalf("charge speed roundtrip=%+v/%v", restored, err)
	}
	invalid := price.Clone()
	invalid.ChargeRates[1].Speed = "unsupported"
	if invalid.validate() == nil {
		t.Fatal("unsupported charge speed accepted")
	}
	invalid = price.Clone()
	invalid.ChargeRates = append(invalid.ChargeRates, invalid.ChargeRates[1])
	if invalid.validate() == nil {
		t.Fatal("same-speed duplicate charge accepted")
	}
}

func TestChineseHolidayCalendarUsesPublishedYears(t *testing.T) {
	price := DefaultPrices()["deepseek-flash"]
	for _, test := range []struct {
		at     string
		prompt float64
		known  bool
	}{
		{"2026-01-01T02:00:00Z", 0.15, true},
		{"2026-01-05T02:00:00Z", 0.30, true},
		{"2026-02-16T07:00:00Z", 0.15, true},
		{"2026-10-01T02:00:00Z", 0.15, true},
		{"2027-02-08T02:00:00Z", 0.15, true},
		{"2027-02-15T02:00:00Z", 0.30, true},
		{"2028-01-03T02:00:00Z", 0.30, false},
	} {
		at, err := time.Parse(time.RFC3339, test.at)
		if err != nil {
			t.Fatal(err)
		}
		if got := price.RatesAt(at).Prompt; got != test.prompt || price.CalendarKnownAt(at) != test.known {
			t.Errorf("%s rate=%v known=%t", test.at, got, price.CalendarKnownAt(at))
		}
	}
}
