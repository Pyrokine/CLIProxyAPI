package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestC04ParseXAIInjectXSearch(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`xai:
  inject-x-search: true
`),
	)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if !cfg.XAI.InjectXSearch {
		t.Fatal("xai.inject-x-search = false, want true")
	}
}

func TestC04ParseXAIAPIKey(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`xai-api-key:
  - api-key: " xai-key "
    priority: 3
    weight: 2
    prefix: " /team-xai/ "
    base-url: " https://api.x.ai/v1 "
    websockets: true
    proxy-url: " http://proxy.local "
    headers:
      X-Custom: value
    models:
      - name: grok-4.5
        alias: grok-latest
        display-name: Grok Latest
        max-context-length: 262144
        force-mapping: true
    excluded-models:
      - " grok-3-* "
    disable-cooling: true
`),
	)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.XAIKey) != 1 {
		t.Fatalf("xai-api-key count = %d, want 1", len(cfg.XAIKey))
	}
	entry := cfg.XAIKey[0]
	if entry.APIKey != "xai-key" || entry.BaseURL != "https://api.x.ai/v1" || entry.ProxyURL != "http://proxy.local" {
		t.Fatalf("normalized entry = %+v", entry)
	}
	if entry.Prefix != "team-xai" || !entry.Websockets || !entry.DisableCooling {
		t.Fatalf("routing fields = %+v", entry)
	}
	if entry.Weight == nil || *entry.Weight != 2 {
		t.Fatalf("weight = %v, want 2", entry.Weight)
	}
	if len(entry.Models) != 1 || !entry.Models[0].ForceMapping || entry.Models[0].MaxContextLength != 262144 {
		t.Fatalf("models = %+v", entry.Models)
	}
	if len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "grok-3-*" {
		t.Fatalf("excluded models = %#v", entry.ExcludedModels)
	}
}

func TestC04LoadXAIAPIKeyRejectsMalformedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(
		path, []byte("xai-api-key:\n  - api-key: xai-key\n    base-url: ftp://api.x.ai/v1\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Fatalf("LoadConfig() error = %v", err)
	}
}

func TestC04ParseXAIAPIKeyRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "missing key", yaml: "xai-api-key:\n  - base-url: https://api.x.ai/v1\n", want: "api-key is empty"},
		{
			name: "invalid base URL", yaml: "xai-api-key:\n  - api-key: xai-key\n    base-url: ftp://api.x.ai/v1\n",
			want: "unsupported URL scheme",
		},
		{
			name: "invalid proxy",
			yaml: "xai-api-key:\n  - api-key: xai-key\n    base-url: https://api.x.ai/v1\n    proxy-url: ftp://proxy.local\n",
			want: "unsupported proxy scheme",
		},
		{
			name: "restricted header",
			yaml: "xai-api-key:\n  - api-key: xai-key\n    base-url: https://api.x.ai/v1\n    headers:\n      Authorization: Bearer other\n",
			want: "restricted",
		},
		{
			name: "invalid weight",
			yaml: "xai-api-key:\n  - api-key: xai-key\n    base-url: https://api.x.ai/v1\n    weight: invalid\n",
			want: "weight must be an integer",
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				_, err := ParseConfigBytes([]byte(tt.yaml))
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want substring %q", err, tt.want)
				}
			},
		)
	}
}
