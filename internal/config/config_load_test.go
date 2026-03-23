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
