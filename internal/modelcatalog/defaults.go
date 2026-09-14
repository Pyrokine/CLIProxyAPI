package modelcatalog

import (
	_ "embed"
	"encoding/json"
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

var documentedProviderDefaults = []Model{
	{
		Name: "deepseek-flash", Provider: "DeepSeek", Channel: "claude", DisplayName: "DeepSeek V4.1 Flash",
	},
	{Name: "deepseek-v4-pro", Provider: "DeepSeek", Channel: "claude", DisplayName: "DeepSeek V4 Pro"},
	{Name: "glm-5.3", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-5.3-flash", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-5.2", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-5.1", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-5-turbo", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-5", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.7", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.7-flash", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.7-flashx", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.6", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.5-air", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.5-airx", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.5-flash", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4-flash-250414", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4-flashx-250414", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-5v-turbo", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.6v", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "autoglm-phone", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.6v-flash", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.6v-flashx", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4v-flash", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.1v-thinking-flashx", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4.1v-thinking-flash", Provider: "GLM", Channel: "openai-compatibility"},
	{Name: "glm-4-voice", Provider: "GLM", Channel: "openai-compatibility"},
}

func DefaultFile() File {
	byName := make(map[string]int)
	out := File{
		SchemaVersion:   SchemaVersion,
		DefaultsVersion: DefaultsVersion,
		Prices:          DefaultPrices(),
	}
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
	for _, model := range documentedProviderDefaults {
		ensure(model.Name, model.Provider, model.Channel, model.DisplayName, false)
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
