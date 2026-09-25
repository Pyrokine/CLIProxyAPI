package cliproxy

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

func TestBuildConfiguredModelsAppliesDisplayNameAndMaxContextLength(t *testing.T) {
	const want = 262144
	tests := []struct {
		name              string
		got               func() []*ModelInfo
		wantContextLength int
	}{
		{
			name:              "codex",
			wantContextLength: want,
			got: func() []*ModelInfo {
				return buildCodexConfigModels(
					&internalconfig.CodexKey{
						Models: []internalconfig.CodexModel{
							{
								Name: "codex-upstream", Alias: "codex-alias", DisplayName: "Configured Model",
								MaxContextLength: want,
							},
						},
					},
				)
			},
		},
		{
			name:              "claude",
			wantContextLength: want,
			got: func() []*ModelInfo {
				return buildClaudeConfigModels(
					&internalconfig.ClaudeKey{
						Models: []internalconfig.ClaudeModel{
							{
								Name: "claude-upstream", Alias: "claude-alias", DisplayName: "Configured Model",
								MaxContextLength: want,
							},
						},
					},
				)
			},
		},
		{
			name:              "gemini",
			wantContextLength: want,
			got: func() []*ModelInfo {
				return buildGeminiConfigModels(
					&internalconfig.GeminiKey{
						Models: []internalconfig.GeminiModel{
							{
								Name: "gemini-upstream", Alias: "gemini-alias", DisplayName: "Configured Model",
								MaxContextLength: want,
							},
						},
					},
				)
			},
		},
		{
			name:              "interactions",
			wantContextLength: want,
			got: func() []*ModelInfo {
				return buildGeminiConfigModels(
					&internalconfig.GeminiKey{
						Models: []internalconfig.GeminiModel{
							{
								Name: "interactions-upstream", Alias: "interactions-alias",
								DisplayName: "Configured Model", MaxContextLength: want,
							},
						},
					},
				)
			},
		},
		{
			name:              "vertex",
			wantContextLength: 0,
			got: func() []*ModelInfo {
				return buildVertexCompatConfigModels(
					&internalconfig.VertexCompatKey{
						Models: []internalconfig.VertexCompatModel{
							{
								Name: "vertex-upstream", Alias: "vertex-alias", DisplayName: "Configured Model",
							},
						},
					},
				)
			},
		},
		{
			name:              "openai compatibility",
			wantContextLength: want,
			got: func() []*ModelInfo {
				return buildOpenAICompatibilityConfigModels(
					&internalconfig.OpenAICompatibility{
						Name: "compat",
						Models: []internalconfig.OpenAICompatibilityModel{
							{
								Name: "compat-upstream", Alias: "compat-alias", DisplayName: "Configured Model",
								MaxContextLength: want,
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
				models := testCase.got()
				if len(models) != 1 {
					t.Fatalf("models length = %d, want 1", len(models))
				}
				model := models[0]
				if model.DisplayName != "Configured Model" {
					t.Fatalf("DisplayName = %q, want Configured Model", model.DisplayName)
				}
				if model.ContextLength != testCase.wantContextLength || model.MaxContextLength != testCase.wantContextLength {
					t.Fatalf(
						"context lengths = %d/%d, want %d",
						model.ContextLength,
						model.MaxContextLength,
						testCase.wantContextLength,
					)
				}
			},
		)
	}
}

func TestBuildConfiguredModelsApplyThinkingOverrides(t *testing.T) {
	thinking := &registry.ThinkingSupport{
		Levels: []string{" HIGH ", "none", "auto", "high", ""},
	}
	tests := []struct {
		name string
		got  func() []*ModelInfo
	}{
		{
			name: "codex",
			got: func() []*ModelInfo {
				return buildCodexConfigModels(
					&internalconfig.CodexKey{
						Models: []internalconfig.CodexModel{
							{
								Name: "unknown-codex", Thinking: thinking,
							},
						},
					},
				)
			},
		},
		{
			name: "xai",
			got: func() []*ModelInfo {
				return buildXAIConfigModels(
					&internalconfig.XAIKey{
						Models: []internalconfig.XAIModel{
							{
								Name: "unknown-xai", Thinking: thinking,
							},
						},
					},
				)
			},
		},
		{
			name: "claude",
			got: func() []*ModelInfo {
				return buildClaudeConfigModels(
					&internalconfig.ClaudeKey{
						Models: []internalconfig.ClaudeModel{
							{
								Name: "unknown-claude", Thinking: thinking,
							},
						},
					},
				)
			},
		},
		{
			name: "gemini",
			got: func() []*ModelInfo {
				return buildGeminiConfigModels(
					&internalconfig.GeminiKey{
						Models: []internalconfig.GeminiModel{
							{
								Name: "unknown-gemini", Thinking: thinking,
							},
						},
					},
				)
			},
		},
		{
			name: "vertex",
			got: func() []*ModelInfo {
				return buildVertexCompatConfigModels(
					&internalconfig.VertexCompatKey{
						Models: []internalconfig.VertexCompatModel{
							{
								Name: "unknown-vertex", Thinking: thinking,
							},
						},
					},
				)
			},
		},
		{
			name: "openai compatibility",
			got: func() []*ModelInfo {
				return buildOpenAICompatibilityConfigModels(
					&internalconfig.OpenAICompatibility{
						Name: "compat",
						Models: []internalconfig.OpenAICompatibilityModel{
							{
								Name: "unknown-openai", Thinking: thinking,
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
				models := testCase.got()
				if len(models) != 1 || models[0].Thinking == nil {
					t.Fatalf("models = %+v, want one model with thinking metadata", models)
				}
				got := models[0].Thinking
				wantLevels := []string{"high", "none", "auto"}
				if len(got.Levels) != len(wantLevels) {
					t.Fatalf("thinking levels = %v, want %v", got.Levels, wantLevels)
				}
				for i := range wantLevels {
					if got.Levels[i] != wantLevels[i] {
						t.Fatalf("thinking levels = %v, want %v", got.Levels, wantLevels)
					}
				}
				if !got.ZeroAllowed || !got.DynamicAllowed {
					t.Fatalf("thinking flags = zero:%t dynamic:%t, want both true", got.ZeroAllowed, got.DynamicAllowed)
				}
			},
		)
	}
}

func TestBuildOpenAICompatibilityUnknownModelDoesNotInventThinkingMetadata(t *testing.T) {
	models := buildOpenAICompatibilityConfigModels(
		&internalconfig.OpenAICompatibility{
			Name: "compat",
			Models: []internalconfig.OpenAICompatibilityModel{
				{
					Name: "unknown-openai-model",
				},
			},
		},
	)
	if len(models) != 1 {
		t.Fatalf("models length = %d, want 1", len(models))
	}
	if models[0].Thinking != nil {
		t.Fatalf("unknown model thinking metadata = %+v, want nil", models[0].Thinking)
	}
}

func TestRegisterModelsForAuthRegistersXAIModels(t *testing.T) {
	const authID = "xai-model-registration-test"
	GlobalModelRegistry().UnregisterClient(authID)
	defer GlobalModelRegistry().UnregisterClient(authID)

	cfg := &internalconfig.Config{}
	svc := &Service{cfg: cfg}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	svc.registerModelsForAuthWithConfig(auth, cfg)
	if !GlobalModelRegistry().ClientSupportsModel(authID, "grok-4.6") {
		t.Fatal("expected xAI OAuth credential to register Grok 4.6")
	}
	if !GlobalModelRegistry().ClientSupportsModel(authID, "grok-4.5") {
		t.Fatal("expected xAI OAuth credential to register Grok chat models")
	}
	if !GlobalModelRegistry().ClientSupportsModel(authID, "grok-imagine-video-1.5") {
		t.Fatal("expected xAI OAuth credential to register Grok Imagine Video 1.5 GA")
	}
	if GlobalModelRegistry().ClientSupportsModel(authID, "grok-imagine-video-1.5-preview") {
		t.Fatal("default xAI OAuth catalog must not expose the preview alias")
	}
}

func TestRegisterModelsForAuthPublishesCurrentNativeModels(t *testing.T) {
	tests := []struct {
		provider string
		present  []string
		absent   []string
	}{
		{
			provider: "claude",
			present:  []string{"claude-fable-5-1", "claude-opus-5-5"},
		},
		{
			provider: "gemini",
			present: []string{
				"gemini-3.8-flash",
				"gemini-3.1-flash-image",
				"gemini-3.1-flash-lite-image",
			},
			absent: []string{"gemini-3-pro-preview", "gemini-3.1-flash-image-preview"},
		},
		{
			provider: "vertex",
			present: []string{
				"gemini-3.8-flash",
				"gemini-3.1-flash-image",
				"gemini-3.1-flash-lite-image",
			},
			absent: []string{"gemini-3-pro-preview", "gemini-3.1-flash-lite-preview"},
		},
		{
			provider: "aistudio",
			present: []string{
				"gemini-3.8-flash",
				"gemini-3.1-flash-image",
				"gemini-3.1-flash-lite-image",
			},
			absent: []string{"gemini-3-pro-preview", "gemini-3.1-flash-lite-preview"},
		},
		{
			provider: "xai",
			present:  []string{"grok-4.7", "grok-imagine-image-2.0"},
			absent:   []string{"grok-imagine-video-1.5-preview"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.provider, func(t *testing.T) {
			authID := "current-native-models-" + testCase.provider
			GlobalModelRegistry().UnregisterClient(authID)
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(authID) })

			cfg := &internalconfig.Config{}
			svc := &Service{cfg: cfg}
			svc.registerModelsForAuthWithConfig(
				&coreauth.Auth{
					ID:       authID,
					Provider: testCase.provider,
					Attributes: map[string]string{
						"auth_kind": "oauth",
					},
				},
				cfg,
			)
			for _, modelID := range testCase.present {
				if !GlobalModelRegistry().ClientSupportsModel(authID, modelID) {
					t.Errorf("%s credential did not publish current model %q", testCase.provider, modelID)
				}
			}
			for _, modelID := range testCase.absent {
				if GlobalModelRegistry().ClientSupportsModel(authID, modelID) {
					t.Errorf("%s credential published retired model %q", testCase.provider, modelID)
				}
			}
		})
	}
}

func TestC04RegisterModelsForXAIAPIKeyUsesConfiguredModels(t *testing.T) {
	const authID = "xai-api-key-model-registration-test"
	GlobalModelRegistry().UnregisterClient(authID)
	defer GlobalModelRegistry().UnregisterClient(authID)

	cfg := &internalconfig.Config{
		XAIKey: []internalconfig.XAIKey{
			{
				APIKey:  "xai-key",
				BaseURL: "https://api.x.ai/v1",
				Models: []internalconfig.XAIModel{
					{
						Name: "grok-4.5", Alias: "grok-latest", DisplayName: "Grok Latest", MaxContextLength: 262144,
					},
				},
			},
		},
	}
	svc := &Service{cfg: cfg}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":   "xai-key",
			"base_url":  "https://api.x.ai/v1",
			"auth_kind": "apikey",
		},
	}

	svc.registerModelsForAuthWithConfig(auth, cfg)
	if !GlobalModelRegistry().ClientSupportsModel(authID, "grok-latest") {
		t.Fatal("expected configured xAI alias to be registered")
	}
	if GlobalModelRegistry().ClientSupportsModel(authID, "grok-4.5") {
		t.Fatal("configured xAI model list must replace the default catalog for this credential")
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

func TestConfigResolversUseExactConfigIndex(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{APIKey: "shared-key", Models: []internalconfig.ClaudeModel{{Name: "claude-first"}}},
			{APIKey: "shared-key", Models: []internalconfig.ClaudeModel{{Name: "claude-second"}}},
		},
		GeminiKey: []internalconfig.GeminiKey{
			{APIKey: "shared-key", Models: []internalconfig.GeminiModel{{Name: "gemini-first"}}},
			{APIKey: "shared-key", Models: []internalconfig.GeminiModel{{Name: "gemini-second"}}},
		},
		InteractionsKey: []internalconfig.GeminiKey{
			{APIKey: "shared-key", Models: []internalconfig.GeminiModel{{Name: "interactions-first"}}},
			{APIKey: "shared-key", Models: []internalconfig.GeminiModel{{Name: "interactions-second"}}},
		},
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{
			{APIKey: "shared-key", Models: []internalconfig.VertexCompatModel{{Name: "vertex-first"}}},
			{APIKey: "shared-key", Models: []internalconfig.VertexCompatModel{{Name: "vertex-second"}}},
		},
		CodexKey: []internalconfig.CodexKey{
			{APIKey: "shared-key", Models: []internalconfig.CodexModel{{Name: "codex-first"}}},
			{APIKey: "shared-key", Models: []internalconfig.CodexModel{{Name: "codex-second"}}},
		},
		XAIKey: []internalconfig.XAIKey{
			{APIKey: "shared-key", Models: []internalconfig.XAIModel{{Name: "xai-first"}}},
			{APIKey: "shared-key", Models: []internalconfig.XAIModel{{Name: "xai-second"}}},
		},
	}
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			coreauth.AttributeSource:      "config:test[token]",
			coreauth.AttributeConfigIndex: "1",
			coreauth.AttributeAPIKey:      "shared-key",
		},
	}
	tests := []struct {
		name string
		got  func() string
		want string
	}{
		{
			name: "claude",
			got: func() string {
				return resolveConfigClaudeKey(cfg, auth).Models[0].Name
			},
			want: "claude-second",
		},
		{
			name: "gemini",
			got: func() string {
				return resolveConfigGeminiKey(cfg, auth).Models[0].Name
			},
			want: "gemini-second",
		},
		{
			name: "interactions",
			got: func() string {
				return resolveConfigInteractionsKey(cfg, auth).Models[0].Name
			},
			want: "interactions-second",
		},
		{
			name: "vertex",
			got: func() string {
				return resolveConfigVertexCompatKey(cfg, auth).Models[0].Name
			},
			want: "vertex-second",
		},
		{
			name: "codex",
			got: func() string {
				return resolveConfigCodexKey(cfg, auth).Models[0].Name
			},
			want: "codex-second",
		},
		{
			name: "xai",
			got: func() string {
				return resolveConfigXAIKey(cfg, auth).Models[0].Name
			},
			want: "xai-second",
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				if got := testCase.got(); got != testCase.want {
					t.Fatalf("resolved model = %q, want %q", got, testCase.want)
				}
			},
		)
	}
}

func TestResolveConfigCodexKeyUsesConfigIndexBeforeCredentialFallback(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey: "shared-key", BaseURL: "https://first.example/v1",
				Models: []internalconfig.CodexModel{{Name: "first-model"}},
			},
			{
				APIKey: "shared-key", BaseURL: "https://second.example/v1",
				Models: []internalconfig.CodexModel{{Name: "second-model"}},
			},
		},
	}

	tests := []struct {
		name       string
		attributes map[string]string
		wantIndex  int
	}{
		{
			name: "valid index",
			attributes: map[string]string{
				coreauth.AttributeSource:      "config:codex[test]",
				coreauth.AttributeConfigIndex: "1",
				coreauth.AttributeAPIKey:      "shared-key",
				"base_url":                    "https://second.example/v1/",
			},
			wantIndex: 1,
		},
		{
			name: "valid index is authoritative",
			attributes: map[string]string{
				coreauth.AttributeSource:      "config:codex[test]",
				coreauth.AttributeConfigIndex: "0",
				coreauth.AttributeAPIKey:      "shared-key",
				"base_url":                    "https://second.example/v1",
			},
			wantIndex: 0,
		},
		{
			name: "out of range index falls back by credentials",
			attributes: map[string]string{
				coreauth.AttributeSource:      "config:codex[test]",
				coreauth.AttributeConfigIndex: "9",
				coreauth.AttributeAPIKey:      "shared-key",
				"base_url":                    "https://first.example/v1",
			},
			wantIndex: 0,
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				auth := &coreauth.Auth{Attributes: testCase.attributes}
				entry := resolveConfigCodexKey(cfg, auth)
				if entry != &cfg.CodexKey[testCase.wantIndex] {
					t.Fatalf("resolved entry = %+v, want config index %d", entry, testCase.wantIndex)
				}
			},
		)
	}
}

func TestResolveConfigGeminiKeyUsesExactBaseURL(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{APIKey: "shared-key", Models: []internalconfig.GeminiModel{{Name: "default-model"}}},
			{
				APIKey:  "shared-key",
				BaseURL: "https://gateway.example/v1",
				Models:  []internalconfig.GeminiModel{{Name: "custom-model"}},
			},
		},
	}
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://gateway.example/v1",
		},
	}

	entry := resolveConfigGeminiKey(cfg, auth)
	if entry == nil || entry.BaseURL != "https://gateway.example/v1" {
		t.Fatalf("resolved Gemini key = %+v, want custom base URL", entry)
	}
}

func TestOpenAICompatibilityRegistrationUsesConfigIndex(t *testing.T) {
	const authID = "openai-compat-config-index-test"
	GlobalModelRegistry().UnregisterClient(authID)
	defer GlobalModelRegistry().UnregisterClient(authID)

	cfg := &internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name: "shared",
				Models: []internalconfig.OpenAICompatibilityModel{
					{
						Name: "first-upstream", Alias: "first-public",
					},
				},
			},
			{
				Name: "shared",
				Models: []internalconfig.OpenAICompatibilityModel{
					{
						Name: "second-upstream", Alias: "second-public",
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "shared",
		Attributes: map[string]string{
			coreauth.AttributeSource:      "config:shared[token]",
			coreauth.AttributeConfigIndex: "1",
			"compat_name":                 "shared",
			"provider_key":                "shared",
		},
	}

	(&Service{cfg: cfg}).registerModelsForAuthWithConfig(auth, cfg)
	if !GlobalModelRegistry().ClientSupportsModel(authID, "second-public") {
		t.Fatal("second config entry model was not registered")
	}
	if GlobalModelRegistry().ClientSupportsModel(authID, "first-public") {
		t.Fatal("name fallback selected the first duplicate config entry")
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
