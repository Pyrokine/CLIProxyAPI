package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetaAPIKeyDefaultsAndSecretBoundary(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("meta-api-key:\n  - api-key: meta-test-key\n    models:\n      - name: muse-4\n        alias: muse\n"))
	if err != nil {
		t.Fatalf("parse Meta API key: %v", err)
	}
	if len(cfg.MetaKey) != 1 || cfg.MetaKey[0].APIKey != "meta-test-key" ||
		cfg.MetaKey[0].BaseURL != "https://api.meta.ai/v1" {
		t.Fatalf("Meta API key was not normalized: %#v", cfg.MetaKey)
	}
	for _, raw := range []string{
		"meta-api-key:\n  - api-key: dca:private-device-token\n",
		"meta-api-key:\n  - api-key: ''\n",
		"meta-api-key:\n  - api-key: meta-key\n    weight: 999999999\n",
	} {
		if _, err = ParseConfigBytes([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid Meta API key configuration: %q", raw)
		}
	}
}

func TestMetaAPIKeyLoadRejectsDCAWithoutChangingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := "host: 127.0.0.1\nmeta-api-key:\n  - api-key: dca:private-device-token\n"
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "DCA tokens require OAuth storage") {
		t.Fatalf("LoadConfig accepted DCA token as Meta API key: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != input {
		t.Fatalf("invalid Meta API key changed config: %v", err)
	}
}

func TestMetaAPIKeyMixedInvalidEntriesRejectWithoutChangingConfig(t *testing.T) {
	const valid = "meta-api-key:\n  - api-key: ' LLM|valid '\n"
	cfg, err := ParseConfigBytes([]byte(valid))
	if err != nil || len(cfg.MetaKey) != 1 || cfg.MetaKey[0].APIKey != "LLM|valid" || cfg.MetaKey[0].BaseURL != "https://api.meta.ai/v1" {
		t.Fatalf("valid Meta entry = %+v, error = %v", cfg, err)
	}
	for _, entry := range []string{"{}", "api-key: '   '", "base-url: https://api.meta.ai/v1", "headers: {X-Trace: placeholder}", "api-key: dca:requires-oauth-storage", "api-key: DCA:requires-oauth-storage"} {
		t.Run(entry, func(t *testing.T) {
			data := []byte(valid + "  - " + entry + "\n")
			parsed, err := ParseConfigBytes(data)
			if err == nil || parsed != nil || !strings.Contains(err.Error(), "meta-api-key[1].api-key") {
				t.Fatalf("mixed Meta parse = %+v, error = %v", parsed, err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err == nil || loaded != nil || !strings.Contains(err.Error(), "meta-api-key[1].api-key") {
				t.Fatalf("mixed Meta load = %+v, error = %v", loaded, err)
			}
			written, err := os.ReadFile(path)
			if err != nil || string(written) != string(data) {
				t.Fatalf("rejected Meta load changed file: %v", err)
			}
		})
	}
}

func TestMetaOAuthRulesParse(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`oauth-model-alias:
  meta:
    - name: muse-spark-1.3
      alias: muse-latest
      fork: true
      force-mapping: true
oauth-excluded-models:
  meta: [muse-spark-1.1]
oauth-request-scoped-errors:
  meta:
    - status: 400
      match: [context_length_exceeded]
      action: stop
`))
	if err != nil {
		t.Fatal(err)
	}
	aliases := cfg.OAuthModelAlias["meta"]
	if len(aliases) != 1 || aliases[0].Name != "muse-spark-1.3" || aliases[0].Alias != "muse-latest" || !aliases[0].Fork || !aliases[0].ForceMapping {
		t.Fatalf("Meta OAuth model aliases: %+v", aliases)
	}
	if excluded := cfg.OAuthExcludedModels["meta"]; len(excluded) != 1 || excluded[0] != "muse-spark-1.1" {
		t.Fatalf("Meta OAuth model exclusions: %+v", excluded)
	}
	if rules := cfg.OAuthRequestScopedErrors["meta"]; len(rules) != 1 || rules[0].Status != 400 || rules[0].Action != "stop" || len(rules[0].Match) != 1 || rules[0].Match[0] != "context_length_exceeded" {
		t.Fatalf("Meta OAuth error rules: %+v", rules)
	}
}
