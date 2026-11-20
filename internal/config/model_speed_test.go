package config

import (
	"slices"
	"testing"
)

func TestModelSpeedOptionsKeepModelAndChannelBoundaries(t *testing.T) {
	for _, test := range []struct {
		model, channel string
		fast, ultra    bool
	}{
		{"gpt-6.2-astra", "codex", true, true},
		{"gpt-6.1-sol", "openai-compatibility", true, true},
		{"gpt-6.1-terra", "codex", true, false},
		{"gpt-5.5", "codex", true, false},
		{"gpt-5-mini", "openai-compatibility", true, false},
		{"gpt-4.1", "openai-compatibility", true, false},
		{"gpt-4.1-mini", "openai-compatibility", true, false},
		{"gpt-4o", "openai-compatibility", true, false},
		{"gpt-4o-mini", "openai-compatibility", true, false},
		{"o3", "openai-compatibility", true, false},
		{"claude-fable-5-2", "claude", true, false},
		{"claude-opus-5-5", "claude", true, false},
		{"claude-haiku-5-5", "claude", false, false},
		{"gpt-image-2.6-flare", "codex", false, false},
		{"gpt-6.2-astra", "meta", false, false},
		{"unknown-model", "codex", false, false},
	} {
		options := ModelSpeedOptions(test.model, test.channel)
		if slices.Contains(options, "fast") != test.fast || slices.Contains(options, "ultrafast") != test.ultra {
			t.Errorf("%s/%s options = %v", test.channel, test.model, options)
		}
		if !ModelSpeedAllowed(test.model, test.channel, "inherit") || ModelSpeedAllowed(
			test.model, test.channel, "turbo",
		) {
			t.Errorf("%s speed validation changed", test.model)
		}
	}
}

func TestModelCatalogSpeedResolvesAliasAndExplicitInheritance(t *testing.T) {
	cfg := &SDKConfig{
		ModelCatalog: ModelCatalogConfig{
			Models: []ModelCatalogModel{
				{Name: "gpt-6.2-astra", Channel: "codex", Aliases: []string{"public-astra"}, Speed: "ultrafast"},
				{Name: "other-client", Channel: "codex", Speed: "inherit"},
			},
		},
	}
	if got := cfg.ModelCatalogSpeed("codex", "public-astra(high)", "gpt-6.2-astra"); got != "ultrafast" {
		t.Fatalf("alias speed = %q", got)
	}
	if got := cfg.ModelCatalogSpeed("openai-compatibility", "public-astra"); got != "" {
		t.Fatalf("Codex speed leaked into another channel: %q", got)
	}
	if got := cfg.ModelCatalogSpeed("codex", "other-client", "gpt-6.2-astra"); got != "inherit" {
		t.Fatalf("explicit client inheritance = %q", got)
	}
}
