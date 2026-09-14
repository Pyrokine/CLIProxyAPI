package config

import "testing"

func TestAPIKeyBlocksModel(t *testing.T) {
	cfg := &SDKConfig{
		APIKeyRules: map[string]APIKeyRule{
			"sk-test":  {BlockedModels: []string{"claude-opus-*", "models/gemini-2.5-pro", "gpt-5.5"}},
			"sk-alias": {BlockedModels: []string{"gpt-5.2"}},
		},
		ModelCatalog: ModelCatalogConfig{Models: []ModelCatalogModel{{Name: "gpt-5.2", Aliases: []string{"py-gpt"}}}},
	}

	for _, model := range []string{"claude-opus-4-7", "models/gemini-2.5-pro", "gpt-5.5(8192)"} {
		if !cfg.APIKeyBlocksModel("sk-test", model) {
			t.Fatalf("expected %q to be blocked", model)
		}
	}
	if cfg.APIKeyBlocksModel("sk-test", "claude-sonnet-4-6") {
		t.Fatal("expected claude-sonnet-4-6 to be allowed")
	}
	if !cfg.APIKeyBlocksModel("sk-alias", "py-gpt") {
		t.Fatal("expected canonical block to apply to model alias")
	}
	if cfg.APIKeyBlocksModel("sk-other", "claude-opus-4-7") {
		t.Fatal("expected key without rules to be allowed")
	}
}

func TestModelCatalogModelEnabled(t *testing.T) {
	enabled := true
	disabled := false
	cfg := &SDKConfig{
		ModelCatalog: ModelCatalogConfig{
			Models: []ModelCatalogModel{
				{Name: "models/gemini-2.5-pro", Enabled: &disabled},
				{Name: "claude-sonnet-4-6", Enabled: &enabled},
			},
		},
	}

	if cfg.ModelCatalogModelEnabled("gemini-2.5-pro") {
		t.Fatal("expected resource-prefixed disabled catalog entry to block bare model name")
	}
	if cfg.ModelCatalogModelEnabled("gemini-2.5-pro(8192)") {
		t.Fatal("expected disabled catalog entry to block suffixed model name")
	}
	if !cfg.ModelCatalogModelEnabled("claude-sonnet-4-6") {
		t.Fatal("expected explicitly enabled model to remain enabled")
	}
	if !cfg.ModelCatalogModelEnabled("gpt-5.5") {
		t.Fatal("expected missing catalog entry to default to enabled")
	}

	SetRuntimeModelCatalog([]ModelCatalogModel{{Name: "gpt-5.2", Aliases: []string{"py-gpt"}, Enabled: &disabled}})
	t.Cleanup(func() { runtimeModelCatalog.Store(runtimeModelCatalogSnapshot{}) })
	if cfg.ModelCatalogModelEnabled("gpt-5.2(8192)") {
		t.Fatal("expected runtime catalog snapshot to block suffixed disabled model name")
	}
	if cfg.ModelCatalogModelEnabled("py-gpt") {
		t.Fatal("expected runtime catalog snapshot to block disabled model alias")
	}
}

func TestPruneGloballyExcludedAPIKeyRules(t *testing.T) {
	cfg := &Config{
		APIKeyRules: map[string]APIKeyRule{
			"sk-test": {
				BlockedModels: []string{"claude-opus-4-7", "models/gemini-2.5-pro", "claude-sonnet-4-6", ""},
			},
			"sk-empty": {BlockedModels: []string{"claude-opus-4-7"}},
		},
		OAuthExcludedModels: map[string][]string{
			"claude": {"claude-opus-*"},
			"gemini": {"models/gemini-2.5-pro"},
		},
	}

	cfg.PruneGloballyExcludedAPIKeyRules()

	if cfg.APIKeyBlocksModel("sk-test", "claude-opus-4-7") {
		t.Fatal("expected globally excluded wildcard model to be removed from API key rule")
	}
	if cfg.APIKeyBlocksModel("sk-test", "models/gemini-2.5-pro") {
		t.Fatal("expected globally excluded resource-prefixed model to be removed from API key rule")
	}
	if !cfg.APIKeyBlocksModel("sk-test", "claude-sonnet-4-6") {
		t.Fatal("expected non-global API key block to remain")
	}
	if _, ok := cfg.APIKeyRules["sk-empty"]; ok {
		t.Fatal("expected empty API key rule to be removed")
	}
}
