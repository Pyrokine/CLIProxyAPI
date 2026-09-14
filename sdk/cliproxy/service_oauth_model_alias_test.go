package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	internalapi "github.com/Pyrokine/CLIProxyAPI/v6/internal/api"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestApplyModelCatalogConfigMigratesConfigAliasIntoNewCatalog(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
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
	if len(codexAliases) != 1 || codexAliases[0].Name != "gpt-5" || codexAliases[0].Alias != "g5" || codexAliases[0].Fork {
		t.Fatalf("codex aliases = %#v", codexAliases)
	}
	found := false
	for _, model := range cfg.ModelCatalog.Models {
		if model.Name == "gpt-5" {
			if len(model.Aliases) != 0 {
				t.Fatalf("catalog model aliases = %#v, want channel aliases kept separate", model.Aliases)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("catalog models = %#v", cfg.ModelCatalog.Models)
	}
}

func TestApplyModelCatalogConfigKeepsExistingCatalogAuthoritative(t *testing.T) {
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
			"codex": {{Name: "gpt-5", Alias: "stale-alias"}},
		},
	}
	svc := &Service{configPath: configPath}

	aliases, err := svc.applyModelCatalogConfig(cfg)
	if err != nil {
		t.Fatalf("applyModelCatalogConfig: %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("aliases = %#v, want existing catalog aliases", aliases)
	}
	if len(cfg.ModelCatalog.Models) != 1 || len(cfg.ModelCatalog.Models[0].Aliases) != 0 {
		t.Fatalf("catalog models = %#v", cfg.ModelCatalog.Models)
	}
}

func TestApplyManagementConfigCommitDoesNotMutatePublishedConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := modelcatalog.File{}
	modelcatalog.ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5", Alias: "stored-alias"}},
		},
	)
	if err := modelcatalog.Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	published := &config.Config{
		Debug: true,
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5", Alias: "published-alias"}},
		},
	}
	svc := &Service{configPath: configPath, cfg: &config.Config{}}
	if err := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{Config: published, Generation: 1},
	); err != nil {
		t.Fatalf("applyManagementConfigCommit: %v", err)
	}
	if len(published.ModelCatalog.Models) != 0 {
		t.Fatalf("published model catalog was mutated: %#v", published.ModelCatalog.Models)
	}
	publishedAliases := published.OAuthModelAlias["codex"]
	if len(publishedAliases) != 1 || publishedAliases[0].Alias != "published-alias" {
		t.Fatalf("published aliases were mutated: %#v", publishedAliases)
	}
	svc.cfgMu.RLock()
	runtimeAliases := svc.cfg.OAuthModelAlias["codex"]
	svc.cfgMu.RUnlock()
	if len(runtimeAliases) != 1 || runtimeAliases[0].Alias != "stored-alias" {
		t.Fatalf("runtime aliases = %#v", runtimeAliases)
	}
}

func TestApplyManagementConfigCommitUsesPreparedModelCatalog(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := modelcatalog.Save(
		configPath, modelcatalog.File{
			Models: []modelcatalog.Model{
				{
					Name: "disk-model", Provider: "Test", Group: "Test", Enabled: true,
				},
			},
		},
	); err != nil {
		t.Fatalf("Save disk catalog: %v", err)
	}
	enabled := true
	published := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "prepared-model", Alias: "prepared-alias"}},
		},
	}
	published.ModelCatalog.Models = []config.ModelCatalogModel{
		{
			Name: "prepared-model", Provider: "test", Group: "Test", Enabled: &enabled,
		},
	}
	svc := &Service{configPath: configPath, cfg: &config.Config{}}
	t.Cleanup(func() { config.SetRuntimeModelCatalog(nil) })

	if err := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{
			Config:               published,
			Generation:           1,
			ModelCatalogPrepared: true,
		},
	); err != nil {
		t.Fatalf("applyManagementConfigCommit: %v", err)
	}
	svc.cfgMu.RLock()
	runtimeCfg := svc.cfg
	svc.cfgMu.RUnlock()
	if len(runtimeCfg.ModelCatalog.Models) != 1 || runtimeCfg.ModelCatalog.Models[0].Name != "prepared-model" {
		t.Fatalf("runtime models = %#v", runtimeCfg.ModelCatalog.Models)
	}
	runtimeAliases := runtimeCfg.OAuthModelAlias["codex"]
	if len(runtimeAliases) != 1 || runtimeAliases[0].Alias != "prepared-alias" {
		t.Fatalf("runtime aliases = %#v", runtimeAliases)
	}
	if !runtimeCfg.ModelCatalogModelEnabled("prepared-model") {
		t.Fatal("prepared model was not published to the runtime catalog")
	}

	rollback := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "rollback-model", Alias: "rollback-alias"}},
		},
	}
	rollback.ModelCatalog.Models = []config.ModelCatalogModel{
		{
			Name: "rollback-model", Provider: "test", Group: "Test", Enabled: &enabled,
		},
	}
	if err := svc.applyManagementConfigCommit(
		context.Background(),
		internalapi.ManagementConfigCommit{
			Config:               rollback,
			Generation:           2,
			ModelCatalogPrepared: true,
		},
	); err != nil {
		t.Fatalf("apply compensating commit: %v", err)
	}
	svc.cfgMu.RLock()
	runtimeCfg = svc.cfg
	svc.cfgMu.RUnlock()
	if svc.managementConfigGeneration != 2 || len(runtimeCfg.ModelCatalog.Models) != 1 ||
		runtimeCfg.ModelCatalog.Models[0].Name != "rollback-model" {
		t.Fatalf(
			"compensating runtime = generation %d, models %#v",
			svc.managementConfigGeneration,
			runtimeCfg.ModelCatalog.Models,
		)
	}
	if aliases := runtimeCfg.OAuthModelAlias["codex"]; len(aliases) != 1 || aliases[0].Alias != "rollback-alias" {
		t.Fatalf("compensating aliases = %#v", aliases)
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

func TestApplyOAuthModelAliasUsesConfiguredDisplayName(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5.5", Alias: "oauth-gpt", DisplayName: "OAuth GPT"}},
		},
	}
	models := []*ModelInfo{{ID: "gpt-5.5", DisplayName: "GPT-5.5"}}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("models length = %d, want 1", len(out))
	}
	if out[0].ID != "oauth-gpt" || out[0].DisplayName != "OAuth GPT" {
		t.Fatalf("aliased model = %+v", out[0])
	}
}

func TestApplyOAuthModelAlias_ForkAddsAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true, DisplayName: "Configured GPT Five"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5", DisplayName: "Upstream GPT Five"},
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
	if out[0].DisplayName != "Upstream GPT Five" || out[1].DisplayName != "Configured GPT Five" {
		t.Fatalf("display names = %q/%q", out[0].DisplayName, out[1].DisplayName)
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
