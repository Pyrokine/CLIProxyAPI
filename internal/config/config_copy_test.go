package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCloneCreatesIndependentConfig(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
api-keys:
  - client-key
api-key-aliases:
  client-key: primary
gemini-api-key:
  - api-key: provider-key
    headers:
      X-Test: value
oauth-excluded-models:
  codex:
    - gpt-*
plugins:
  configs:
    sample:
      enabled: true
      custom:
        nested: value
model-catalog:
  models:
    - name: model-a
      aliases:
        - alias-a
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	cfg.Home = HomeConfig{
		Enabled: true,
		NodeID:  "node-a",
		TLS: HomeTLSConfig{
			ClientCert:          "cert-a",
			ClientKey:           "key-a",
			UseTargetServerName: true,
		},
	}

	cloned, err := Clone(cfg)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if cloned == cfg {
		t.Fatal("Clone returned the source pointer")
	}
	if cloned.Home != cfg.Home {
		t.Fatalf("Home = %#v, want %#v", cloned.Home, cfg.Home)
	}

	cloned.APIKeys[0] = "changed-client-key"
	cloned.APIKeyAliases["client-key"] = "changed-alias"
	cloned.GeminiKey[0].Headers["X-Test"] = "changed-header"
	cloned.OAuthExcludedModels["codex"][0] = "changed-model"
	cloned.ModelCatalog.Models[0].Aliases[0] = "changed-model-alias"
	plugin := cloned.Plugins.Configs["sample"]
	*plugin.Enabled = false
	plugin.Raw.Content[3].Content[1].Value = "changed-plugin-value"
	cloned.Plugins.Configs["sample"] = plugin
	cloned.Home.NodeID = "node-b"

	if cfg.APIKeys[0] != "client-key" {
		t.Fatalf("source APIKeys = %#v", cfg.APIKeys)
	}
	if cfg.APIKeyAliases["client-key"] != "primary" {
		t.Fatalf("source APIKeyAliases = %#v", cfg.APIKeyAliases)
	}
	if cfg.GeminiKey[0].Headers["X-Test"] != "value" {
		t.Fatalf("source headers = %#v", cfg.GeminiKey[0].Headers)
	}
	if cfg.OAuthExcludedModels["codex"][0] != "gpt-*" {
		t.Fatalf("source OAuthExcludedModels = %#v", cfg.OAuthExcludedModels)
	}
	if cfg.ModelCatalog.Models[0].Aliases[0] != "alias-a" {
		t.Fatalf("source model catalog = %#v", cfg.ModelCatalog)
	}
	if cfg.Home.NodeID != "node-a" {
		t.Fatalf("source Home = %#v", cfg.Home)
	}
	originalPlugin := cfg.Plugins.Configs["sample"]
	if originalPlugin.Enabled == nil || !*originalPlugin.Enabled {
		t.Fatalf("source plugin enabled = %#v", originalPlugin.Enabled)
	}
	pluginYAML, err := yaml.Marshal(originalPlugin)
	if err != nil {
		t.Fatalf("marshal source plugin: %v", err)
	}
	if !strings.Contains(string(pluginYAML), "nested: value") {
		t.Fatalf("source plugin changed:\n%s", pluginYAML)
	}
}
