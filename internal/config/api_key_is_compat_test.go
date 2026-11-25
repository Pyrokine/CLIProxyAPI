package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAICompatibilityUseMaxCompletionTokensDecoding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		decode func([]byte, any) error
	}{
		{
			name: "YAML",
			raw: "openai-compatibility:\n  - name: synthetic\n    models:\n      - name:" +
				" explicit-true\n        use-max-completion-tokens: true\n      - name:" +
				" explicit-false\n        use-max-completion-tokens: false\n      - nam" +
				"e: omitted\n",
			decode: yaml.Unmarshal,
		},
		{
			name: "JSON",
			raw: `{"openai-compatibility":[{"name":"synthetic","models":[{"name":"explicit-true","use-max-` +
				`completion-tokens":true},{"name":"explicit-false","use-max-completion-tokens":false},{"name"` +
				`:"omitted"}]}]}`,
			decode: json.Unmarshal,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				var cfg Config
				if err := tc.decode([]byte(tc.raw), &cfg); err != nil {
					t.Fatal(err)
				}
				cloned, err := Clone(&cfg)
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range []*Config{&cfg, cloned} {
					if len(value.OpenAICompatibility) != 1 || len(value.OpenAICompatibility[0].Models) != 3 {
						t.Fatalf("OpenAI-compatible models = %+v", value.OpenAICompatibility)
					}
					for i, model := range value.OpenAICompatibility[0].Models {
						want := i == 0
						if model.UseMaxCompletionTokens != want || model.GetUseMaxCompletionTokens() != want {
							t.Fatalf("model %d capability = %+v, want %t", i, model, want)
						}
					}
				}
			},
		)
	}
}

func TestOpenAICompatibilityTokenLimitYAMLWriteback(t *testing.T) {
	const initial = `openai-compatibility:
  - name: fixture
    base-url: https://provider.fixture.invalid/v1
    models:
      - name: explicit
        alias: explicit
        use-max-completion-tokens: true # Retain model comment
      - name: default-model
        alias: default-model
`
	for _, name := range []string{"startup defaults", "provider save"} {
		t.Run(
			name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if name == "startup defaults" {
					EnsureDefaultsInFile(path, cfg)
				} else if err := SaveConfigPreserveComments(path, cfg); err != nil {
					t.Fatal(err)
				}
				checkYAML := func(wantTrue, wantFalse int) {
					t.Helper()
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					text := string(raw)
					if strings.Count(text, "use-max-completion-tokens: true") != wantTrue ||
						strings.Count(text, "use-max-completion-tokens: false") != wantFalse ||
						!strings.Contains(text, "# Retain model comment") {
						t.Fatalf("unexpected token-limit flags in YAML:\n%s", text)
					}
				}
				checkYAML(1, 1)
				cfg.OpenAICompatibility[0].Models[0].UseMaxCompletionTokens = false
				if err := SaveConfigPreserveComments(path, cfg); err != nil {
					t.Fatal(err)
				}
				checkYAML(0, 2)
				current, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if current.OpenAICompatibility[0].Models[0].UseMaxCompletionTokens {
					t.Fatal("disabled token-limit flag was not read back")
				}
			},
		)
	}
}

func TestAPIKeyModelIsCompatConfigDecoding(t *testing.T) {
	const yamlConfig = `gemini-api-key:
  - models:
      - name: gemini-upstream
        alias: gemini-alias
        is-compat: true
      - name: gemini-native
        alias: gemini-native
interactions-api-key:
  - models:
      - name: interactions-upstream
        alias: interactions-alias
        is-compat: true
xai-api-key:
  - models:
      - name: xai-upstream
        alias: xai-alias
        is-compat: true
claude-api-key:
  - models:
      - name: claude-upstream
        alias: claude-alias
        is-compat: true
codex-api-key:
  - models:
      - name: codex-upstream
        alias: codex-alias
        is-compat: true
openai-compatibility:
  - name: deepseek
    models:
      - name: deepseek-upstream
        alias: deepseek-alias
        is-compat: true
      - name: openai-native
        alias: openai-native
`

	var cfg Config
	if errDecode := yaml.Unmarshal([]byte(yamlConfig), &cfg); errDecode != nil {
		t.Fatalf("decode error: %v", errDecode)
	}

	if len(cfg.GeminiKey) != 1 || !cfg.GeminiKey[0].Models[0].IsCompat {
		t.Fatalf("gemini-api-key IsCompat = %+v, want true", cfg.GeminiKey)
	}
	if cfg.GeminiKey[0].Models[1].IsCompat {
		t.Fatal("gemini-api-key omitted IsCompat = true, want default false")
	}
	if len(cfg.InteractionsKey) != 1 || !cfg.InteractionsKey[0].Models[0].IsCompat {
		t.Fatalf("interactions-api-key IsCompat = %+v, want true", cfg.InteractionsKey)
	}
	if len(cfg.XAIKey) != 1 || !cfg.XAIKey[0].Models[0].IsCompat {
		t.Fatalf("xai-api-key IsCompat = %+v, want true", cfg.XAIKey)
	}
	if len(cfg.ClaudeKey) != 1 || !cfg.ClaudeKey[0].Models[0].IsCompat {
		t.Fatalf("claude-api-key IsCompat = %+v, want true", cfg.ClaudeKey)
	}
	if len(cfg.CodexKey) != 1 || !cfg.CodexKey[0].Models[0].IsCompat {
		t.Fatalf("codex-api-key IsCompat = %+v, want true", cfg.CodexKey)
	}
	if len(cfg.OpenAICompatibility) != 1 || !cfg.OpenAICompatibility[0].Models[0].IsCompat {
		t.Fatalf("openai-compatibility IsCompat = %+v, want true", cfg.OpenAICompatibility)
	}
	if cfg.OpenAICompatibility[0].Models[1].IsCompat {
		t.Fatal("openai-compatibility omitted IsCompat = true, want default false")
	}

	cloned, errClone := Clone(&cfg)
	if errClone != nil {
		t.Fatalf("Clone() error = %v", errClone)
	}
	if !cloned.GeminiKey[0].Models[0].IsCompat ||
		!cloned.InteractionsKey[0].Models[0].IsCompat ||
		!cloned.XAIKey[0].Models[0].IsCompat ||
		!cloned.ClaudeKey[0].Models[0].IsCompat ||
		!cloned.CodexKey[0].Models[0].IsCompat ||
		!cloned.OpenAICompatibility[0].Models[0].IsCompat {
		t.Fatalf("Clone() dropped IsCompat: %+v", cloned)
	}
}
