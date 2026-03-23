package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_ClaudeHeaderDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := []byte(`
claude-header-defaults:
  user-agent: "  my-codex-client/1.0  "
  package-version: "  2.3.4  "
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if got := cfg.ClaudeHeaderDefaults.UserAgent; got != "  my-codex-client/1.0  " {
		t.Fatalf("UserAgent = %q, want %q", got, "  my-codex-client/1.0  ")
	}
	if got := cfg.ClaudeHeaderDefaults.PackageVersion; got != "  2.3.4  " {
		t.Fatalf("PackageVersion = %q, want %q", got, "  2.3.4  ")
	}
}
