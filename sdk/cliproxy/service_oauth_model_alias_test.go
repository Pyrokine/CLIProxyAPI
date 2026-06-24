package cliproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestApplyModelCatalogConfigMigratesConfigAliasIntoCatalog(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "models.json"), []byte(`{
  "schema_version": 1,
  "defaults_version": "test",
  "models": [
    {
      "name": "gpt-5",
      "provider": "GPT",
      "channel": "codex",
      "group": "GPT 5",
      "enabled": true
    }
  ]
}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5", Alias: "g5"}},
		},
	}
	svc := &Service{configPath: configPath}

	aliases, err := svc.applyModelCatalogConfig(cfg)
	if err != nil {
		t.Fatalf("applyModelCatalogConfig: %v", err)
	}
	codexAliases := aliases["codex"]
	if len(codexAliases) != 1 || codexAliases[0].Name != "gpt-5" || codexAliases[0].Alias != "g5" {
		t.Fatalf("codex aliases = %#v", codexAliases)
	}
	if len(cfg.ModelCatalog.Models) != 1 || len(cfg.ModelCatalog.Models[0].Aliases) != 1 || cfg.ModelCatalog.Models[0].Aliases[0] != "g5" {
		t.Fatalf("catalog models = %#v", cfg.ModelCatalog.Models)
	}
}

func TestApplyOAuthModelAlias_Rename(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "g5" {
		t.Fatalf("expected model id %q, got %q", "g5", out[0].ID)
	}
	if out[0].CatalogName != "gpt-5" {
		t.Fatalf("expected catalog name %q, got %q", "gpt-5", out[0].CatalogName)
	}
	if out[0].Name != "models/g5" {
		t.Fatalf("expected model name %q, got %q", "models/g5", out[0].Name)
	}
}

func TestApplyOAuthModelAlias_ForkAddsAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 2 {
		t.Fatalf("expected 2 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].CatalogName != "gpt-5" {
		t.Fatalf("expected forked catalog name %q, got %q", "gpt-5", out[1].CatalogName)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
}

func TestApplyExcludedModelsUsesCatalogNameForAlias(t *testing.T) {
	models := []*ModelInfo{
		{ID: "py-gpt", CatalogName: "gpt-5.2"},
		{ID: "gpt-5.3"},
	}

	out := applyExcludedModels(models, []string{"gpt-5.2"})
	if len(out) != 1 {
		t.Fatalf("expected one model after canonical exclusion, got %d", len(out))
	}
	if out[0].ID != "gpt-5.3" {
		t.Fatalf("expected gpt-5.3 to remain, got %q", out[0].ID)
	}
}

func TestApplyExcludedModelsMatchesResourcePrefixedCatalogName(t *testing.T) {
	models := []*ModelInfo{
		{ID: "gemini-alias", CatalogName: "models/gemini-2.5-pro"},
	}

	out := applyExcludedModels(models, []string{"gemini-2.5-pro"})
	if len(out) != 0 {
		t.Fatalf("expected resource-prefixed catalog name to be excluded, got %+v", out)
	}
}

func TestApplyExcludedModelsCanFilterGeneratedAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5.2", Alias: "py-gpt", Fork: true},
			},
		},
	}
	models := []*ModelInfo{{ID: "gpt-5.2"}}

	aliased := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	out := applyExcludedModels(aliased, []string{"py-gpt"})
	if len(out) != 1 {
		t.Fatalf("expected generated alias to be filtered, got %d models", len(out))
	}
	if out[0].ID != "gpt-5.2" {
		t.Fatalf("expected canonical model to remain, got %q", out[0].ID)
	}
}

func TestApplyOAuthModelAlias_ForkAddsMultipleAliases(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
				{Name: "gpt-5", Alias: "g5-2", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 3 {
		t.Fatalf("expected 3 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
	if out[2].ID != "g5-2" {
		t.Fatalf("expected third model id %q, got %q", "g5-2", out[2].ID)
	}
	if out[2].Name != "models/g5-2" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5-2", out[2].Name)
	}
}
