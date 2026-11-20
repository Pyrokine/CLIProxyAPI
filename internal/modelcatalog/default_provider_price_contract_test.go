package modelcatalog_test

import (
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

func TestZAIReferenceRatesReachUsageLookup(t *testing.T) {
	rates := map[string][3]float64{
		"glm-5.3": {1.4, .26, 4.4}, "glm-5.3-flashx": {.37, .075, 1.25}, "glm-5.3-flashx-legal": {.37, .075, 1.25},
		"glm-5.3-flash": {.15, .03, .5},
		"glm-5.2":       {1.4, .26, 4.4}, "glm-5.1": {1.4, .26, 4.4},
		"glm-5": {1, .2, 3.2}, "glm-4.7": {.6, .11, 2.2},
		"glm-4.7-flash": {0, 0, 0}, "glm-4.7-flashx": {.07, .01, .4},
		"glm-4.6": {.6, .11, 2.2}, "glm-4.5-air": {.2, .03, 1.1},
		"glm-4.5-airx": {1.1, .22, 4.5}, "glm-4.5-flash": {0, 0, 0},
		"glm-4.6v": {.3, .05, .9}, "glm-4.6v-flashx": {.04, .004, .4},
		"glm-4.6v-flash": {0, 0, 0},
	}
	catalog := modelcatalog.DefaultFile()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := modelcatalog.Save(configPath, catalog); err != nil {
		t.Fatal(err)
	}
	provider := usage.BuildModelPriceProvider(configPath)
	if provider == nil {
		t.Fatal("price provider missing")
	}
	snapshot := provider.Snapshot()
	if snapshot == nil {
		t.Fatal("usage reference snapshot missing")
	}
	for name, want := range rates {
		t.Run(
			name, func(t *testing.T) {
				price, ok := catalog.Price(name)
				if !ok || price.Prompt != want[0] || price.CacheRead == nil || *price.CacheRead != want[1] ||
					price.Completion != want[2] {
					t.Fatalf("reference rates = %+v, found=%t, want %v", price, ok, want)
				}
				if price.Status != modelcatalog.PriceStatusPartial || !price.HasReferenceTokenRates() ||
					price.Currency != "USD" ||
					price.Source != "https://docs.z.ai/guides/overview/pricing.md" ||
					price.CheckedAt != "2026-11-18" ||
					price.Region != "unspecified" ||
					price.ServiceTier != "unspecified" ||
					len(price.Limitations) == 0 {
					t.Errorf("reference scope = %+v", price)
				}
				if price.CacheWrite != nil || price.CacheWrite5m != nil || price.CacheWrite1h != nil {
					t.Errorf("storage offer became a cache-write rate: %+v", price)
				}
				input, output, cache, found := snapshot(name)
				if !found || input != want[0] || cache != want[1] || output != want[2] {
					t.Errorf("usage rates = %v/%v/%v found=%t, want %v", input, cache, output, found, want)
				}
			},
		)
	}
}

func TestBigModelCNYAndUndatedPromotionStayUnpriced(t *testing.T) {
	catalog := modelcatalog.DefaultFile()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := modelcatalog.Save(configPath, catalog); err != nil {
		t.Fatal(err)
	}
	provider := usage.BuildModelPriceProvider(configPath)
	if provider == nil || provider.Snapshot() == nil {
		t.Fatal("price provider missing")
	}
	for _, name := range []string{
		"glm-5-turbo", "glm-4-long", "glm-4-flashx-250414", "glm-4-flash-250414", "glm-4-voice",
		"glm-4.1v-thinking-flash", "glm-4.1v-thinking-flashx", "glm-4v-flash", "glm-5v-turbo", "autoglm-phone",
	} {
		price, ok := catalog.Price(name)
		if !ok || price.Status != modelcatalog.PriceStatusUnpriced || price.HasReferenceTokenRates() ||
			price.UnpricedReason == "" ||
			price.CheckedAt != "2026-11-18" {
			t.Errorf("unpriced scope for %s = %+v", name, price)
		}
		if _, _, _, found := provider.Snapshot()(name); found {
			t.Errorf("%s acquired a fabricated USD reference rate", name)
		}
	}
}
