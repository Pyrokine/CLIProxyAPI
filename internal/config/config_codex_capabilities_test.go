package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexConfigurationUpdateCapabilityDecodingAndClone(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		decode func(*Config) error
	}{
		{
			name: "YAML",
			decode: func(cfg *Config) error {
				return yaml.Unmarshal([]byte("codex-api-key:\n  - models:\n      - name: custom-one\n        support-configuration-update: true\n      - name: custom-two\n"), cfg)
			},
		},
		{
			name: "JSON",
			decode: func(cfg *Config) error {
				return json.Unmarshal([]byte(`{"codex-api-key":[{"models":[{"name":"custom-one","support-configuration-update":true},{"name":"custom-two"}]}]}`), cfg)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var cfg Config
			if err := testCase.decode(&cfg); err != nil {
				t.Fatalf("decode config: %v", err)
			}
			cloned, err := Clone(&cfg)
			if err != nil {
				t.Fatalf("clone config: %v", err)
			}
			for _, value := range []*Config{&cfg, cloned} {
				if len(value.CodexKey) != 1 || len(value.CodexKey[0].Models) != 2 ||
					!value.CodexKey[0].Models[0].SupportConfigurationUpdate ||
					value.CodexKey[0].Models[1].SupportConfigurationUpdate {
					t.Fatalf("Codex model capability changed: %+v", value.CodexKey)
				}
			}
		})
	}
}
