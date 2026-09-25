package modelcatalog

import (
	"math"
	"testing"
	"time"
)

func TestInferGroupUsesCurrentProviderGenerations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		model    string
		want     string
	}{
		{provider: "DeepSeek", model: "deepseek-flash", want: "V4.1"},
		{provider: "DeepSeek", model: "deepseek-v4-pro", want: "V4"},
		{provider: "GLM", model: "glm-4.6", want: "GLM 4"},
		{provider: "GLM", model: "GLM-5.3", want: "GLM 5"},
		{provider: "Kimi", model: "kimi-k2.7-code", want: "K2.7"},
		{provider: "Kimi", model: "kimi-k3", want: "K3"},
		{provider: "MiniMax", model: "MiniMax-M3", want: "M3"},
		{provider: "MiniMax", model: "minimax-m2.7-highspeed", want: "M2.7"},
	}

	for _, test := range tests {
		t.Run(
			test.model, func(t *testing.T) {
				if got := inferGroup(test.provider, test.model); got != test.want {
					t.Fatalf("inferGroup(%q, %q) = %q, want %q", test.provider, test.model, got, test.want)
				}
			},
		)
	}
}

func TestEnsureRuntimeModelUsesNamedFamilyBeforeProtocolChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		channel      string
		wantProvider string
		wantGroup    string
	}{
		{name: "deepseek-v4-pro", channel: "claude", wantProvider: "DeepSeek", wantGroup: "V4"},
		{name: "glm-5.3", channel: "openai-compatibility", wantProvider: "GLM", wantGroup: "GLM 5"},
		{name: "grok-4.6", channel: "openai", wantProvider: "Grok", wantGroup: "Grok 4.6"},
		{name: "unidentified-model", channel: "claude", wantProvider: "Claude", wantGroup: "Claude"},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				doc := File{}
				model := doc.EnsureRuntimeModel(test.name, test.channel, "")
				if model == nil {
					t.Fatal("EnsureRuntimeModel() returned nil")
				}
				if model.Channel != test.channel || model.Provider != test.wantProvider || model.Group != test.wantGroup {
					t.Fatalf(
						"runtime model = (channel %q, provider %q, group %q), want (%q, %q, %q)",
						model.Channel,
						model.Provider,
						model.Group,
						test.channel,
						test.wantProvider,
						test.wantGroup,
					)
				}
			},
		)
	}
}

func TestDefaultCatalogIncludesDocumentedDeepSeekAndGLMModels(t *testing.T) {
	t.Parallel()

	catalog := DefaultFile()
	deepSeekModels := []string{
		"deepseek-flash",
		"deepseek-v4-pro",
	}
	glmModels := []string{
		"glm-5.3",
		"glm-5.3-flash",
		"glm-5.2",
		"glm-5.1",
		"glm-5-turbo",
		"glm-5",
		"glm-4.7",
		"glm-4.7-flash",
		"glm-4.7-flashx",
		"glm-4.6",
		"glm-4.5-air",
		"glm-4.5-airx",
		"glm-4.5-flash",
		"glm-4-flash-250414",
		"glm-4-flashx-250414",
		"glm-5v-turbo",
		"glm-4.6v",
		"autoglm-phone",
		"glm-4.6v-flash",
		"glm-4.6v-flashx",
		"glm-4v-flash",
		"glm-4.1v-thinking-flashx",
		"glm-4.1v-thinking-flash",
		"glm-4-voice",
	}
	for _, name := range deepSeekModels {
		model, ok := catalog.Find(name)
		if !ok {
			t.Errorf("default DeepSeek inventory missing %q", name)
			continue
		}
		if model.Provider != "DeepSeek" || model.Channel != "claude" {
			t.Errorf("default DeepSeek model %q = provider %q, channel %q", name, model.Provider, model.Channel)
		}
	}
	flash, ok := catalog.Find("deepseek-flash")
	if !ok || flash.Group != "V4.1" || flash.DisplayName != "DeepSeek V4.1 Flash" {
		t.Fatalf("default DeepSeek Flash model = %+v, %v", flash, ok)
	}
	for _, name := range []string{"deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		if _, ok := catalog.Find(name); ok {
			t.Errorf("legacy DeepSeek alias %q remains in default inventory", name)
		}
		if _, ok := catalog.Price(name); !ok {
			t.Errorf("legacy DeepSeek alias %q is missing its price-only record", name)
		}
	}
	for _, name := range glmModels {
		model, ok := catalog.Find(name)
		if !ok {
			t.Errorf("default GLM inventory missing %q", name)
			continue
		}
		if model.Provider != "GLM" {
			t.Errorf("default GLM model %q provider = %q", name, model.Provider)
		}
	}
	for _, name := range []string{"glm-5.3", "glm-5.3-flash"} {
		model, _ := catalog.Find(name)
		if model == nil || model.Channel != "openai-compatibility" {
			t.Errorf("default GLM model %q channel = %v", name, model)
		}
	}
}

func TestDefaultCatalogIncludesCurrentDocumentedCompatibilityModels(t *testing.T) {
	t.Parallel()

	catalog := DefaultFile()
	for _, test := range []struct {
		name     string
		provider string
	}{
		{name: "qwen3.8-max", provider: "Qwen"},
		{name: "qwen3.8-max-0902", provider: "Qwen"},
		{name: "qwen3.8-flash", provider: "Qwen"},
		{name: "qwen3-coder-next", provider: "Qwen"},
		{name: "MiniMax-M3", provider: "MiniMax"},
		{name: "MiniMax-M2.7", provider: "MiniMax"},
		{name: "MiniMax-M2.7-highspeed", provider: "MiniMax"},
	} {
		model, ok := catalog.Find(test.name)
		if !ok {
			t.Errorf("default inventory missing %q", test.name)
			continue
		}
		if model.Provider != test.provider || model.Channel != "openai-compatibility" {
			t.Errorf("model %q = provider %q, channel %q", test.name, model.Provider, model.Channel)
		}
		if _, ok = catalog.Price(test.name); !ok {
			t.Errorf("model %q is missing a price-status record", test.name)
		}
	}

	for _, name := range []string{
		"glm-5.3-flashx",
		"kimi-k2.8",
		"kimi-k2.8-code",
		"kimi-k3-256k",
		"gpt-5.3-codex-spark",
	} {
		if model, ok := catalog.Find(name); ok {
			t.Errorf("unsupported or lookup-only model %q remains in default inventory: %+v", name, model)
		}
	}
	for _, name := range []string{
		"claude-opus-5-5",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-flare-2026-09-08",
		"gpt-image-2.5-sunburst",
		"gpt-image-2.5-sunburst-2026-09-08",
		"gemini-3.1-flash-lite-image",
		"grok-4.7",
		"grok-imagine-image-2.0",
	} {
		if _, ok := catalog.Find(name); !ok {
			t.Errorf("current native model %q is missing from default inventory", name)
		}
		if _, ok := catalog.Price(name); !ok {
			t.Errorf("current native model %q is missing a price-status record", name)
		}
	}
}

func TestDeepSeekV41DefaultMigrationAddsCanonicalAndRemovesLegacyInventory(t *testing.T) {
	t.Parallel()

	current := DefaultFile()
	current.DefaultsVersion = "2026-09-10"
	models := current.Models[:0]
	for _, model := range current.Models {
		if model.Name != "deepseek-flash" {
			models = append(models, model)
		}
	}
	current.Models = append(
		models,
		Model{
			Name: "deepseek-v4-flash", Provider: "DeepSeek", Channel: "claude", Group: "V4", Enabled: true,
		},
		Model{
			Name: "deepseek-v4-flash-vision-exp", Provider: "DeepSeek", Channel: "claude", Group: "V4 Vision",
			Enabled: true,
		},
	)
	current.DeletePrice("deepseek-flash")
	current.SetPrice(
		"deepseek-v4-flash", Price{Prompt: 0.22, Completion: 0.66, Cache: 0.007, Status: PriceStatusComplete},
	)
	current.SetPrice(
		"deepseek-v4-flash-vision-exp", Price{
			Status: PriceStatusUnpriced, UnpricedReason: "Legacy experimental model had no verified token price.",
		},
	)

	changesByName := make(map[string][]defaultUpdateChange)
	for _, change := range diffDefaults(current, DefaultFile()) {
		changesByName[change.Name] = append(changesByName[change.Name], change)
	}
	canonicalChanges := changesByName["deepseek-flash"]
	if len(canonicalChanges) != 1 || canonicalChanges[0].Type != "new_default" {
		t.Fatalf("deepseek-flash changes = %#v, want one new_default", canonicalChanges)
	}
	for _, legacyName := range []string{"deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		legacyChanges := changesByName[legacyName]
		if len(legacyChanges) != 1 || legacyChanges[0].Type != "default_removed_upstream" {
			t.Fatalf("%s changes = %#v, want one default_removed_upstream", legacyName, legacyChanges)
		}
		if _, ok := legacyChanges[0].Fields["price"]; !ok {
			t.Fatalf("%s removal does not include the replacement price", legacyName)
		}
	}

	applied, err := ApplyDefaultDecisions(
		&current, []ApplyDecision{
			{Name: "deepseek-flash", Action: "use_default"},
			{Name: "deepseek-v4-flash", Action: "remove_default"},
			{Name: "deepseek-v4-flash-vision-exp", Action: "remove_default"},
		},
	)
	if err != nil {
		t.Fatalf("ApplyDefaultDecisions() error = %v", err)
	}
	if applied != 3 {
		t.Fatalf("ApplyDefaultDecisions() applied = %d, want 3", applied)
	}
	flash, ok := current.Find("deepseek-flash")
	if !ok || flash.Group != "V4.1" || flash.DisplayName != "DeepSeek V4.1 Flash" {
		t.Fatalf("migrated deepseek-flash = %+v, %v", flash, ok)
	}
	for _, legacyName := range []string{"deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		if _, ok := current.Find(legacyName); ok {
			t.Fatalf("legacy DeepSeek model %q remains in inventory", legacyName)
		}
		price, ok := current.Price(legacyName)
		if !ok || price.Prompt != 0.15 || price.Completion != 0.6 || price.Cache != 0.003 {
			t.Fatalf("legacy DeepSeek price %q = %+v, %v", legacyName, price, ok)
		}
	}
}

func TestPriceManifestDoesNotCreateInventoryModels(t *testing.T) {
	t.Parallel()

	doc := File{Models: []Model{{Name: "runtime-model", Enabled: true}}}
	SetPrices(
		&doc, map[string]Price{
			"runtime-model":    {Prompt: 1, Completion: 2, Cache: 0.1},
			"price-only-model": {Prompt: 3, Completion: 4, Cache: 0.2},
		},
	)

	if _, ok := doc.Find("price-only-model"); ok {
		t.Fatal("price-only manifest entry created an inventory model")
	}
	if price, ok := doc.Price("price-only-model"); !ok || price.Prompt != 3 {
		t.Fatalf("price-only manifest entry = %+v, %v", price, ok)
	}
}

func TestSchemaV2ModelPriceMigratesToTopLevelManifest(t *testing.T) {
	t.Parallel()

	doc, rewrite, err := decodeFileForLoad(
		[]byte(`{
		"schema_version": 2,
		"defaults_version": "2026-08-26",
		"models": [{
			"name": "legacy-model",
			"provider": "Other",
			"channel": "other",
			"group": "Other",
			"user_created": true,
			"enabled": true,
			"price": {"prompt": 1, "completion": 2, "cache": 0.25}
		}]
	}`),
	)
	if err != nil {
		t.Fatalf("decodeFileForLoad() error = %v", err)
	}
	if !rewrite {
		t.Fatal("schema v2 catalog did not request rewrite")
	}
	model, ok := doc.Find("legacy-model")
	if !ok || model.Price != nil {
		t.Fatalf("migrated model = %#v", model)
	}
	price, ok := doc.Price("legacy-model")
	if !ok || price.Prompt != 1 || price.Completion != 2 || price.Cache != 0.25 {
		t.Fatalf("migrated price = %+v, %v", price, ok)
	}
}

func TestPriceCacheWriteTTLRatesSurviveSanitizeAndUTCBands(t *testing.T) {
	t.Parallel()

	base5m := 2.5
	base1h := 4.0
	band5m := 5.0
	band1h := 8.0
	price := Price{
		CacheWrite5m: &base5m,
		CacheWrite1h: &base1h,
		UTCRateBands: []utcPriceBand{
			{
				CacheWrite5m: &band5m,
				CacheWrite1h: &band1h,
				Weekdays:     []string{" Monday "},
				Start:        " 01:00 ",
				End:          " 02:00 ",
			},
		},
	}
	sanitizePrice(&price)
	base5m = 99
	base1h = 99
	band5m = 99
	band1h = 99
	if rate, ok := price.CacheWrite5mRate(); !ok || rate != 2.5 {
		t.Fatalf("base 5m cache-write rate = %v, %v", rate, ok)
	}
	if rate, ok := price.CacheWrite1hRate(); !ok || rate != 4 {
		t.Fatalf("base 1h cache-write rate = %v, %v", rate, ok)
	}
	at := time.Date(2026, 9, 7, 1, 30, 0, 0, time.UTC)
	bandPrice := price.RatesAt(at)
	if rate, ok := bandPrice.CacheWrite5mRate(); !ok || rate != 5 {
		t.Fatalf("band 5m cache-write rate = %v, %v", rate, ok)
	}
	if rate, ok := bandPrice.CacheWrite1hRate(); !ok || rate != 8 {
		t.Fatalf("band 1h cache-write rate = %v, %v", rate, ok)
	}
	*bandPrice.CacheWrite5m = 100
	if *price.UTCRateBands[0].CacheWrite5m != 5 {
		t.Fatal("RatesAt shared the UTC band 5m rate pointer")
	}
	if err := price.validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	onlyTTL := Price{CacheWrite5m: new(float64(1))}
	if !onlyTTL.HasReferenceTokenRates() {
		t.Fatal("5m cache-write rate did not count as a reference token rate")
	}
	invalid5m := Price{CacheWrite5m: new(float64(-1))}
	if err := invalid5m.validate(); err == nil || err.Error() != "invalid cache_write_5m price" {
		t.Fatalf("invalid 5m rate error = %v", err)
	}
	invalid1h := Price{CacheWrite1h: new(math.Inf(1))}
	if err := invalid1h.validate(); err == nil || err.Error() != "invalid cache_write_1h price" {
		t.Fatalf("invalid 1h rate error = %v", err)
	}
	unsupportedCurrency := Price{Prompt: 1, Currency: " eur "}
	if err := unsupportedCurrency.validate(); err == nil ||
		err.Error() != `unsupported price currency " eur ": usage aggregation requires USD` {
		t.Fatalf("unsupported currency error = %v", err)
	}
}

func TestPriceStatusAndEffectiveRangeRemainExplicit(t *testing.T) {
	t.Parallel()

	cacheRead := 0.075
	price := Price{
		Prompt:         0.75,
		Completion:     3.75,
		CacheRead:      &cacheRead,
		Status:         PriceStatusPartial,
		AutoApply:      true,
		EffectiveFrom:  "2026-08-13",
		EffectiveUntil: "2026-12-31",
	}
	sanitizePrice(&price)
	if price.EffectiveStatus() != PriceStatusPartial || !price.Automatic() {
		t.Fatalf("price status = %q, automatic = %v", price.EffectiveStatus(), price.Automatic())
	}
	if rate, ok := price.CacheReadRate(); !ok || rate != cacheRead || price.Cache != cacheRead {
		t.Fatalf("cache read rate = %v, %v; legacy cache = %v", rate, ok, price.Cache)
	}
	if !price.EffectiveAt(time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)) {
		t.Fatal("price was not effective on its final calendar day")
	}
	if price.EffectiveAt(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("price remained effective after its final calendar day")
	}
}
