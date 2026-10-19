package cliproxy

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestBatchModelReprojectionUsesCurrentCredential(t *testing.T) {
	models := registry.GetCodexProModels()
	if len(models) == 0 {
		t.Fatal("Codex Pro model catalog is empty")
	}
	modelID := models[0].ID
	for _, entry := range []string{"plugin model sync", "model catalog update"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			cfg := &config.Config{}
			initial := &coreauth.Auth{
				ID: "stale-batch-" + entry, Provider: "codex", Status: coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "pro"},
			}
			service := newModelRefreshTestService(t, cfg, initial)
			if entry == "plugin model sync" {
				service.pluginHost = pluginhost.New()
			}
			service.registerModelsForAuth(initial)
			if !GlobalModelRegistry().ClientSupportsModel(initial.ID, modelID) {
				t.Fatalf("initial model %s was not registered", modelID)
			}
			updated := initial.Clone()
			updated.Attributes["excluded_models"] = modelID
			if _, err := service.coreManager.Update(ctx, updated); err != nil {
				t.Fatal(err)
			}
			switch entry {
			case "plugin model sync":
				service.syncPluginModelRuntime(ctx)
			case "model catalog update":
				if err := service.applyModelCatalogRuntimeUpdate(cfg); err != nil {
					t.Fatal(err)
				}
			}
			if GlobalModelRegistry().ClientSupportsModel(initial.ID, modelID) {
				t.Fatalf("batch registration restored excluded model %s", modelID)
			}
		})
	}
}
