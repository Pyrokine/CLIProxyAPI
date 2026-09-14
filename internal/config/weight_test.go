package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialWeightYAMLValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "negative", content: "gemini-api-key:\n  - api-key: key\n    weight: -1\n"},
		{name: "zero", content: "interactions-api-key:\n  - api-key: key\n    weight: 0\n"},
		{name: "maximum", content: "claude-api-key:\n  - api-key: key\n    weight: 1000000\n"},
		{
			name:    "openai nested",
			content: "openai-compatibility:\n  - name: test\n    base-url: https://example.com\n    api-key-entries:\n      - api-key: key\n        weight: 5\n",
		},
		{name: "fraction", content: "gemini-api-key:\n  - api-key: key\n    weight: 1.5\n", wantErr: true},
		{name: "quoted integer", content: "gemini-api-key:\n  - api-key: key\n    weight: \"7\"\n", wantErr: true},
		{name: "above maximum", content: "gemini-api-key:\n  - api-key: key\n    weight: 1000001\n", wantErr: true},
		{
			name: "integer overflow", content: "gemini-api-key:\n  - api-key: key\n    weight: 9223372036854775808\n",
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				_, err := ParseConfigBytes([]byte(test.content))
				if (err != nil) != test.wantErr {
					t.Fatalf("ParseConfigBytes() error = %v, wantErr = %v", err, test.wantErr)
				}
			},
		)
	}
}

func TestCredentialWeightExplicitZeroPersists(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("gemini-api-key:\n  - api-key: key\n    weight: 0\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.GeminiKey) != 1 || cfg.GeminiKey[0].Weight == nil || *cfg.GeminiKey[0].Weight != 0 {
		t.Fatalf("parsed Gemini weight = %#v, want explicit zero", cfg.GeminiKey)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(configPath, []byte("gemini-api-key:\n  - api-key: key\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err = SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(saved), "weight: 0") {
		t.Fatalf("saved config does not preserve explicit zero weight:\n%s", saved)
	}
}

func TestValidateCredentialWeightsCoversEveryConfiguredFamily(t *testing.T) {
	invalid := maxCredentialWeight + 1
	families := []struct {
		name string
		cfg  *Config
	}{
		{name: "gemini", cfg: &Config{GeminiKey: []GeminiKey{{Weight: &invalid}}}},
		{name: "interactions", cfg: &Config{InteractionsKey: []GeminiKey{{Weight: &invalid}}}},
		{name: "claude", cfg: &Config{ClaudeKey: []ClaudeKey{{Weight: &invalid}}}},
		{name: "vertex", cfg: &Config{VertexCompatAPIKey: []VertexCompatKey{{Weight: &invalid}}}},
		{name: "codex", cfg: &Config{CodexKey: []CodexKey{{Weight: &invalid}}}},
		{
			name: "openai",
			cfg:  &Config{OpenAICompatibility: []OpenAICompatibility{{APIKeyEntries: []OpenAICompatibilityAPIKey{{Weight: &invalid}}}}},
		},
	}
	for _, family := range families {
		t.Run(
			family.name, func(t *testing.T) {
				if err := family.cfg.ValidateCredentialWeights(); err == nil {
					t.Fatal("ValidateCredentialWeights() accepted an above-maximum weight")
				}
			},
		)
	}
}
