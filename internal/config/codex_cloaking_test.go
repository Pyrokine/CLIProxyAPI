package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptionalCodexCloaking(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("codex:\n  disable-codex-cloaking: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if !cfg.Codex.DisableCodexCloaking {
		t.Fatal("DisableCodexCloaking = false, want true")
	}
}

func TestSaveConfigPreservesCodexKeyCloakingOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("codex:\n  disable-codex-cloaking: true\ncodex-api-key:\n  - api-key: test-key\n    base-url: https://example.test\n")
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	cfg.CodexKey[0].DisableCodexCloaking = &disabled
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CodexKey[0].DisableCodexCloaking == nil || *reloaded.CodexKey[0].DisableCodexCloaking {
		t.Fatalf("explicit false override lost: %+v", reloaded.CodexKey[0])
	}
	cfg.CodexKey[0].DisableCodexCloaking = nil
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err = LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CodexKey[0].DisableCodexCloaking != nil || !reloaded.Codex.DisableCodexCloaking {
		t.Fatalf("cleared override still present or global changed: %+v", reloaded)
	}
}
