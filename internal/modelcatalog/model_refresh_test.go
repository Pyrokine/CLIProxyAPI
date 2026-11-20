package modelcatalog

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCurrentModelReferencePricesAndMediaChannels(t *testing.T) {
	catalog := DefaultFile()
	for _, test := range []struct {
		id                  string
		input, output, read float64
	}{
		{"gpt-6.2-astra", 8, 40, .4},
		{"gpt-6.2-sol", 2, 10, .1},
		{"gpt-6.1-terra", 1, 5, .05},
		{"claude-fable-5-2", 8, 40, .2},
		{"claude-haiku-5-5", .1, .5, .01},
		{"gemini-4-carbon", 2, 10, .1},
		{"gemini-4-argon", 2, 10, .1},
		{"gemini-4-hydrogen", 1.5, 7.5, .075},
		{"kimi-k3.1-pro", 2, 10, .2},
		{"kimi-k3.1-pro-highspeed", 4, 20, .2},
		{"qwen4-max", 1.5, 4.5, .075},
		{"glm-5.3-flashx", .37, 1.25, .075},
		{"glm-5.3-flashx-legal", .37, 1.25, .075},
		{"MiniMax-M3.1-Flash", .15, .6, .015},
		{"muse-wonder-1.0", 4, 20, .2},
		{"muse-ember-1.0", .1, .5, .004},
	} {
		price, ok := catalog.Price(test.id)
		if !ok || price.Prompt != test.input || price.Completion != test.output || price.Cache != test.read ||
			price.Source == "" || price.CheckedAt != "2026-11-18" || price.Currency != "USD" {
			t.Errorf("reference %s = %+v, found=%t", test.id, price, ok)
		}
		if _, ok := catalog.Find(test.id); !ok {
			t.Errorf("current model %s missing from default inventory", test.id)
		}
	}
	for _, id := range []string{
		"gpt-image-2.6-flare", "gpt-image-2.6-sunburst", "muse-image-1.0", "muse-voice-transcribe-1.0",
	} {
		model, exists := catalog.Find(id)
		channel := "meta"
		if registry.IsOpenAIImageModel(id) {
			channel = "codex"
			if registry.IsCodexImageModel(id) {
				t.Errorf("public image model %s acquired an OAuth entitlement", id)
			}
		}
		if !exists || model.Channel != channel {
			t.Errorf("media model %s missing from %s inventory: %+v", id, channel, model)
		}
		price, exists := catalog.Price(id)
		if !exists || price.Status != PriceStatusPartial || !price.RequestScoped || price.HasBaseTokenRates() || len(price.ModalityRates)+len(price.ChargeRates) == 0 {
			t.Errorf("specialized price %s = %+v", id, price)
		}
	}
	for _, id := range []string{"MiniMax-M2.7", "MiniMax-M2.7-highspeed", "qwen3-coder-next", "iflow-rome-30ba3b"} {
		if _, exists := catalog.Find(id); exists {
			t.Errorf("retired model %s remains in default inventory", id)
		}
		price, exists := catalog.Price(id)
		if !exists || price.Status != PriceStatusUnpriced || price.UnpricedReason == "" {
			t.Errorf("retired price %s = %+v", id, price)
		}
	}
	qwen, _ := catalog.Price("qwen4-max")
	if qwen.Region != "Singapore" || qwen.Status != PriceStatusPartial {
		t.Errorf("Qwen regional reference = %+v", qwen)
	}
	for _, id := range []string{"gemini-4-carbon", "gemini-4-argon"} {
		price, _ := catalog.Price(id)
		if price.EffectiveUntil != "" || price.Status != PriceStatusPartial || len(price.ConditionalRates) == 0 {
			t.Errorf("dated Gemini promotion %s = %+v", id, price)
		}
	}
}
