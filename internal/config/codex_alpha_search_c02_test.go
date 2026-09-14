package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigPreservesCodexAlphaSearchEndpoint(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := []byte(`codex-api-key:
  - api-key: codex-key
    base-url: https://codex.example.com/v1
    alpha-search: true
`)
	if errWrite := os.WriteFile(configPath, configYAML, 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	cfg, errLoad := LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	if len(cfg.CodexKey) != 1 {
		t.Fatalf("Codex key count = %d, want 1", len(cfg.CodexKey))
	}
	credential := cfg.CodexKey[0]
	if !credential.AlphaSearch {
		t.Fatal("alpha-search = false, want true")
	}
	if credential.BaseURL != "https://codex.example.com/v1" {
		t.Fatalf("base-url = %q", credential.BaseURL)
	}
}

func TestCodexAlphaSearchCapabilityIsCodexOnly(t *testing.T) {
	cfg := &Config{
		CodexKey: []CodexKey{
			{
				APIKey:      "codex-key",
				BaseURL:     "https://codex.example.com/v1",
				AlphaSearch: true,
			},
		},
		XAIKey: []XAIKey{
			{
				APIKey:      "xai-key",
				BaseURL:     "https://api.x.ai/v1",
				AlphaSearch: true,
			},
		},
	}

	cfg.SanitizeCodexKeys()
	cfg.SanitizeXAIKeys()

	if len(cfg.CodexKey) != 1 || !cfg.CodexKey[0].AlphaSearch {
		t.Fatalf("Codex alpha-search was not preserved: %#v", cfg.CodexKey)
	}
	if len(cfg.XAIKey) != 1 || cfg.XAIKey[0].AlphaSearch {
		t.Fatalf("xAI retained Codex alpha-search capability: %#v", cfg.XAIKey)
	}
}
