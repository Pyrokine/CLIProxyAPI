package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseConfigBytesClaudeCodeModelListCloaking(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "defaults to cloaking enabled",
			yaml: "port: 8317\n",
			want: false,
		},
		{
			name: "disables model list cloaking",
			yaml: "claude-code:\n  disable-cloaking-model-list: true\n",
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				cfg, errParse := ParseConfigBytes([]byte(test.yaml))
				if errParse != nil {
					t.Fatalf("ParseConfigBytes() error = %v", errParse)
				}
				if got := cfg.ClaudeCode.DisableCloakingModelList; got != test.want {
					t.Fatalf("DisableCloakingModelList = %t, want %t", got, test.want)
				}
			},
		)
	}
}

func TestClaudeCloakNormalizationOnParseAndLoad(t *testing.T) {
	for _, test := range []struct {
		name, cloak, mode string
		words             []string
		cacheUserID       *bool
	}{
		{name: "nil cloak"},
		{name: "trim fields and preserve false", cloak: "    cloak:\n      mode: '  never  '\n      strict-mode: false\n      cache-user-id: false\n      sensitive-words: [' alpha ', '', '  ', ' beta ']\n", mode: "never", words: []string{"alpha", "beta"}, cacheUserID: new(false)},
		{name: "empty words and preserve true", cloak: "    cloak:\n      mode: ' always '\n      cache-user-id: true\n      sensitive-words: ['', '  ']\n", mode: "always", cacheUserID: new(true)},
		{name: "preserve unknown mode", cloak: "    cloak:\n      mode: ' future-mode '\n      cache-user-id: null\n", mode: "future-mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte("claude-api-key:\n  - api-key: synthetic-key\n" + test.cloak)
			parsed, err := ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfigOptional(path, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range []*Config{parsed, loaded} {
				if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].APIKey != "synthetic-key" {
					t.Fatalf("Claude credentials changed: %+v", cfg.ClaudeKey)
				}
				cloak := cfg.ClaudeKey[0].Cloak
				if test.cloak == "" {
					if cloak != nil {
						t.Fatalf("nil cloak became %+v", cloak)
					}
					continue
				}
				if cloak == nil || cloak.Mode != test.mode || cloak.StrictMode || !reflect.DeepEqual(cloak.SensitiveWords, test.words) || !reflect.DeepEqual(cloak.CacheUserID, test.cacheUserID) {
					t.Fatalf("cloak = %+v, want mode %q, words %v, cache-user-id %v", cloak, test.mode, test.words, test.cacheUserID)
				}
			}
		})
	}
}

func TestParseConfigBytesClaudeModelLevelCooling(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		want       bool
	}{
		{name: "default", yaml: "port: 8317\n"},
		{name: "enabled", yaml: "claude:\n  model-level-cooling: true\n", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseConfigBytes: %v", err)
			}
			if cfg.Claude.ModelLevelCooling != tt.want {
				t.Fatalf("ModelLevelCooling = %t, want %t", cfg.Claude.ModelLevelCooling, tt.want)
			}
		})
	}
}
