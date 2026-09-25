package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveConfigPreserveCommentsWithModelsJSONRemovesModelAliasFromYAML(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	initial := []byte(`host: 127.0.0.1
port: 8317
oauth-model-alias:
  codex:
    - name: stale
      alias: stale-alias
model-catalog:
  models:
    - name: stale-model
`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "models.json"), []byte(`{"schema_version":1,"defaults_version":"test","models":[]}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Host: "127.0.0.1",
		Port: 8317,
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"codex": {{Name: "catalog-name", Alias: "catalog-alias", Fork: true}},
		},
		ModelCatalog: ModelCatalogConfig{Models: []ModelCatalogModel{{Name: "runtime-only"}}},
	}

	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	if strings.Contains(text, "oauth-model-alias:") || strings.Contains(text, "stale-alias") || strings.Contains(
		text, "catalog-alias",
	) {
		t.Fatalf("expected OAuth model aliases to stay external, got:\n%s", text)
	}
	if strings.Contains(text, "model-catalog:") || strings.Contains(text, "runtime-only") {
		t.Fatalf("expected model catalog to stay external, got:\n%s", text)
	}
}

func TestSaveConfigPreserveCommentsPrunesAPIKeyRules(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(`host: 127.0.0.1
port: 8317
api-key-rules:
  sk-test:
    blocked-models:
      - claude-opus-4-7
  sk-empty:
    blocked-models:
      - gemini-2.5-pro
oauth-excluded-models:
  claude:
    - claude-opus-*
  gemini:
    - gemini-2.5-pro
`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Host: "127.0.0.1",
		Port: 8317,
		APIKeyRules: map[string]APIKeyRule{
			"sk-test": {BlockedModels: []string{"claude-sonnet-4-6"}},
		},
		OAuthExcludedModels: map[string][]string{
			"claude": {"claude-opus-*"},
			"gemini": {"gemini-2.5-pro"},
		},
	}

	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	if strings.Contains(text, "claude-opus-4-7") || strings.Contains(text, "sk-empty") {
		t.Fatalf("expected stale API key rules to be pruned, got:\n%s", text)
	}
	if !strings.Contains(text, "claude-sonnet-4-6") || !strings.Contains(text, "gemini-2.5-pro") {
		t.Fatalf("expected remaining API key rule and global excludes to be preserved, got:\n%s", text)
	}
}

func TestSaveConfigPreserveCommentsReplacesFileAtomically(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("host: 127.0.0.1\nport: 8317\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if err = SaveConfigPreserveComments(configPath, &Config{Host: "127.0.0.1", Port: 9090}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("config file was updated in place")
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", after.Mode().Perm())
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "port: 9090") {
		t.Fatalf("config missing updated port:\n%s", written)
	}
}

func TestSaveConfigPreserveCommentsCreatesMissingFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	enabled := false
	maxIdle := 0
	cfg := &Config{
		Host:  "127.0.0.1",
		Port:  9090,
		Debug: true,
		Antigravity: AntigravityConfig{
			ConnectionPool: AntigravityConnectionPoolConfig{
				Enabled:             &enabled,
				MaxIdleConnsPerHost: &maxIdle,
			},
		},
	}
	cfg.APIKeys = []string{"sk-created"}

	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("save missing config: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat created config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load created config: %v", err)
	}
	if loaded.Port != 9090 || !loaded.Debug || len(loaded.APIKeys) != 1 || loaded.APIKeys[0] != "sk-created" {
		t.Fatalf("created config = %#v", loaded)
	}
	pool := loaded.Antigravity.ConnectionPool
	if pool.Enabled == nil || *pool.Enabled || pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 0 {
		t.Fatalf("created pool = %#v, want explicit false and zero", pool)
	}
}

func TestSaveConfigAddsExplicitAntigravityPoolFalseAndZero(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	enabled := false
	maxIdle := 0
	cfg := &Config{
		Host: "127.0.0.1",
		Antigravity: AntigravityConfig{
			ConnectionPool: AntigravityConnectionPoolConfig{
				Enabled:             &enabled,
				MaxIdleConnsPerHost: &maxIdle,
			},
		},
	}
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := loaded.Antigravity.ConnectionPool
	if pool.Enabled == nil || *pool.Enabled || pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 0 {
		t.Fatalf("saved pool = %#v, want explicit false and zero", pool)
	}
}

func TestSaveConfigPreservesAntigravityPoolYAMLAndReloadSemantics(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(`# keep pool comment
antigravity:
  connection-pool:
    enabled: false
    idle-conn-timeout: ""
    max-idle-conns-per-host: 0
    future-options: {}
codex:
  orphan-delegation-compatibility: true
`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := cfg.Antigravity.ConnectionPool
	if pool.Enabled == nil || *pool.Enabled || pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 0 {
		t.Fatalf("loaded pool = %#v, want explicit false and zero", pool)
	}
	if !cfg.Codex.OrphanDelegationCompatibility {
		t.Fatal("orphan delegation setting was not loaded")
	}
	cfg, err = Clone(cfg)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	negative := -3
	cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost = &negative
	if err = SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	text := string(written)
	for _, expected := range []string{
		"# keep pool comment",
		"enabled: false",
		"idle-conn-timeout: \"\"",
		"max-idle-conns-per-host: -3",
		"future-options: {}",
		"orphan-delegation-compatibility: true",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("saved config missing %q:\n%s", expected, text)
		}
	}

	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	reloadedPool := reloaded.Antigravity.ConnectionPool
	if reloadedPool.Enabled == nil || *reloadedPool.Enabled || reloadedPool.MaxIdleConnsPerHost == nil ||
		*reloadedPool.MaxIdleConnsPerHost != -3 {
		t.Fatalf("reloaded pool = %#v, want explicit false and negative max idle", reloadedPool)
	}
	if !reloaded.Codex.OrphanDelegationCompatibility {
		t.Fatal("orphan delegation setting was not preserved across save and reload")
	}
}

func TestSaveConfigPreservesEmptyAntigravityConnectionPoolMap(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("antigravity:\n  connection-pool: {}\n")
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Antigravity.ConnectionPool.Enabled != nil || cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost != nil {
		t.Fatalf("empty pool gained explicit values: %#v", cfg.Antigravity.ConnectionPool)
	}
	cfg, err = Clone(cfg)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err = SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(written), "connection-pool: {}") {
		t.Fatalf("empty connection-pool map was not preserved:\n%s", written)
	}
	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Antigravity.ConnectionPool.Enabled != nil ||
		reloaded.Antigravity.ConnectionPool.MaxIdleConnsPerHost != nil {
		t.Fatalf("reloaded empty pool gained explicit values: %#v", reloaded.Antigravity.ConnectionPool)
	}
}

func TestSaveConfigPreserveCommentsFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	original := []byte("host: 127.0.0.1\nport: 8317\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore directory mode: %v", err)
		}
	}()

	if err := SaveConfigPreserveComments(configPath, &Config{Host: "127.0.0.1", Port: 9090}); err == nil {
		t.Fatal("SaveConfigPreserveComments succeeded in a non-writable directory")
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(original) {
		t.Fatalf("config changed after failed save:\n%s", written)
	}
}

func TestSaveConfigPreserveCommentsPreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.yaml")
	linkPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(targetPath, []byte("host: 127.0.0.1\nport: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatal(err)
	}

	if err := SaveConfigPreserveComments(linkPath, &Config{Host: "127.0.0.1", Port: 9090}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config symlink was replaced")
	}
	written, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "port: 9090") {
		t.Fatalf("symlink target missing updated port:\n%s", written)
	}
}
