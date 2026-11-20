package cliproxy

import (
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestServiceGemini31FlashLitePreviewRegistration(t *testing.T) {
	const previewID = "gemini-3.1-flash-lite-preview"
	catalog := registry.GetGlobalRegistry()
	for _, test := range []struct{ provider, defaultModel string }{
		{"gemini", "gemini-3.1-flash-lite"},
		{"gemini-cli", previewID},
		{"aistudio", "gemini-3.1-flash-lite"},
		{"gemini-interactions", "gemini-3.1-flash-lite"},
	} {
		t.Run(
			test.provider, func(t *testing.T) {
				provider, modelID := test.provider, test.defaultModel
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"auth_kind": "oauth"},
				}
				catalog.UnregisterClient(auth.ID)
				t.Cleanup(func() { catalog.UnregisterClient(auth.ID) })
				cfg := &config.Config{}
				svc := &Service{cfg: cfg}
				svc.registerModelsForAuthWithConfig(auth, cfg)
				model := catalog.GetModelInfoForClient(auth.ID, modelID)
				if model == nil || !catalog.ClientSupportsModel(auth.ID, modelID) {
					t.Fatal("credential did not publish the channel's default model")
				}
				want := registry.LookupStaticModelInfoByChannel(modelID, provider)
				if !reflect.DeepEqual(model, want) {
					t.Fatalf("registered metadata = %+v, want channel metadata %+v", model, want)
				}
				if legacy := registry.LookupStaticModelInfoByChannel(previewID, provider); legacy == nil {
					t.Fatal("historical exact preview metadata is unavailable")
				}

				cfg.OAuthExcludedModels = map[string][]string{provider: {modelID}}
				svc.registerModelsForAuthWithConfig(auth, cfg)
				if catalog.ClientSupportsModel(auth.ID, modelID) {
					t.Fatal("global exclusion retained the model")
				}
				auth.Attributes["excluded_models"] = "gemini-2.5-pro"
				svc.registerModelsForAuthWithConfig(auth, cfg)
				if !catalog.ClientSupportsModel(auth.ID, modelID) {
					t.Fatal("account exclusion override did not restore the default model")
				}
				if modelID != previewID && catalog.ClientSupportsModel(auth.ID, previewID) {
					t.Fatal("account exclusion override published the retired Developer API preview")
				}
				auth.Attributes["excluded_models"] = modelID
				cfg.OAuthExcludedModels = nil
				svc.registerModelsForAuthWithConfig(auth, cfg)
				if catalog.ClientSupportsModel(auth.ID, modelID) {
					t.Fatal("account exclusion retained the model")
				}
			},
		)
	}
}

func TestServiceGemini31FlashLitePreviewExplicitModels(t *testing.T) {
	const modelID = "gemini-3.1-flash-lite-preview"
	for _, test := range []struct{ provider, alias string }{
		{"gemini", "configured-preview"}, {"gemini", ""},
		{"gemini-interactions", "configured-preview"}, {"gemini-interactions", ""},
	} {
		t.Run(
			test.provider+"/"+test.alias, func(t *testing.T) {
				provider, publicID := test.provider, test.alias
				if publicID == "" {
					publicID = modelID
				}
				entry := config.GeminiKey{
					APIKey: "synthetic-gemini-preview-key",
					Models: []config.GeminiModel{{Name: modelID, Alias: test.alias}},
				}
				cfg := &config.Config{GeminiKey: []config.GeminiKey{entry}, InteractionsKey: []config.GeminiKey{entry}}
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"auth_kind": "apikey", "api_key": "synthetic-gemini-preview-key"},
				}
				catalog := registry.GetGlobalRegistry()
				catalog.UnregisterClient(auth.ID)
				t.Cleanup(func() { catalog.UnregisterClient(auth.ID) })
				svc := &Service{cfg: cfg}
				svc.registerModelsForAuthWithConfig(auth, cfg)
				if publicID != modelID && catalog.ClientSupportsModel(auth.ID, modelID) ||
					catalog.ClientSupportsModel(auth.ID, "gemini-3.1-flash-lite") {
					t.Fatal("explicit model list also published default or unaliased catalog IDs")
				}
				model := catalog.GetModelInfoForClient(auth.ID, publicID)
				if model == nil || model.CatalogName != modelID || !model.UserDefined || model.Thinking == nil {
					t.Fatalf("configured preview metadata = %+v", model)
				}
				if model.NativeCapabilities == nil || model.NativeCapabilities.WebSearch == nil || !*model.NativeCapabilities.WebSearch {
					t.Fatalf("configured preview native capabilities = %+v", model.NativeCapabilities)
				}
				want := registry.LookupStaticModelInfoByChannel(modelID, provider)
				if want == nil || !reflect.DeepEqual(model.Thinking, want.Thinking) ||
					!reflect.DeepEqual(model.Thinking.Levels, []string{"minimal", "high"}) {
					t.Fatalf("configured preview thinking = %+v, want channel history %+v", model.Thinking, want)
				}
			},
		)
	}
}
