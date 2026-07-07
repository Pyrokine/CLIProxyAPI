package modelcatalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestLoadOrInitMigratesLegacyPricesAliasesAndEnabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(
		filepath.Join(dir, "model-prices.json"), []byte(`{
  "gpt-5.2": {"prompt": 9, "completion": 19, "cache": 0.9},
  "legacy-extra": {"prompt": 1, "completion": 2, "cache": 0.1}
}`), 0o600,
	); err != nil {
		t.Fatalf("write legacy prices: %v", err)
	}
	disabled := false
	cfg := &config.Config{}
	cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{
		"codex": {{Name: "gpt-image-2", Alias: "gpt-image"}},
	}
	cfg.ModelCatalog.Models = []config.ModelCatalogModel{{Name: "gpt-5.2", Enabled: &disabled, Group: "GPT 5"}}
	doc, err := LoadOrInit(configPath, cfg)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	gpt52, ok := doc.Find("gpt-5.2")
	if !ok {
		t.Fatal("gpt-5.2 missing")
	}
	if gpt52.Enabled {
		t.Fatal("gpt-5.2 enabled migrated true, want false")
	}
	if gpt52.Price == nil || gpt52.Price.Prompt != 9 || gpt52.Price.Completion != 19 || gpt52.Price.Cache != 0.9 {
		t.Fatalf("gpt-5.2 price = %#v", gpt52.Price)
	}
	image, ok := doc.Find("gpt-image-2")
	if !ok {
		t.Fatal("gpt-image-2 missing")
	}
	if len(image.Aliases) != 1 || image.Aliases[0] != "gpt-image" {
		t.Fatalf("gpt-image-2 aliases = %#v", image.Aliases)
	}
	legacy, ok := doc.Find("legacy-extra")
	if !ok {
		t.Fatal("legacy-extra missing")
	}
	if !legacy.UserCreated {
		t.Fatal("legacy-extra should be user_created")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("models.json not written: %v", err)
	}
}

func TestLoadOrInitMigratesLegacyAliasIntoExistingCatalog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	doc := File{Models: []Model{{Name: "gpt-5", Provider: "GPT", Channel: "codex", Enabled: true}}}
	if err := Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadOrInit(
		configPath, &config.Config{
			OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"codex": {{Name: "gpt-5", Alias: "g5"}},
			},
		},
	)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	model, ok := loaded.Find("gpt-5")
	if !ok {
		t.Fatal("gpt-5 missing")
	}
	if len(model.Aliases) != 1 || model.Aliases[0] != "g5" {
		t.Fatalf("gpt-5 aliases = %#v", model.Aliases)
	}
	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	model, ok = reloaded.Find("gpt-5")
	if !ok || len(model.Aliases) != 1 || model.Aliases[0] != "g5" {
		t.Fatalf("reloaded gpt-5 = %#v", model)
	}
}

func TestToOAuthModelAliasExpandsGeminiOAuthChannelsAndForks(t *testing.T) {
	t.Parallel()
	doc := File{
		Models: []Model{
			{
				Name:     "gemini-3-pro-preview",
				Provider: "Gemini",
				Channel:  "gemini",
				Aliases:  []string{"py-gemini"},
			},
		},
	}
	aliases := doc.ToOAuthModelAlias()
	for _, channel := range []string{"gemini-cli", "vertex", "aistudio", "antigravity"} {
		entries := aliases[channel]
		if len(entries) != 1 {
			t.Fatalf("aliases[%s] = %#v, want one entry", channel, entries)
		}
		if entries[0].Name != "gemini-3-pro-preview" || entries[0].Alias != "py-gemini" || !entries[0].Fork {
			t.Fatalf("aliases[%s][0] = %#v", channel, entries[0])
		}
	}
	if _, exists := aliases["gemini"]; exists {
		t.Fatalf("gemini API-key channel should not receive OAuth aliases: %#v", aliases["gemini"])
	}
}

func TestRemoveDefaultModelRecordsTombstoneAndPreviewCanRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	doc := DefaultFile()
	if !doc.RemoveModel("gpt-5.2", DefaultFile()) {
		t.Fatal("expected gpt-5.2 to be removed")
	}
	if err := Save(configPath, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate: %v", err)
	}
	found := false
	for _, change := range preview.Changes {
		if change.Name == "gpt-5.2" && change.Type == "restore_removed_default" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("restore change for gpt-5.2 not found: %#v", preview.Changes)
	}
	result, err := ApplyDefaultUpdate(configPath, []ApplyDecision{{Name: "gpt-5.2", Action: "restore"}})
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate: %v", err)
	}
	if result.Applied != 1 || result.BackupPath == "" {
		t.Fatalf("ApplyDefaultUpdate result = %#v", result)
	}
	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reloaded.Find("gpt-5.2"); !ok {
		t.Fatal("gpt-5.2 was not restored")
	}
	for _, name := range reloaded.RemovedDefaultModels {
		if name == "gpt-5.2" {
			t.Fatal("gpt-5.2 tombstone still present after restore")
		}
	}
}

func TestApplyOAuthModelAliasPersistsForceMappedAliases(t *testing.T) {
	t.Parallel()

	doc := File{}
	changed := ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5.4", Alias: "gpt-5.4-fast", Fork: true, ForceMapping: true}},
		},
	)
	if !changed {
		t.Fatal("ApplyOAuthModelAlias changed = false, want true")
	}
	model, ok := doc.Find("gpt-5.4")
	if !ok {
		t.Fatal("gpt-5.4 missing")
	}
	if len(model.Aliases) != 1 || model.Aliases[0] != "gpt-5.4-fast" {
		t.Fatalf("aliases = %#v", model.Aliases)
	}
	if len(model.ForceMappedAliases) != 1 || model.ForceMappedAliases[0] != "gpt-5.4-fast" {
		t.Fatalf("force mapped aliases = %#v", model.ForceMappedAliases)
	}

	aliases := doc.ToOAuthModelAlias()["codex"]
	if len(aliases) != 1 || !aliases[0].ForceMapping {
		t.Fatalf("oauth aliases = %#v", aliases)
	}
}

func TestDeleteOAuthModelAliasChannelClearsForceMappedAliases(t *testing.T) {
	t.Parallel()

	doc := File{}
	ApplyOAuthModelAlias(
		&doc,
		map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-5.4", Alias: "gpt-5.4-fast", Fork: true, ForceMapping: true}},
		},
	)
	if !DeleteOAuthModelAliasChannel(&doc, "codex") {
		t.Fatal("DeleteOAuthModelAliasChannel changed = false, want true")
	}
	model, ok := doc.Find("gpt-5.4")
	if !ok {
		t.Fatal("gpt-5.4 missing")
	}
	if len(model.Aliases) != 0 || len(model.ForceMappedAliases) != 0 {
		t.Fatalf("aliases = %#v, force mapped aliases = %#v", model.Aliases, model.ForceMappedAliases)
	}
}
