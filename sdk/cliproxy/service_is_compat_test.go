package cliproxy

import (
	"strconv"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRegisterModelsForAuthPreservesIsCompat(t *testing.T) {
	for _, compat := range []bool{false, true} {
		t.Run(
			strconv.FormatBool(compat), func(t *testing.T) {
				authID := "is-compat-registration-test-" + strconv.FormatBool(compat)
				modelRegistry := registry.GetGlobalRegistry()
				modelRegistry.UnregisterClient(authID)
				defer modelRegistry.UnregisterClient(authID)
				cfg := &internalconfig.Config{
					ClaudeKey: []internalconfig.ClaudeKey{
						{
							APIKey: "compat-key",
							Models: []internalconfig.ClaudeModel{
								{
									Name: "deepseek-flash", Alias: "deepseek-alias", IsCompat: compat,
								},
							},
						},
					},
				}
				auth := &coreauth.Auth{
					ID: authID, Provider: "claude",
					Attributes: map[string]string{"api_key": "compat-key", "auth_kind": "apikey"},
				}
				(&Service{cfg: cfg}).registerModelsForAuthWithConfig(auth, cfg)
				modelInfo := modelRegistry.GetModelInfoForClient(authID, "deepseek-alias")
				if modelInfo == nil || modelInfo.IsCompat != compat {
					t.Fatalf("registered model = %+v, want IsCompat=%t", modelInfo, compat)
				}
			},
		)
	}
}

func TestConfiguredDeepSeekModelsUseExplicitCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		models func(bool) []*ModelInfo
	}{
		{
			"Claude API key", func(compat bool) []*ModelInfo {
				return buildClaudeConfigModels(
					&internalconfig.ClaudeKey{
						Models: []internalconfig.ClaudeModel{
							{
								Name: "deepseek-flash", Alias: "deepseek-latest", IsCompat: compat,
							},
						},
					},
				)
			},
		},
		{
			"OpenAI compatibility", func(compat bool) []*ModelInfo {
				return buildOpenAICompatibilityConfigModels(
					&internalconfig.OpenAICompatibility{
						Name: "DeepSeek",
						Models: []internalconfig.OpenAICompatibilityModel{
							{
								Name: "deepseek-flash", Alias: "deepseek-latest", IsCompat: compat,
							},
						},
					},
				)
			},
		},
	}
	for _, test := range tests {
		for _, compat := range []bool{false, true} {
			t.Run(
				test.name+"/"+strconv.FormatBool(compat), func(t *testing.T) {
					models := test.models(compat)
					if len(models) != 1 {
						t.Fatalf("models = %+v, want one model", models)
					}
					model := models[0]
					if model.IsCompat != compat || model.ContextLength != 1048576 ||
						model.MaxCompletionTokens != 393216 {
						t.Fatalf(
							"DeepSeek metadata = compat:%t context:%d completion:%d", model.IsCompat,
							model.ContextLength, model.MaxCompletionTokens,
						)
					}
				},
			)
		}
	}
}

func TestConfiguredModelBuildersPreserveIsCompat(t *testing.T) {
	tests := []struct {
		name   string
		models func(bool) []*ModelInfo
	}{
		{
			"claude", func(compat bool) []*ModelInfo {
				return buildClaudeConfigModels(
					&internalconfig.ClaudeKey{
						Models: []internalconfig.ClaudeModel{
							{
								Name: "claude-upstream", Alias: "claude-alias", IsCompat: compat,
							},
						},
					},
				)
			},
		},
		{
			"codex", func(compat bool) []*ModelInfo {
				return buildCodexConfigModels(
					&internalconfig.CodexKey{
						Models: []internalconfig.CodexModel{
							{
								Name: "codex-upstream", Alias: "codex-alias", IsCompat: compat,
							},
						},
					},
				)
			},
		},
		{
			"gemini", func(compat bool) []*ModelInfo {
				return buildGeminiConfigModels(
					&internalconfig.GeminiKey{
						Models: []internalconfig.GeminiModel{
							{
								Name: "gemini-upstream", Alias: "gemini-alias", IsCompat: compat,
							},
						},
					},
				)
			},
		},
		{
			"openai compatibility", func(compat bool) []*ModelInfo {
				return buildOpenAICompatibilityConfigModels(
					&internalconfig.OpenAICompatibility{
						Name: "compat",
						Models: []internalconfig.OpenAICompatibilityModel{
							{
								Name: "compat-upstream", Alias: "compat-alias", IsCompat: compat,
							},
						},
					},
				)
			},
		},
	}
	for _, test := range tests {
		for _, compat := range []bool{false, true} {
			t.Run(
				test.name+"/"+strconv.FormatBool(compat), func(t *testing.T) {
					models := test.models(compat)
					if len(models) != 1 || models[0].IsCompat != compat {
						t.Fatalf("configured models = %+v, want IsCompat=%t", models, compat)
					}
					registryModel := toRegistryModelInfo(models[0])
					if registryModel == nil || registryModel.IsCompat != compat {
						t.Fatalf("registry model = %+v, want IsCompat=%t", registryModel, compat)
					}
					if roundTrip := fromRegistryModelInfo(registryModel); roundTrip == nil ||
						roundTrip.IsCompat != compat {
						t.Fatalf("round-trip model = %+v, want IsCompat=%t", roundTrip, compat)
					}
				},
			)
		}
	}
}
