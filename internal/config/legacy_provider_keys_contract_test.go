package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOriginalLegacyProviderKeysLoadAndSave(t *testing.T) {
	const original = `# retained legacy input
port: 8317
generative-language-api-key: [" synthetic-gemini ", "synthetic-gemini", ""]
openai-compatibility:
  - name: First
    base-url: https://first.synthetic.invalid/v1
    api-keys: [synthetic-first, synthetic-first, synthetic-other]
    headers: {X-Synthetic: retained}
    models: [{name: Model-A, alias: Public-A}]
  - name: Second
    base-url: https://second.synthetic.invalid/v1
    api-keys: [synthetic-first]
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GeminiKey) != 1 || cfg.GeminiKey[0].APIKey != "synthetic-gemini" {
		t.Fatalf("legacy Gemini keys were not decoded: %+v", cfg.GeminiKey)
	}
	if len(cfg.OpenAICompatibility) != 2 || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 2 ||
		len(cfg.OpenAICompatibility[1].APIKeyEntries) != 1 {
		t.Fatalf("legacy provider keys were not decoded independently: %+v", cfg.OpenAICompatibility)
	}
	first := cfg.OpenAICompatibility[0]
	if first.APIKeyEntries[0].APIKey != "synthetic-first" ||
		first.APIKeyEntries[1].APIKey != "synthetic-other" ||
		first.Headers["X-Synthetic"] != "retained" ||
		first.Models[0].Alias != "Public-A" {
		t.Fatalf("original provider fields changed: %+v", first)
	}
	if content, errRead := os.ReadFile(path); errRead != nil || string(content) != original {
		t.Fatalf("loading rewrote the original file: %q, %v", content, errRead)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, found := saved["generative-language-api-key"]; found {
		t.Fatal("explicit save retained the replaced Gemini key")
	}
	for _, provider := range saved["openai-compatibility"].([]any) {
		if _, found := provider.(map[string]any)["api-keys"]; found {
			t.Fatal("explicit save retained replaced OpenAI api-keys")
		}
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.GeminiKey, reloaded.GeminiKey) || !reflect.DeepEqual(
		cfg.OpenAICompatibility, reloaded.OpenAICompatibility,
	) || !strings.Contains(string(data), "# retained legacy input") {
		t.Fatal("canonical save/reload changed the original provider data or comment")
	}
}

func TestLegacyProviderKeysRespectCanonicalPresence(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	for _, sample := range []struct {
		name, modernGemini, modernOpenAI string
		want                             int
	}{
		{"absent", "", "", 1},
		{"null", "gemini-api-key: null\n", "    api-key-entries: null\n", 0},
		{"empty", "gemini-api-key: []\n", "    api-key-entries: []\n", 0},
		{
			"populated", "gemini-api-key: [{api-key: synthetic-modern}]\n",
			"    api-key-entries: [{api-key: synthetic-modern, proxy-url: 'http://" +
				"proxy.synthetic.invalid', disabled: " +
				"true}]\n",
			1,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				data := sample.modernGemini +
					("generative-language-api-key: [synthetic-legacy]\nopenai-compatibility:" +
						"\n  - name: Original\n    base-url: https://original.synthetic.invalid" +
						"\n    api-keys: [synthetic-legacy]\n") + sample.modernOpenAI
				var cfg Config
				if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
					t.Fatal(err)
				}
				if len(cfg.GeminiKey) != sample.want || len(cfg.OpenAICompatibility) != 1 ||
					len(cfg.OpenAICompatibility[0].APIKeyEntries) != sample.want {
					t.Fatalf("canonical presence changed: %+v / %+v", cfg.GeminiKey, cfg.OpenAICompatibility)
				}
				if sample.name == "populated" {
					entry := cfg.OpenAICompatibility[0].APIKeyEntries[0]
					if cfg.GeminiKey[0].APIKey != "synthetic-modern" || entry.APIKey != "synthetic-modern" ||
						entry.ProxyURL != "http://proxy.synthetic.invalid" ||
						!entry.Disabled {
						t.Fatalf("canonical credential fields changed: %+v", entry)
					}
				}
			},
		)
	}
}

func TestLegacyProviderKeysAliasesAndErrors(t *testing.T) {
	const aliases = `defaults: &defaults
  generative-language-api-key: [synthetic-gemini]
<<: *defaults
provider: &provider
  name: Aliased
  base-url: https://aliased.synthetic.invalid
  api-keys: [synthetic-openai]
openai-compatibility: [*provider]
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(aliases), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.GeminiKey) != 1 || len(cfg.OpenAICompatibility) != 1 ||
		len(cfg.OpenAICompatibility[0].APIKeyEntries) != 1 {
		t.Fatal("valid legacy YAML aliases lost their provider keys")
	}
	for _, sample := range []struct{ body, field string }{
		{"generative-language-api-key: {api-key: synthetic-private-value}\n", "generative-language-api-key"},
		{
			"openai-compatibility: [{name: Original, base-url: 'https://original.sy" +
				"nthetic.invalid', api-keys: {api-key: synthetic-private-value}}]\n",
			"api-keys",
		},
	} {
		var parsed Config
		err := yaml.Unmarshal([]byte(sample.body), &parsed)
		if err == nil || !strings.Contains(err.Error(), sample.field) || strings.Contains(
			err.Error(), "synthetic-private-value",
		) {
			t.Fatalf("legacy field error must identify the key without its value: %v", err)
		}
	}
}

func TestRetiredAmpConfigurationIsRejected(t *testing.T) {
	for _, field := range []string{
		"amp-upstream-url", "amp-upstream-api-key", "amp-restrict-management-to-localhost", "amp-model-mappings",
	} {
		t.Run(
			field, func(t *testing.T) {
				for _, prefix := range []string{"", "defaults: &defaults\n  "} {
					body := prefix + field + ": synthetic-private-value\n"
					if prefix != "" {
						body += "<<: *defaults\n"
					}
					var cfg Config
					err := yaml.Unmarshal([]byte(body), &cfg)
					if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(
						err.Error(), "no longer supported",
					) || strings.Contains(err.Error(), "synthetic-private-value") {
						t.Fatalf("retired Amp field must return its migration diagnostic: %v", err)
					}
				}
			},
		)
	}
}
