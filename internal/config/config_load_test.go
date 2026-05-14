package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_WebsocketAuth_DefaultTrue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	// Minimal config without ws-auth field.
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if !cfg.WebsocketAuth {
		t.Fatal("expected WebsocketAuth to default to true")
	}
}

func TestLoadConfig_WebsocketAuth_ExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	content := "port: 8317\nws-auth: false\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.WebsocketAuth {
		t.Fatal("expected WebsocketAuth to be false when explicitly set")
	}
}

func TestLoadConfig_WebsocketAuth_ExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	content := "port: 8317\nws-auth: true\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if !cfg.WebsocketAuth {
		t.Fatal("expected WebsocketAuth to be true when explicitly set")
	}
}

func TestLoadConfig_RemoteManagementDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository {
		t.Fatalf("panel repo = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, DefaultPanelGitHubRepository)
	}
	if cfg.RemoteManagement.CPAGitHubRepository != defaultCPAGitHubRepository {
		t.Fatalf("cpa repo = %q, want %q", cfg.RemoteManagement.CPAGitHubRepository, defaultCPAGitHubRepository)
	}
	if cfg.RemoteManagement.AutoUpdateCPA {
		t.Fatal("expected auto-update-cpa to default to false")
	}
}
