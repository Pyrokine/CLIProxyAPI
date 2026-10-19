package cliproxy

import (
	"slices"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestMetaAndKimiAIExecutorBinding(t *testing.T) {
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	for _, provider := range []string{"kimi", "kimi-ai", "meta"} {
		service.ensureExecutorsForAuth(&coreauth.Auth{ID: provider + "-auth", Provider: provider, Status: coreauth.StatusActive})
		resolved, ok := service.coreManager.Executor(provider)
		if !ok || resolved == nil || resolved.Identifier() != provider {
			t.Errorf("provider %s executor = %v, found=%t", provider, resolved, ok)
		}
	}
	if found, _ := service.coreManager.Executor("meta"); found != nil {
		if _, ok := found.(*executor.MetaExecutor); !ok {
			t.Errorf("Meta executor = %T", found)
		}
	}
}

func TestMetaAndKimiAIHomeExecutors(t *testing.T) {
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	service.registerHomeExecutors()
	for _, provider := range []string{"kimi", "kimi-ai", "meta"} {
		resolved, ok := service.coreManager.Executor(provider)
		if !ok || resolved == nil || resolved.Identifier() != provider {
			t.Errorf("baseline %s executor = %v, found=%t", provider, resolved, ok)
		}
	}
}

func TestMetaConfigModelsDoNotReplaceOAuthCatalog(t *testing.T) {
	cfg := &config.Config{MetaKey: []config.MetaKey{{
		APIKey: "config-key", BaseURL: "https://api.meta.ai/v1",
		Models: []internalconfig.CodexModel{{Name: "private-muse", Alias: "private-alias"}},
	}}}
	registry := registry.GetGlobalRegistry()
	for _, id := range []string{"meta-config-model-test", "meta-oauth-model-test"} {
		registry.UnregisterClient(id)
		id := id
		t.Cleanup(func() { registry.UnregisterClient(id) })
	}
	service := &Service{cfg: cfg}
	service.registerModelsForAuthWithConfig(&coreauth.Auth{
		ID: "meta-config-model-test", Provider: "meta", Status: coreauth.StatusActive,
		Attributes: map[string]string{"source": "config:meta[0]", "auth_kind": "apikey", "api_key": "config-key", "base_url": "https://api.meta.ai/v1", coreauth.AttributeConfigIndex: "0"},
	}, cfg)
	service.registerModelsForAuthWithConfig(&coreauth.Auth{
		ID: "meta-oauth-model-test", Provider: "meta", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth", "source": "oauth.json", "api_key": "config-key"},
	}, cfg)
	configModels := registry.GetModelsForClient("meta-config-model-test")
	if len(configModels) != 1 || configModels[0].ID != "private-alias" || configModels[0].Type != "meta" {
		t.Fatalf("config Meta models = %#v", configModels)
	}
	oauthModels := registry.GetModelsForClient("meta-oauth-model-test")
	if len(oauthModels) == 0 {
		t.Fatal("OAuth Meta catalog is empty")
	}
	for _, model := range oauthModels {
		if model.ID == "private-alias" {
			t.Fatal("config API key model leaked into OAuth Meta account")
		}
	}
}

func TestKimiAIModelsKeepProviderIdentity(t *testing.T) {
	const authID = "kimi-ai-model-test"
	cfg := &config.Config{}
	catalog := registry.GetGlobalRegistry()
	catalog.UnregisterClient(authID)
	t.Cleanup(func() { catalog.UnregisterClient(authID) })
	(&Service{cfg: cfg}).registerModelsForAuthWithConfig(&coreauth.Auth{
		ID: authID, Provider: "kimi-ai", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}, cfg)
	models := catalog.GetModelsForClient(authID)
	if len(models) == 0 {
		t.Fatal("Kimi.ai model catalog is empty")
	}
	if providers := catalog.GetModelProviders(models[0].ID); !slices.Contains(providers, "kimi-ai") {
		t.Errorf("Kimi.ai model providers = %v", providers)
	}
}
