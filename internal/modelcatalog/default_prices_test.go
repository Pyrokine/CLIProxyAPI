package modelcatalog

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultPricesMatchCurrentPublishedRates(t *testing.T) {
	t.Parallel()

	cacheWriteSol := 5.0
	cacheWriteTerra := 2.5
	cacheWriteLuna := 0.25
	cacheWriteAstra := 12.5
	cacheWriteCyber := 15.625
	want := map[string]Price{
		"gpt-5.6-sol": {
			Prompt: 4, Completion: 20, Cache: 0.4, CacheWrite: &cacheWriteSol,
			Status: PriceStatusPartial,
		},
		"gpt-5.6-terra": {
			Prompt: 2, Completion: 12, Cache: 0.2, CacheWrite: &cacheWriteTerra,
			Status: PriceStatusPartial,
		},
		"gpt-5.6-luna": {
			Prompt: 0.2, Completion: 1.2, Cache: 0.02, CacheWrite: &cacheWriteLuna,
			Status: PriceStatusPartial,
		},
		"gpt-6-astra": {
			Prompt: 10, Completion: 50, Cache: 1, CacheWrite: &cacheWriteAstra,
			Status: PriceStatusPartial,
		},
		"gpt-5.6-cyber": {
			Prompt: 12.5, Completion: 75, Cache: 1.25, CacheWrite: &cacheWriteCyber,
			Status: PriceStatusPartial,
		},
		"deepseek-flash": {
			Prompt: 0.15, Completion: 0.6, Cache: 0.003,
			Status: PriceStatusComplete,
		},
		"deepseek-v4-pro": {
			Prompt: 0.66, Completion: 1.98, Cache: 0.022,
			Status: PriceStatusComplete,
		},
		"deepseek-v4-flash": {
			Prompt: 0.15, Completion: 0.6, Cache: 0.003,
			Status: PriceStatusComplete,
		},
		"deepseek-v4-flash-vision-exp": {
			Prompt: 0.15, Completion: 0.6, Cache: 0.003,
			Status: PriceStatusComplete,
		},
		"gemini-3.7-flash": {
			Prompt: 0.75, Completion: 3.75, Cache: 0.075,
			Status: PriceStatusPartial, AutoApply: true,
		},
		"kimi-k3": {
			Prompt: 3, Completion: 15, Cache: 0.3,
			Status: PriceStatusPartial, AutoApply: true,
		},
	}
	prices := DefaultPrices()
	for name, expected := range want {
		got, ok := prices[name]
		if !ok {
			t.Errorf("DefaultPrices()[%q] missing", name)
			continue
		}
		if got.Prompt != expected.Prompt || got.Completion != expected.Completion || got.Cache != expected.Cache ||
			got.EffectiveStatus() != expected.EffectiveStatus() || got.AutoApply != expected.AutoApply ||
			!reflect.DeepEqual(got.CacheWrite, expected.CacheWrite) {
			t.Errorf("DefaultPrices()[%q] = %+v, want core fields %+v", name, got, expected)
		}
		if got.CacheRead == nil || *got.CacheRead != expected.Cache {
			t.Errorf("DefaultPrices()[%q].CacheRead = %v, want %v", name, got.CacheRead, expected.Cache)
		}
		if got.Currency != "USD" || got.Source == "" || got.CheckedAt == "" {
			t.Errorf("DefaultPrices()[%q] source metadata = %+v", name, got)
		}
	}

	metadata := map[string]struct {
		source      string
		limitations []string
	}{
		"gpt-6-astra": {
			source: "https://developers.openai.com/api/docs/models/gpt-6-astra",
			limitations: []string{
				"Requests with more than 272K input tokens use 2x input and cache pricing and 1.5x output pricing; the current manifest does not select that tier automatically.",
				"Access is rolling out through the Trusted Access Program; this reference price does not establish entitlement.",
			},
		},
		"gpt-5.6-cyber": {
			source: "https://developers.openai.com/api/docs/models/gpt-5.6-cyber",
			limitations: []string{
				"Requests with more than 272K input tokens use 2x input and 1.5x output pricing; the current manifest does not select that tier automatically. The official source does not publish long-context cache rates.",
				"Access requires separate Daybreak approval and provisioning.",
			},
		},
	}
	for name, expected := range metadata {
		got := prices[name]
		if got.Source != expected.source || got.CheckedAt != "2026-09-04" ||
			!reflect.DeepEqual(got.Limitations, expected.limitations) {
			t.Errorf(
				"DefaultPrices()[%q] metadata = %+v, want source %q and limitations %v", name, got, expected.source,
				expected.limitations,
			)
		}
	}

	// This contract test intentionally compares a compile-time manifest version.
	// noinspection GoBoolExpressions
	if DefaultsVersion != "2026-09-11" {
		t.Fatalf("DefaultsVersion = %q, want 2026-09-11", DefaultsVersion)
	}
}

func TestDeepSeekDefaultPricesSelectRecurringUTCBands(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("test fixture weekday = %s", monday.Weekday())
	}
	models := []struct {
		name    string
		offPeak tokenRates
		peak    tokenRates
	}{
		{
			name:    "deepseek-flash",
			offPeak: tokenRates{Prompt: 0.15, Completion: 0.6, Cache: 0.003},
			peak:    tokenRates{Prompt: 0.3, Completion: 1.2, Cache: 0.006},
		},
		{
			name:    "deepseek-v4-pro",
			offPeak: tokenRates{Prompt: 0.66, Completion: 1.98, Cache: 0.022},
			peak:    tokenRates{Prompt: 1.32, Completion: 3.96, Cache: 0.044},
		},
		{
			name:    "deepseek-v4-flash",
			offPeak: tokenRates{Prompt: 0.15, Completion: 0.6, Cache: 0.003},
			peak:    tokenRates{Prompt: 0.3, Completion: 1.2, Cache: 0.006},
		},
		{
			name:    "deepseek-v4-flash-vision-exp",
			offPeak: tokenRates{Prompt: 0.15, Completion: 0.6, Cache: 0.003},
			peak:    tokenRates{Prompt: 0.3, Completion: 1.2, Cache: 0.006},
		},
	}
	times := []struct {
		name string
		at   time.Time
		peak bool
	}{
		{name: "weekday off-peak before first band", at: monday.Add(59 * time.Minute)},
		{name: "weekday first peak band", at: monday.Add(time.Hour), peak: true},
		{name: "weekday gap starts at 04:00", at: monday.Add(4 * time.Hour)},
		{name: "weekday second peak band", at: monday.Add(6 * time.Hour), peak: true},
		{name: "weekday peak end is exclusive at 10:00", at: monday.Add(10 * time.Hour)},
		{name: "weekend is off-peak", at: monday.AddDate(0, 0, 5).Add(2 * time.Hour)},
	}

	prices := DefaultPrices()
	for _, model := range models {
		t.Run(
			model.name, func(t *testing.T) {
				price, ok := prices[model.name]
				if !ok {
					t.Fatalf("missing price for %s", model.name)
				}
				for _, test := range times {
					t.Run(
						test.name, func(t *testing.T) {
							want := model.offPeak
							if test.peak {
								want = model.peak
							}
							selected := price.RatesAt(test.at)
							cacheRead, hasCacheRead := selected.CacheReadRate()
							if !hasCacheRead || selected.Prompt != want.Prompt ||
								selected.Completion != want.Completion || cacheRead != want.Cache {
								t.Fatalf(
									"RatesAt(%s) = %+v, cache_read=%v/%t; want %+v",
									test.at.Format(time.RFC3339),
									selected,
									cacheRead,
									hasCacheRead,
									want,
								)
							}
						},
					)
				}
			},
		)
	}
}

func TestDefaultCatalogKeepsPriceManifestSeparateFromInventory(t *testing.T) {
	t.Parallel()

	catalog := DefaultFile()
	aliases := catalog.ToOAuthModelAlias()
	for _, name := range []string{"gpt-5", "MiniMax-M3", "gpt-5.6-cyber"} {
		if _, ok := catalog.Price(name); !ok {
			t.Errorf("default price manifest missing %q", name)
		}
		if _, ok := catalog.Find(name); ok {
			t.Errorf("price-only record %q created an inventory model", name)
		}
		for channel, entries := range aliases {
			for _, entry := range entries {
				if modelKey(entry.Name) == modelKey(name) || modelKey(entry.Alias) == modelKey(name) {
					t.Errorf("price-only record %q created OAuth alias in channel %q: %+v", name, channel, entry)
				}
			}
		}
	}
	astra, ok := catalog.Find("gpt-6-astra")
	if !ok || astra.Provider != "GPT" || astra.Channel != "codex" || astra.Group != "GPT 6" || !astra.Enabled {
		t.Fatalf("default Astra inventory model = %+v, %v", astra, ok)
	}
	if _, ok := catalog.Price("gpt-6-astra"); !ok {
		t.Fatal("default Astra price is missing")
	}
	for _, name := range []string{"glm-4.6", "qwen3-max-preview", "imagen-4.0-generate-001"} {
		price, ok := catalog.Price(name)
		if !ok || price.EffectiveStatus() != PriceStatusUnpriced || price.UnpricedReason == "" {
			t.Errorf("unpriced record %q = %+v, %v", name, price, ok)
		}
	}
}

func TestDefaultUpdateAppliesInventoryAndPriceManifestIndependently(t *testing.T) {
	t.Parallel()

	const previousDefaultsVersion = "2026-08-26"
	current := DefaultFile()
	current.DefaultsVersion = previousDefaultsVersion
	current.DeletePrice("MiniMax-M3")
	current.SetPrice("gpt-5.6-sol", Price{Prompt: 5, Completion: 30, Cache: 0.5})
	current.DeletePrice("gpt-6-astra")
	current.SetPrice("gpt-5.6-cyber", Price{Prompt: 1, Completion: 2, Cache: 0.1})
	models := current.Models[:0]
	for _, model := range current.Models {
		if model.Name != "gemini-3.7-flash" {
			models = append(models, model)
		}
	}
	current.Models = models

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, current); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate() error = %v", err)
	}
	changes := make(map[string]defaultUpdateChange, len(preview.Changes))
	for _, change := range preview.Changes {
		changes[change.Name] = change
	}
	for _, name := range []string{"MiniMax-M3", "gpt-5.6-sol", "gpt-6-astra", "gpt-5.6-cyber", "gemini-3.7-flash"} {
		if _, ok := changes[name]; !ok {
			t.Errorf("default update missing %q: %#v", name, preview.Changes)
		}
	}

	decisions := make([]ApplyDecision, 0, len(changes))
	for name := range changes {
		decisions = append(decisions, ApplyDecision{Name: name, Action: "use_default"})
	}
	result, err := applyDefaultUpdate(configPath, decisions)
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate() error = %v", err)
	}
	if result.Applied != len(changes) || result.BackupPath == "" {
		t.Fatalf("ApplyDefaultUpdate() = %#v, changes=%d", result, len(changes))
	}
	updated, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() updated catalog error = %v", err)
	}
	if updated.DefaultsVersion != DefaultsVersion || updated.SchemaVersion != SchemaVersion {
		t.Fatalf("updated versions = (%d, %q)", updated.SchemaVersion, updated.DefaultsVersion)
	}
	defaults := DefaultFile()
	for _, name := range []string{"MiniMax-M3", "gpt-5.6-cyber"} {
		if _, ok := updated.Find(name); ok {
			t.Fatalf("price-only default %q became inventory during update", name)
		}
		got, gotOK := updated.Price(name)
		want, wantOK := defaults.Price(name)
		if !gotOK || !wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("updated price-only default %q = %+v, %v; want %+v, %v", name, got, gotOK, want, wantOK)
		}
	}
	astra, ok := updated.Find("gpt-6-astra")
	if !ok || astra.Provider != "GPT" || astra.Channel != "codex" || astra.Group != "GPT 6" || !astra.Enabled {
		t.Fatalf("updated Astra inventory model = %+v, %v", astra, ok)
	}
	gotAstraPrice, gotAstraPriceOK := updated.Price("gpt-6-astra")
	wantAstraPrice, wantAstraPriceOK := defaults.Price("gpt-6-astra")
	if !gotAstraPriceOK || !wantAstraPriceOK || !reflect.DeepEqual(gotAstraPrice, wantAstraPrice) {
		t.Fatalf(
			"updated Astra price = %+v, %v; want %+v, %v",
			gotAstraPrice,
			gotAstraPriceOK,
			wantAstraPrice,
			wantAstraPriceOK,
		)
	}
	if _, ok := updated.Find("gemini-3.7-flash"); !ok {
		t.Fatal("new inventory model was not applied")
	}
	if price, ok := updated.Price("gpt-5.6-sol"); !ok || price.Prompt != 4 {
		t.Fatalf("updated gpt-5.6-sol price = %+v, %v", price, ok)
	}
	backupData, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("read backup error = %v", err)
	}
	backup, err := decodeFile(backupData)
	if err != nil {
		t.Fatalf("decode backup error = %v", err)
	}
	if backup.DefaultsVersion != previousDefaultsVersion {
		t.Fatalf("backup defaults version = %q", backup.DefaultsVersion)
	}
	if price, ok := backup.Price("gpt-5.6-sol"); !ok || price.Prompt != 5 {
		t.Fatalf("backup gpt-5.6-sol price = %+v, %v", price, ok)
	}
}

func TestDefaultUpdateRemovesStaleInventoryWithoutDeletingPriceOnlyDefault(t *testing.T) {
	t.Parallel()

	const name = "gpt-5"
	current := DefaultFile()
	current.DefaultsVersion = "2026-08-26"
	if _, ok := current.Find(name); ok {
		t.Fatalf("%s unexpectedly exists in default inventory", name)
	}
	defaultPrice, ok := current.Price(name)
	if !ok {
		t.Fatalf("default price missing for %s", name)
	}
	current.Models = append(
		current.Models, Model{
			Name:        name,
			Provider:    "OpenAI",
			Channel:     "codex",
			Group:       "GPT 5",
			UserCreated: false,
			Enabled:     true,
		},
	)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, current); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	result, err := applyDefaultUpdate(configPath, []ApplyDecision{{Name: name, Action: "remove_default"}})
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate() error = %v", err)
	}
	if result.Applied != 1 {
		t.Fatalf("ApplyDefaultUpdate() applied = %d, want 1", result.Applied)
	}
	updated, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() updated catalog error = %v", err)
	}
	if _, ok := updated.Find(name); ok {
		t.Fatalf("stale inventory model %s was not removed", name)
	}
	price, ok := updated.Price(name)
	if !ok || !reflect.DeepEqual(price, defaultPrice) {
		t.Fatalf("price-only default %s = %+v, %v; want %+v", name, price, ok, defaultPrice)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate() error = %v", err)
	}
	if len(preview.Changes) != 0 {
		t.Fatalf("default update left changes: %#v", preview.Changes)
	}
}
