package cliproxy

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRegisterModelsForAuthPreservesIsCompat(t *testing.T) {
	const authID = "is-compat-registration-test"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	defer modelRegistry.UnregisterClient(authID)

	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey: "compat-key",
				Models: []internalconfig.ClaudeModel{
					{
						Name: "deepseek-upstream", Alias: "deepseek-alias", IsCompat: true,
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":   "compat-key",
			"auth_kind": "apikey",
		},
	}

	(&Service{cfg: cfg}).registerModelsForAuthWithConfig(auth, cfg)
	modelInfo := modelRegistry.GetModelInfoForClient(authID, "deepseek-alias")
	if modelInfo == nil || !modelInfo.IsCompat {
		t.Fatalf("registered model = %+v, want IsCompat=true", modelInfo)
	}
}

func TestConfiguredDeepSeekModelsInheritStaticCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		models func() []*ModelInfo
	}{
		{
			name: "Claude API key",
			models: func() []*ModelInfo {
				return buildClaudeConfigModels(
					&internalconfig.ClaudeKey{
						Models: []internalconfig.ClaudeModel{{Name: "deepseek-flash", Alias: "deepseek-latest"}},
					},
				)
			},
		},
		{
			name: "OpenAI compatibility",
			models: func() []*ModelInfo {
				return buildOpenAICompatibilityConfigModels(
					&internalconfig.OpenAICompatibility{
						Name: "DeepSeek",
						Models: []internalconfig.OpenAICompatibilityModel{
							{Name: "deepseek-flash", Alias: "deepseek-latest"},
						},
					},
				)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				models := testCase.models()
				if len(models) != 1 {
					t.Fatalf("models = %+v, want one model", models)
				}
				model := models[0]
				if !model.IsCompat || model.ContextLength != 1048576 || model.MaxCompletionTokens != 393216 {
					t.Fatalf(
						"DeepSeek metadata = compat:%t context:%d completion:%d",
						model.IsCompat,
						model.ContextLength,
						model.MaxCompletionTokens,
					)
				}
			},
		)
	}
}

func TestConfiguredModelBuildersPreserveIsCompat(t *testing.T) {
	tests := []struct {
		name   string
		models func() []*ModelInfo
	}{
		{
			name: "claude",
			models: func() []*ModelInfo {
				return buildClaudeConfigModels(
					&internalconfig.ClaudeKey{
						Models: []internalconfig.ClaudeModel{
							{
								Name: "claude-upstream", Alias: "claude-alias", IsCompat: true,
							},
						},
					},
				)
			},
		},
		{
			name: "codex",
			models: func() []*ModelInfo {
				return buildCodexConfigModels(
					&internalconfig.CodexKey{
						Models: []internalconfig.CodexModel{
							{
								Name: "codex-upstream", Alias: "codex-alias", IsCompat: true,
							},
						},
					},
				)
			},
		},
		{
			name: "gemini",
			models: func() []*ModelInfo {
				return buildGeminiConfigModels(
					&internalconfig.GeminiKey{
						Models: []internalconfig.GeminiModel{
							{
								Name: "gemini-upstream", Alias: "gemini-alias", IsCompat: true,
							},
						},
					},
				)
			},
		},
		{
			name: "openai compatibility",
			models: func() []*ModelInfo {
				return buildOpenAICompatibilityConfigModels(
					&internalconfig.OpenAICompatibility{
						Name: "compat",
						Models: []internalconfig.OpenAICompatibilityModel{
							{
								Name: "compat-upstream", Alias: "compat-alias", IsCompat: true,
							},
						},
					},
				)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				models := testCase.models()
				if len(models) != 1 || !models[0].IsCompat {
					t.Fatalf("configured models = %+v, want one IsCompat model", models)
				}
				registryModel := toRegistryModelInfo(models[0])
				if registryModel == nil || !registryModel.IsCompat {
					t.Fatalf("registry model = %+v, want IsCompat=true", registryModel)
				}
				if roundTrip := fromRegistryModelInfo(registryModel); roundTrip == nil || !roundTrip.IsCompat {
					t.Fatalf("round-trip model = %+v, want IsCompat=true", roundTrip)
				}
			},
		)
	}
}
