package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClonePreservesIndependentSessionAffinitySubagents(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		value *bool
	}{
		{name: "unset"},
		{name: "enabled", value: new(true)},
		{name: "disabled", value: new(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{}
			cfg.Routing.SessionAffinitySubagents = test.value
			cloned, err := Clone(cfg)
			if err != nil {
				t.Fatalf("Clone: %v", err)
			}
			got := cloned.Routing.SessionAffinitySubagents
			if test.value == nil {
				if got != nil {
					t.Fatalf("cloned value = %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *test.value {
				t.Fatalf("cloned value = %v, want %v", got, *test.value)
			}
			if got == test.value {
				t.Fatal("Clone reused the source bool pointer")
			}
			*got = !*got
			if *test.value != (test.name == "enabled") {
				t.Fatal("mutating clone changed source bool")
			}
		})
	}
}

func TestClonePreservesIndependentAntigravityConnectionPoolPointers(t *testing.T) {
	t.Parallel()

	falseValue := false
	trueValue := true
	zero := 0
	negative := -3
	for _, test := range []struct {
		name    string
		enabled *bool
		maxIdle *int
	}{
		{name: "unset"},
		{name: "explicit false and zero", enabled: &falseValue, maxIdle: &zero},
		{name: "enabled and negative", enabled: &trueValue, maxIdle: &negative},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{
				Antigravity: AntigravityConfig{
					ConnectionPool: AntigravityConnectionPoolConfig{
						Enabled:             test.enabled,
						IdleConnTimeout:     "",
						MaxIdleConnsPerHost: test.maxIdle,
					},
				},
			}
			cloned, err := Clone(cfg)
			if err != nil {
				t.Fatalf("Clone: %v", err)
			}
			got := cloned.Antigravity.ConnectionPool
			if test.enabled == nil {
				if got.Enabled != nil {
					t.Fatalf("Enabled = %v, want nil", *got.Enabled)
				}
			} else {
				if got.Enabled == nil || *got.Enabled != *test.enabled {
					t.Fatalf("Enabled = %v, want %v", got.Enabled, *test.enabled)
				}
				if got.Enabled == test.enabled {
					t.Fatal("Clone reused the source enabled pointer")
				}
			}
			if test.maxIdle == nil {
				if got.MaxIdleConnsPerHost != nil {
					t.Fatalf("MaxIdleConnsPerHost = %v, want nil", *got.MaxIdleConnsPerHost)
				}
			} else {
				if got.MaxIdleConnsPerHost == nil || *got.MaxIdleConnsPerHost != *test.maxIdle {
					t.Fatalf("MaxIdleConnsPerHost = %v, want %v", got.MaxIdleConnsPerHost, *test.maxIdle)
				}
				if got.MaxIdleConnsPerHost == test.maxIdle {
					t.Fatal("Clone reused the source max-idle pointer")
				}
			}
		})
	}
}

func TestParseConfigPreservesExplicitSessionAffinitySubagentsFalse(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfigBytes([]byte("routing:\n  session-affinity: true\n  session-affinity-subagents: false\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if cfg.Routing.SessionAffinitySubagents == nil || *cfg.Routing.SessionAffinitySubagents {
		t.Fatalf("session-affinity-subagents = %v, want explicit false", cfg.Routing.SessionAffinitySubagents)
	}
}

func TestCloneCreatesIndependentConfig(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`
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
antigravity:
  sensitive-words:
    - API
    - proxy
devin:
  sensitive-words:
    - sensitive phrase
`),
	)
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
	cloned.Antigravity.SensitiveWords[0] = "changed-sensitive-word"
	cloned.Devin.SensitiveWords[0] = "changed-devin-sensitive-word"
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
	if got := cfg.Antigravity.SensitiveWords[0]; got != "API" {
		t.Fatalf("source Antigravity sensitive word = %q, want API", got)
	}
	if got := cfg.Devin.SensitiveWords[0]; got != "sensitive phrase" {
		t.Fatalf("source Devin sensitive word = %q, want sensitive phrase", got)
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
