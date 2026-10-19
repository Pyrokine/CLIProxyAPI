package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClientApplyPatchConfigurationDefaultsAndSDKDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{name: "omitted", raw: "{}\n"},
		{name: "disabled", raw: "client: {codex: {enable-apply-patch: false}}\n"},
		{name: "enabled", raw: "client: {codex: {enable-apply-patch: true}}\n", want: true},
		{name: "null", raw: "client: {codex: {enable-apply-patch: null}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			var sdk SDKConfig
			if err := yaml.Unmarshal([]byte(tc.raw), &sdk); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Config
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if cfg.Client.Codex.EnableApplyPatch != tc.want || sdk.Client.Codex.EnableApplyPatch != tc.want || decoded.Client.Codex.EnableApplyPatch != tc.want {
				t.Fatalf("client capability = %t, SDK = %t, JSON = %t, want %t", cfg.Client.Codex.EnableApplyPatch, sdk.Client.Codex.EnableApplyPatch, decoded.Client.Codex.EnableApplyPatch, tc.want)
			}
		})
	}
}

func TestClientApplyPatchConfigurationRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decode func(*Config) error
	}{
		{name: "YAML", decode: func(cfg *Config) error {
			return yaml.Unmarshal([]byte("client:\n  codex:\n    enable-apply-patch: true\n"), cfg)
		}},
		{name: "JSON", decode: func(cfg *Config) error {
			return json.Unmarshal([]byte(`{"client":{"codex":{"enable-apply-patch":true}}}`), cfg)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if cfg.Client.Codex.EnableApplyPatch {
				t.Fatal("default enabled")
			}
			if err := tc.decode(&cfg); err != nil {
				t.Fatal(err)
			}
			if !cfg.Client.Codex.EnableApplyPatch {
				t.Fatal("explicit setting was not decoded")
			}
			cloned, err := Clone(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !cloned.Client.Codex.EnableApplyPatch {
				t.Fatal("clone lost setting")
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("# keep comment\nclient:\n  codex:\n    enable-apply-patch: false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{true, false} {
				cloned.Client.Codex.EnableApplyPatch = enabled
				if err := SaveConfigPreserveComments(path, cloned); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var loaded Config
				if err := yaml.Unmarshal(data, &loaded); err != nil {
					t.Fatal(err)
				}
				if loaded.Client.Codex.EnableApplyPatch != enabled {
					t.Fatalf("persisted capability = %t, want %t", loaded.Client.Codex.EnableApplyPatch, enabled)
				}
			}
			if !cfg.Client.Codex.EnableApplyPatch {
				t.Fatal("clone changed original")
			}
		})
	}
}
