package modelcatalog_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestImagenPerImageReferencesStayOutsideTokenRates(t *testing.T) {
	t.Parallel()

	prices := modelcatalog.DefaultPrices()
	for id, amount := range map[string]string{
		"imagen-4.0-generate-001":       "0.04",
		"imagen-4.0-ultra-generate-001": "0.06",
		"imagen-4.0-fast-generate-001":  "0.02",
		"imagen-3.0-generate-002":       "0.04",
		"imagen-3.0-fast-generate-001":  "0.02",
	} {
		t.Run(
			id, func(t *testing.T) {
				price, found := prices[id]
				if !found || price.EffectiveStatus() != modelcatalog.PriceStatusPartial ||
					price.HasBaseTokenRates() || len(price.ChargeRates) != 1 {
					t.Fatalf("%s token price = %+v, found=%t", id, price, found)
				}
				if price.Currency != "USD" || price.Source != "https://cloud.google.com/model-garden/pricing" ||
					price.CheckedAt != "2026-11-18" {
					t.Errorf("%s source metadata = %+v", id, price)
				}
				limitations := strings.Join(price.Limitations, "\n")
				for _, required := range []string{
					"USD " + amount + " per generated image",
					"https://docs.cloud.google.com/vertex-ai/generative-ai/docs/models/imagen/", "2026-06-30",
				} {
					if !strings.Contains(limitations, required) {
						t.Errorf("%s limitations omit %q: %s", id, required, limitations)
					}
				}
				if price.Region != "unspecified" || price.ServiceTier != "unspecified" || price.EffectiveFrom != "" ||
					price.EffectiveUntil != "" {
					t.Errorf("%s has unsupported rate conditions: %+v", id, price)
				}
			},
		)
	}
}

func TestDefaultImagePricesStayUnpricedInUsageReceipts(t *testing.T) {
	ids := []string{
		"gpt-image-1.5", "gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-flare-2026-09-08",
		"gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst-2026-09-08",
		"gemini-3-pro-image", "gemini-2.5-flash-image", "gemini-3.1-flash-image", "gemini-3.1-flash-lite-image",
		"imagen-4.0-generate-001", "imagen-4.0-ultra-generate-001", "imagen-4.0-fast-generate-001",
		"imagen-3.0-generate-002", "imagen-3.0-fast-generate-001",
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := modelcatalog.Save(configPath, modelcatalog.DefaultFile()); err != nil {
		t.Fatal(err)
	}
	provider := usage.BuildModelPriceProvider(configPath)
	if provider == nil || provider.Snapshot() == nil {
		t.Fatal("default prices did not create a usage provider")
	}
	for _, id := range ids {
		if _, _, _, found := provider.Snapshot()(id); found {
			t.Errorf("legacy usage lookup exposes numeric token prices for %s", id)
		}
	}
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{})
	persister.SetPriceProvider(provider)
	persister.Start(t.Context())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage test store did not initialize")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	plugin := usage.NewLoggerPlugin(persister, nil, func() bool { return true })
	now := time.Now().UTC()
	for _, id := range ids {
		providerName := "codex"
		if strings.HasPrefix(id, "imagen-") {
			providerName = "vertex"
		}
		plugin.HandleUsage(
			ctx, coreusage.Record{
				Model: id, Provider: providerName, RequestedAt: now, RequestID: "image-price-" + id,
				Detail: coreusage.Detail{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100},
			},
		)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, total, err := persister.QueryEventsContext(
			ctx, now.Add(-time.Minute), now.Add(time.Minute), usage.EventFilters{}, 1, 100, "timestamp", false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if total == len(ids) {
			seen := make(map[string]bool, len(ids))
			for _, event := range events {
				seen[event.Model] = true
				receipt := event.PricingReceipt
				if event.CostStatus != usage.CostStatusUnpriced || event.Cost != 0 || receipt == nil {
					t.Fatalf(
						"%s usage cost/status/receipt = %v/%s/%+v", event.Model, event.Cost, event.CostStatus, receipt,
					)
				}
				if receipt.CostStatus != usage.CostStatusUnpriced || receipt.CostMicro != 0 ||
					receipt.CostReason == "" ||
					receipt.Coverage.PricedTokens != 0 ||
					receipt.Coverage.UnpricedTokens != 1100 {
					t.Errorf("%s receipt = %+v", event.Model, receipt)
				}
				for _, item := range receipt.LineItems {
					if item.Priced || item.RatePerMillion != nil {
						t.Errorf("%s exposes a priced image-token item: %+v", event.Model, item)
					}
				}
			}
			for _, id := range ids {
				if !seen[id] {
					t.Errorf("usage receipt missing for %s", id)
				}
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("image usage receipts = %d, want %d: %v", total, len(ids), ctx.Err())
		case <-ticker.C:
		}
	}
}
