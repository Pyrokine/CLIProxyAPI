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
		SDKConfig: SDKConfig{ModelCatalog: ModelCatalogConfig{Models: []ModelCatalogModel{{Name: "runtime-only"}}}},
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
		SDKConfig: SDKConfig{
			APIKeyRules: map[string]APIKeyRule{
				"sk-test": {BlockedModels: []string{"claude-sonnet-4-6"}},
			},
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
