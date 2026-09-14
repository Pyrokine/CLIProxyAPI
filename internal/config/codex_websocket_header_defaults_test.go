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
  user-agent: "  claude-cli/2.1.220 (external, cli)  "
  package-version: "  0.94.0  "
  runtime-version: "  v26.3.0  "
  os: "  MacOS  "
  arch: "  arm64  "
  timeout: "  600  "
  timezone: "  Asia/Singapore  "
  stabilize-device-profile: false
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if got := cfg.ClaudeHeaderDefaults.UserAgent; got != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("UserAgent = %q", got)
	}
	if got := cfg.ClaudeHeaderDefaults.PackageVersion; got != "0.94.0" {
		t.Fatalf("PackageVersion = %q", got)
	}
	if got := cfg.ClaudeHeaderDefaults.RuntimeVersion; got != "v26.3.0" {
		t.Fatalf("RuntimeVersion = %q", got)
	}
	if got := cfg.ClaudeHeaderDefaults.OS; got != "MacOS" {
		t.Fatalf("OS = %q", got)
	}
	if got := cfg.ClaudeHeaderDefaults.Arch; got != "arm64" {
		t.Fatalf("Arch = %q", got)
	}
	if got := cfg.ClaudeHeaderDefaults.Timeout; got != "600" {
		t.Fatalf("Timeout = %q", got)
	}
	if got := cfg.ClaudeHeaderDefaults.Timezone; got != "Asia/Singapore" {
		t.Fatalf("Timezone = %q", got)
	}
	if cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil || *cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile {
		t.Fatalf("StabilizeDeviceProfile = %v, want false", cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile)
	}
}
