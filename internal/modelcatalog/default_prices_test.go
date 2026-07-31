package modelcatalog

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDefaultPricesMatchCurrentPublishedRates(t *testing.T) {
	t.Parallel()

	want := map[string]Price{
		"gpt-5.6-sol":      {Prompt: 5, Completion: 30, Cache: 0.5},
		"gpt-5.6-terra":    {Prompt: 2.5, Completion: 15, Cache: 0.25},
		"gpt-5.6-luna":     {Prompt: 1, Completion: 6, Cache: 0.1},
		"claude-sonnet-5":  {Prompt: 2, Completion: 10, Cache: 0.2},
		"claude-fable-5":   {Prompt: 10, Completion: 50, Cache: 1},
		"deepseek-v4-pro":  {Prompt: 0.435, Completion: 0.87, Cache: 0.003625},
		"grok-4.5":         {Prompt: 2, Completion: 6, Cache: 0.3},
		"kimi-k2.7-code":   {Prompt: 0.95, Completion: 4, Cache: 0.19},
		"gemini-3.5-flash": {Prompt: 1.5, Completion: 9, Cache: 0.15},
	}
	prices := DefaultPrices()
	for name, expected := range want {
		if got, ok := prices[name]; !ok || got != expected {
			t.Errorf("DefaultPrices()[%q] = %+v, want %+v", name, got, expected)
		}
	}
	if DefaultsVersion != "2026-07-31" {
		t.Fatalf("DefaultsVersion = %q, want 2026-07-31", DefaultsVersion)
	}
}

func TestDefaultCatalogModelsHavePricesOrExplicitExemption(t *testing.T) {
	t.Parallel()

	exemptions := map[string]string{
		"grok-imagine-image":             "media output is priced per image",
		"grok-imagine-image-quality":     "media output is priced per image",
		"grok-imagine-video":             "media output is priced per second",
		"grok-imagine-video-1.5-preview": "media output is priced per second",
	}
	catalog := DefaultFile()
	missing := make([]string, 0)
	for _, model := range catalog.Models {
		if model.Price != nil {
			continue
		}
		if _, ok := exemptions[model.Name]; ok {
			continue
		}
		missing = append(missing, model.Name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("default models without prices or exemptions: %v", missing)
	}
	for name, reason := range exemptions {
		if reason == "" {
			t.Errorf("price exemption %q has no reason", name)
		}
		if _, ok := catalog.Find(name); !ok {
			t.Errorf("price exemption %q does not match a default model", name)
		}
	}
}

func TestDefaultPriceUpdateFromJuneCatalog(t *testing.T) {
	t.Parallel()

	const previousDefaultsVersion = "2026-06-23"
	newModels := map[string]struct{}{
		"gpt-5.6-sol":     {},
		"gpt-5.6-terra":   {},
		"gpt-5.6-luna":    {},
		"claude-sonnet-5": {},
		"deepseek-v4-pro": {},
		"grok-4.5":        {},
		"kimi-k2.7-code":  {},
	}
	current := DefaultFile()
	current.DefaultsVersion = previousDefaultsVersion
	models := current.Models[:0]
	for _, model := range current.Models {
		if _, remove := newModels[model.Name]; remove {
			continue
		}
		models = append(models, model)
	}
	current.Models = models
	fable, ok := current.Find("claude-fable-5")
	if !ok {
		t.Fatal("claude-fable-5 missing from default catalog")
	}
	fable.Enabled = false
	fable.Price = &Price{Prompt: 5, Completion: 25, Cache: 0.5}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, current); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate() error = %v", err)
	}
	if preview.CurrentDefaultsVersion != previousDefaultsVersion || preview.LatestDefaultsVersion != DefaultsVersion {
		t.Fatalf("preview versions = (%q, %q)", preview.CurrentDefaultsVersion, preview.LatestDefaultsVersion)
	}
	changes := make(map[string]DefaultUpdateChange, len(preview.Changes))
	for _, change := range preview.Changes {
		changes[change.Name] = change
	}
	if len(changes) != 8 {
		t.Fatalf("default update changes = %v, want eight price changes", sortedChangeNames(changes))
	}
	for name := range newModels {
		if change := changes[name]; change.Type != "new_default" {
			t.Errorf("change %q = %#v, want new_default", name, change)
		}
	}
	fableChange := changes["claude-fable-5"]
	if fableChange.Type != "changed_default" || len(fableChange.Fields) != 1 || fableChange.Fields["price"].Default == nil {
		t.Fatalf("claude-fable-5 change = %#v, want price-only changed_default", fableChange)
	}

	decisions := make([]ApplyDecision, 0, len(changes))
	for name := range changes {
		decisions = append(decisions, ApplyDecision{Name: name, Action: "use_default"})
	}
	result, err := ApplyDefaultUpdate(configPath, decisions)
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate() error = %v", err)
	}
	if result.Applied != 8 || result.BackupPath == "" {
		t.Fatalf("ApplyDefaultUpdate() = %#v", result)
	}
	updated, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() updated catalog error = %v", err)
	}
	if updated.DefaultsVersion != DefaultsVersion || updated.SchemaVersion != SchemaVersion {
		t.Fatalf("updated versions = (%d, %q)", updated.SchemaVersion, updated.DefaultsVersion)
	}
	updatedFable, ok := updated.Find("claude-fable-5")
	if !ok || updatedFable.Enabled || updatedFable.Price == nil || *updatedFable.Price != DefaultPrices()["claude-fable-5"] {
		t.Fatalf("updated claude-fable-5 = %#v", updatedFable)
	}
	for name := range newModels {
		model, exists := updated.Find(name)
		if !exists || model.UserCreated || model.Price == nil {
			t.Errorf("updated model %q = %#v", name, model)
		}
	}
	backupData, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("read backup error = %v", err)
	}
	backup, err := decodeFile(backupData)
	if err != nil {
		t.Fatalf("decode backup error = %v", err)
	}
	if backup.DefaultsVersion != previousDefaultsVersion {
		t.Fatalf("backup defaults version = %q", backup.DefaultsVersion)
	}
	backupFable, ok := backup.Find("claude-fable-5")
	if !ok || backupFable.Price == nil || *backupFable.Price != (Price{Prompt: 5, Completion: 25, Cache: 0.5}) {
		t.Fatalf("backup claude-fable-5 = %#v", backupFable)
	}
}

func sortedChangeNames(changes map[string]DefaultUpdateChange) []string {
	names := make([]string, 0, len(changes))
	for name := range changes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
