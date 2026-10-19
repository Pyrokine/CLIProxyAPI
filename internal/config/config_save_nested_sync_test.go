package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSaveConfigRemovesDeletedPluginSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(`plugins:
  enabled: true
  configs:
    alpha:
      enabled: true
      # keep this setting
      keep: yes
      obsolete: old
      nested:
        active: yes
        deleted: old
    beta:
      enabled: true
`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(initial, &cfg); err != nil {
		t.Fatal(err)
	}
	alpha := cfg.Plugins.Configs["alpha"]
	removeMapKey(&alpha.Raw, "obsolete")
	nested := alpha.Raw.Content[findMapKeyIndex(&alpha.Raw, "nested")+1]
	removeMapKey(nested, "deleted")
	cfg.Plugins.Configs["alpha"] = alpha
	delete(cfg.Plugins.Configs, "beta")
	if err := SaveConfigPreserveComments(path, &cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result yaml.Node
	if err := yaml.Unmarshal(written, &result); err != nil {
		t.Fatal(err)
	}
	plugins := result.Content[0].Content[findMapKeyIndex(result.Content[0], "plugins")+1]
	configs := plugins.Content[findMapKeyIndex(plugins, "configs")+1]
	alphaNode := configs.Content[findMapKeyIndex(configs, "alpha")+1]
	if findMapKeyIndex(configs, "beta") >= 0 || findMapKeyIndex(alphaNode, "obsolete") >= 0 ||
		findMapKeyIndex(alphaNode.Content[findMapKeyIndex(alphaNode, "nested")+1], "deleted") >= 0 {
		t.Fatalf("removed plugin settings persisted: %s", written)
	}
	if !strings.Contains(string(written), "# keep this setting") || !strings.Contains(string(written), "keep: yes") {
		t.Fatalf("retained plugin config or comment changed: %s", written)
	}
	delete(cfg.Plugins.Configs, "alpha")
	if err := SaveConfigPreserveComments(path, &cfg); err != nil {
		t.Fatal(err)
	}
	written, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(written, &result); err != nil {
		t.Fatal(err)
	}
	plugins = result.Content[0].Content[findMapKeyIndex(result.Content[0], "plugins")+1]
	if findMapKeyIndex(plugins, "configs") >= 0 {
		t.Fatalf("empty plugin configs section persisted: %s", written)
	}
}

func TestSaveConfigReordersPluginSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(`plugins:
  enabled: true
  configs:
    sample:
      vision_models:
        - name: model-a
          detail: detail-a
          old_prop: stale-a
        - name: model-b
          detail: detail-b
          old_prop: stale-b
`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(initial, &cfg); err != nil {
		t.Fatal(err)
	}
	var reordered yaml.Node
	if err := yaml.Unmarshal([]byte(`vision_models:
  - name: model-b
    detail: new-detail-b
    extra_b: only-in-b
  - name: model-a
    detail: new-detail-a
`), &reordered); err != nil {
		t.Fatal(err)
	}
	plugin := cfg.Plugins.Configs["sample"]
	plugin.Raw = *reordered.Content[0]
	cfg.Plugins.Configs["sample"] = plugin
	if err := SaveConfigPreserveComments(path, &cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Plugins struct {
			Configs map[string]struct {
				VisionModels []struct {
					Name    string `yaml:"name"`
					Detail  string `yaml:"detail"`
					ExtraB  string `yaml:"extra_b"`
					OldProp string `yaml:"old_prop"`
				} `yaml:"vision_models"`
			} `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(written, &result); err != nil {
		t.Fatal(err)
	}
	models := result.Plugins.Configs["sample"].VisionModels
	if len(models) != 2 || models[0].Name != "model-b" || models[0].Detail != "new-detail-b" ||
		models[0].ExtraB != "only-in-b" || models[0].OldProp != "" ||
		models[1].Name != "model-a" || models[1].Detail != "new-detail-a" ||
		models[1].ExtraB != "" || models[1].OldProp != "" || strings.Contains(string(written), "old_prop:") {
		t.Fatalf("reordered plugin models retained stale values: %s", written)
	}
}

func TestSaveConfigAddsPluginToExistingSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("# keep port comment\nport: 8080\n# keep plugins comment\nplugins:\n  enabled: true\n  dir: custom-dir\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	var raw yaml.Node
	if err := yaml.Unmarshal([]byte("enabled: false\n"), &raw); err != nil {
		t.Fatal(err)
	}
	disabled := false
	cfg := &Config{Port: 8080, Plugins: PluginsConfig{
		Enabled: true, Dir: "custom-dir",
		Configs: map[string]PluginInstanceConfig{"new-item": {Enabled: &disabled, Raw: *raw.Content[0]}},
	}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"# keep port comment", "# keep plugins comment", "dir: custom-dir", "new-item:", "enabled: false"} {
		if !strings.Contains(string(written), expected) {
			t.Fatalf("plugin config lost %q: %s", expected, written)
		}
	}
}

func TestSaveConfigRemovesDeletedAPIKeyHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("meta-api-key:\n  - api-key: test-key\n    headers:\n      X-Keep: yes\n      X-Delete: old\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(initial, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg.MetaKey[0].Headers, "X-Delete")
	if err := SaveConfigPreserveComments(path, &cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "X-Delete") || !strings.Contains(string(written), "X-Keep") {
		t.Fatalf("API key headers not replaced: %s", written)
	}
}

func TestSaveConfigPreservesExplicitMetaCoolingAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	disableCooling := false
	retry := 0
	cfg := &Config{MetaKey: []MetaKey{{APIKey: "test-key", DisableCooling: &disableCooling, RequestRetry: &retry}}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "disable-cooling: false") || !strings.Contains(string(written), "request-retry: 0") {
		t.Fatalf("explicit Meta overrides omitted: %s", written)
	}
}

func TestSaveConfigPreservesNewClaudeCacheUserIDFalse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		original string
	}{
		{"existing_cloak", "claude-api-key:\n  - api-key: test-key\n    cloak:\n      mode: always\n"},
		{"new_cloak", "claude-api-key:\n  - api-key: test-key\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.original), 0o600); err != nil {
				t.Fatal(err)
			}
			cacheUserID := false
			cfg := &Config{ClaudeKey: []ClaudeKey{{
				APIKey: "test-key", Cloak: &CloakConfig{CacheUserID: &cacheUserID},
			}}}
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(written), "cache-user-id: false") || strings.Contains(string(written), "mode: always") {
				t.Fatalf("Claude cloak settings not saved: %s", written)
			}
			loaded, err := LoadConfigOptional(path, false)
			if err != nil || len(loaded.ClaudeKey) != 1 || loaded.ClaudeKey[0].Cloak == nil ||
				loaded.ClaudeKey[0].Cloak.CacheUserID == nil || *loaded.ClaudeKey[0].Cloak.CacheUserID {
				t.Fatalf("loaded cloak cache setting = %#v, error = %v", loaded, err)
			}
		})
	}
}

func TestSaveConfigClearsRemovedClaudeCloakFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("claude-api-key:\n  - api-key: test-key\n    cloak:\n      mode: always\n      strict-mode: true\n      cache-user-id: true\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ClaudeKey: []ClaudeKey{{APIKey: "test-key", Cloak: &CloakConfig{Mode: "auto"}}}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "mode: auto") || strings.Contains(string(written), "strict-mode:") ||
		strings.Contains(string(written), "cache-user-id:") {
		t.Fatalf("removed Claude cloak fields remained: %s", written)
	}
}

func TestSaveConfigRemovesClaudeCloak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("claude-api-key:\n  - api-key: test-key\n    cloak:\n      mode: always\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ClaudeKey: []ClaudeKey{{APIKey: "test-key"}}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || len(loaded.ClaudeKey) != 1 || loaded.ClaudeKey[0].Cloak != nil {
		t.Fatalf("Claude cloak remained after removal: %+v, error: %v", loaded, err)
	}
}

func TestSaveConfigPreservesNewZeroPluginSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var raw yaml.Node
	if err := yaml.Unmarshal([]byte("enabled: false\ntimeout: 0\nempty_str: \"\"\nempty_list: []\n"), &raw); err != nil {
		t.Fatal(err)
	}
	disabled := false
	cfg := &Config{Port: 8080, Plugins: PluginsConfig{Configs: map[string]PluginInstanceConfig{
		"zero-plugin": {Enabled: &disabled, Raw: *raw.Content[0]},
	}}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"zero-plugin:", "enabled: false", "timeout: 0", "empty_str: \"\"", "empty_list: []"} {
		if !strings.Contains(string(written), field) {
			t.Fatalf("plugin setting %q omitted: %s", field, written)
		}
	}
	var loaded Config
	if err := yaml.Unmarshal(written, &loaded); err != nil {
		t.Fatal(err)
	}
	plugin, ok := loaded.Plugins.Configs["zero-plugin"]
	if !ok || plugin.Enabled == nil || *plugin.Enabled {
		t.Fatalf("zero plugin not restored: %#v", loaded.Plugins.Configs)
	}
}
