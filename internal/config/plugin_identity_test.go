package config

import (
	"strings"
	"testing"
)

func TestNormalizePluginsConfigCanonicalizesPluginID(t *testing.T) {
	cfg := &Config{Plugins: PluginsConfig{Configs: map[string]PluginInstanceConfig{
		"Alpha": {},
	}}}

	cfg.NormalizePluginsConfig()

	if _, exists := cfg.Plugins.Configs["alpha"]; !exists {
		t.Fatalf("normalized plugin configs = %#v, want alpha", cfg.Plugins.Configs)
	}
	if _, exists := cfg.Plugins.Configs["Alpha"]; exists {
		t.Fatalf("mixed-case plugin config remains: %#v", cfg.Plugins.Configs)
	}
}

func TestValidateConfigWarnsDuplicateCaseInsensitivePluginIDs(t *testing.T) {
	cfg := &Config{Plugins: PluginsConfig{Configs: map[string]PluginInstanceConfig{
		"Alpha": {},
		"alpha": {},
	}}}

	warnings := ValidateConfig(cfg)
	for _, warning := range warnings {
		if strings.Contains(warning, "duplicate plugin ids") {
			return
		}
	}
	t.Fatalf("warnings = %#v, want duplicate plugin id warning", warnings)
}
