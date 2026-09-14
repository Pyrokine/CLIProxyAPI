package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveConfigPreserveCommentsClearsConfigAPIKeyProvider(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(`host: 127.0.0.1
port: 8317
auth:
  providers:
    config-api-key:
      api-key-entries:
        - api-key: stale-entry
      api-keys:
        - stale-legacy
`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Host: "127.0.0.1", Port: 8317, APIKeys: nil}
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	for _, forbidden := range []string{"config-api-key", "stale-entry", "stale-legacy"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("cleared API key provider still contains %q:\n%s", forbidden, text)
		}
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.APIKeys) != 0 {
		t.Fatalf("reloaded API keys = %#v, want empty", loaded.APIKeys)
	}
}
