package cliproxy

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestBuildCodexConfigModels_PreservesStaticMetadata(t *testing.T) {
	models := buildCodexConfigModels(
		&internalconfig.CodexKey{
			Models: []internalconfig.CodexModel{
				{
					Name: "gpt-5.5", Alias: "g55",
				},
			},
		},
	)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "g55" {
		t.Fatalf("expected alias g55, got %q", models[0].ID)
	}
	if upstream := registry.LookupStaticModelInfo("gpt-5.5"); upstream == nil || models[0].ContextLength != upstream.ContextLength {
		t.Fatalf("expected static context_length, got %d", models[0].ContextLength)
	}
	if models[0].MaxCompletionTokens == 0 {
		t.Fatal("expected max_completion_tokens to be preserved")
	}
	if models[0].Thinking == nil || len(models[0].Thinking.Levels) == 0 {
		t.Fatalf("expected thinking metadata, got %+v", models[0].Thinking)
	}
}

func TestBuildConfigModels_PreservesSupportedParameters(t *testing.T) {
	models := buildConfigModels([]internalconfig.CodexModel{{Name: "gpt-5.5", Alias: "g55"}}, "openai", "openai")
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	upstream := registry.LookupStaticModelInfo("gpt-5.5")
	if upstream == nil {
		t.Fatal("expected static metadata for gpt-5.5")
	}
	if len(models[0].SupportedParameters) != len(upstream.SupportedParameters) {
		t.Fatalf(
			"expected supported parameters to match upstream, got %v want %v", models[0].SupportedParameters,
			upstream.SupportedParameters,
		)
	}
}

func TestCatalogEnabledModelsRejectsConfigAliasWhenUpstreamDisabled(t *testing.T) {
	enabled := false
	cfg := &internalconfig.Config{}
	cfg.ModelCatalog.Models = []internalconfig.ModelCatalogModel{{Name: "gpt-5.5", Enabled: &enabled}}
	internalconfig.SetRuntimeModelCatalog(cfg.ModelCatalog.Models)
	svc := &Service{cfg: cfg}

	codexModels := buildConfigModels([]internalconfig.CodexModel{{Name: "gpt-5.5", Alias: "g55"}}, "openai", "openai")
	if len(codexModels) != 1 || codexModels[0].CatalogName != "gpt-5.5" {
		t.Fatalf("expected codex config model to retain catalog name, got %+v", codexModels)
	}
	if filtered := svc.catalogEnabledModels(codexModels); len(filtered) != 0 {
		t.Fatalf("expected disabled upstream to filter codex alias, got %+v", filtered)
	}

	compatModels := buildOpenAICompatibilityConfigModels(
		&internalconfig.OpenAICompatibility{
			Name:   "compat",
			Models: []internalconfig.OpenAICompatibilityModel{{Name: "gpt-5.5", Alias: "g55"}},
		},
	)
	if len(compatModels) != 1 || compatModels[0].CatalogName != "gpt-5.5" {
		t.Fatalf("expected compat config model to retain catalog name, got %+v", compatModels)
	}
	if filtered := svc.catalogEnabledModels(compatModels); len(filtered) != 0 {
		t.Fatalf("expected disabled upstream to filter compat alias, got %+v", filtered)
	}

	pluginModels := []*ModelInfo{{ID: "plugin-alias", Name: "gpt-5.5"}}
	if filtered := svc.catalogEnabledModels(pluginModels); len(filtered) != 0 {
		t.Fatalf("expected disabled upstream to filter plugin alias, got %+v", filtered)
	}
}
