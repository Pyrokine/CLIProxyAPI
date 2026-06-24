package modelcatalog

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

//go:embed default_prices.json
var defaultPricesJSON []byte

type defaultChannel struct {
	Channel  string
	Provider string
}

var defaultChannels = []defaultChannel{
	{Channel: "codex", Provider: "GPT"},
	{Channel: "claude", Provider: "Claude"},
	{Channel: "gemini", Provider: "Gemini"},
	{Channel: "vertex", Provider: "Gemini"},
	{Channel: "gemini-cli", Provider: "Gemini"},
	{Channel: "aistudio", Provider: "Gemini"},
	{Channel: "antigravity", Provider: "Gemini"},
	{Channel: "qwen", Provider: "Qwen"},
	{Channel: "iflow", Provider: ""},
	{Channel: "kimi", Provider: "Kimi"},
	{Channel: "xai", Provider: "Grok"},
}

func DefaultFile() File {
	prices := DefaultPrices()
	byName := make(map[string]int)
	out := File{SchemaVersion: SchemaVersion, DefaultsVersion: DefaultsVersion}
	ensure := func(name, provider, channel, displayName string, userCreated bool) *Model {
		name = normalizeModelName(name)
		if name == "" {
			return nil
		}
		key := modelKey(name)
		if index, exists := byName[key]; exists {
			existing := &out.Models[index]
			if existing.DisplayName == "" && strings.TrimSpace(displayName) != "" {
				existing.DisplayName = strings.TrimSpace(displayName)
			}
			return existing
		}
		provider = strings.TrimSpace(provider)
		if provider == "" {
			provider = inferProvider(channel, name)
		}
		channel = inferChannel(channel, name)
		model := Model{
			Name:        name,
			Provider:    provider,
			Channel:     channel,
			Group:       inferGroup(provider, name),
			DisplayName: strings.TrimSpace(displayName),
			UserCreated: userCreated,
			Enabled:     true,
		}
		if model.DisplayName == "" {
			model.DisplayName = displayNameFromName(name)
		}
		out.Models = append(out.Models, model)
		byName[key] = len(out.Models) - 1
		return &out.Models[len(out.Models)-1]
	}
	for _, item := range defaultChannels {
		models := defaultModelsByChannel(item.Channel)
		for _, info := range models {
			if info == nil {
				continue
			}
			ensure(info.ID, item.Provider, item.Channel, info.DisplayName, false)
		}
	}
	priceNames := make([]string, 0, len(prices))
	for name := range prices {
		priceNames = append(priceNames, name)
	}
	sort.Strings(priceNames)
	for _, name := range priceNames {
		model := ensure(name, "", "", "", false)
		price := prices[name]
		model.Price = &price
	}
	out.Sanitize()
	return out
}

func defaultModelsByChannel(channel string) []*registry.ModelInfo {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "codex":
		return registry.GetEmbeddedOpenAIModels()
	case "xai", "x-ai", "grok":
		return registry.GetEmbeddedXAIModels()
	default:
		return registry.GetStaticModelDefinitionsByChannel(channel)
	}
}

func DefaultPrices() map[string]Price {
	var prices map[string]Price
	if err := json.Unmarshal(defaultPricesJSON, &prices); err != nil || len(prices) == 0 {
		return nil
	}
	out := make(map[string]Price, len(prices))
	for name, price := range prices {
		name = normalizeModelName(name)
		if name == "" {
			continue
		}
		out[name] = price
	}
	return out
}
