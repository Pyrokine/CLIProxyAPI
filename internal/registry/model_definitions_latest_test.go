package registry

import (
	"strings"
	"testing"
)

func TestDeepSeekModelsUseCurrentAnthropicCompatibleIDs(t *testing.T) {
	for _, modelID := range []string{
		"deepseek-flash",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"deepseek-v4-flash-vision-exp",
	} {
		model := LookupStaticModelInfo(modelID)
		if model == nil {
			t.Errorf("missing %s", modelID)
			continue
		}
		if model.OwnedBy != "deepseek" || model.Type != "claude" || !model.IsCompat {
			t.Errorf("%s identity = %+v, want DeepSeek Claude-compatible model", modelID, model)
		}
		if model.ContextLength != 1048576 || model.MaxCompletionTokens != 393216 {
			t.Errorf(
				"%s token limits = %d/%d, want 1048576/393216",
				modelID,
				model.ContextLength,
				model.MaxCompletionTokens,
			)
		}
	}

	for _, modelID := range []string{
		"deepseek-v4.1-flash",
		"deepseek-v4.1-pro",
		"deepseek-chat",
		"deepseek-reasoner",
	} {
		if model := LookupStaticModelInfo(modelID); model != nil {
			t.Errorf("unsupported DeepSeek model %s = %+v, want nil", modelID, model)
		}
	}
}

func TestKimiModelsIncludeK27Code(t *testing.T) {
	model := LookupStaticModelInfo("kimi-k2.7-code")
	if model == nil {
		t.Fatal("missing kimi-k2.7-code")
	}
	if model.ContextLength != 262144 {
		t.Fatalf("ContextLength = %d, want 262144", model.ContextLength)
	}
	if model.MaxCompletionTokens != 65536 {
		t.Fatalf("MaxCompletionTokens = %d, want 65536", model.MaxCompletionTokens)
	}
	if model.Thinking == nil {
		t.Fatal("Thinking = nil, want Kimi K2.7 thinking support")
	}
	if model.Thinking.ZeroAllowed {
		t.Fatal("Thinking.ZeroAllowed = true, want false")
	}
}

func TestKimiModelsIncludeFinalK3Variants(t *testing.T) {
	tests := []struct {
		id             string
		contextLength  int
		thinkingLevels []string
	}{
		{id: "kimi-k2.7-code-highspeed", contextLength: 262144, thinkingLevels: []string{"low", "high"}},
		{id: "kimi-k3", contextLength: 1048576, thinkingLevels: []string{"low", "high", "max"}},
		{id: "kimi-k3-256k", contextLength: 262144, thinkingLevels: []string{"low", "high", "max"}},
	}

	for _, test := range tests {
		t.Run(
			test.id, func(t *testing.T) {
				model := findModelByID(GetKimiModels(), test.id)
				if model == nil {
					t.Fatal("model missing")
				}
				if model.ContextLength != test.contextLength {
					t.Fatalf("ContextLength = %d, want %d", model.ContextLength, test.contextLength)
				}
				if model.MaxCompletionTokens != 65536 {
					t.Fatalf("MaxCompletionTokens = %d, want 65536", model.MaxCompletionTokens)
				}
				if model.Thinking == nil {
					t.Fatal("Thinking = nil")
				}
				if len(model.Thinking.Levels) != len(test.thinkingLevels) {
					t.Fatalf("Thinking.Levels = %v, want %v", model.Thinking.Levels, test.thinkingLevels)
				}
				for index := range test.thinkingLevels {
					if model.Thinking.Levels[index] != test.thinkingLevels[index] {
						t.Fatalf("Thinking.Levels = %v, want %v", model.Thinking.Levels, test.thinkingLevels)
					}
				}
			},
		)
	}
}

func TestC05FinalModelCatalogEntries(t *testing.T) {
	tests := []struct {
		name                string
		model               *ModelInfo
		wantContext         int
		wantCompletion      int
		wantThinkingLevels  []string
		wantDynamicThinking bool
	}{
		{
			name:                "Claude Opus 5",
			model:               findModelByID(GetClaudeModels(), "claude-opus-5"),
			wantContext:         1000000,
			wantCompletion:      128000,
			wantThinkingLevels:  []string{"low", "medium", "high", "xhigh", "max"},
			wantDynamicThinking: true,
		},
		{
			name:                "Gemini 3.6 Flash",
			model:               findModelByID(GetGeminiModels(), "gemini-3.6-flash"),
			wantThinkingLevels:  []string{"minimal", "low", "medium", "high"},
			wantDynamicThinking: true,
		},
		{
			name:                "Vertex Gemini 3 Pro",
			model:               findModelByID(GetGeminiVertexModels(), "gemini-3-pro"),
			wantThinkingLevels:  []string{"low", "high"},
			wantDynamicThinking: true,
		},
		{
			name:                "Vertex Gemini 3 Flash",
			model:               findModelByID(GetGeminiVertexModels(), "gemini-3-flash"),
			wantThinkingLevels:  []string{"minimal", "low", "medium", "high"},
			wantDynamicThinking: true,
		},
		{
			name:                "Vertex Gemini 3.1 Pro",
			model:               findModelByID(GetGeminiVertexModels(), "gemini-3.1-pro"),
			wantThinkingLevels:  []string{"low", "medium", "high"},
			wantDynamicThinking: true,
		},
		{
			name:                "Vertex Gemini 3.1 Flash Lite",
			model:               findModelByID(GetGeminiVertexModels(), "gemini-3.1-flash-lite"),
			wantThinkingLevels:  []string{"minimal", "low", "medium", "high"},
			wantDynamicThinking: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if tt.model == nil {
					t.Fatal("model missing")
				}
				if tt.wantContext > 0 && tt.model.ContextLength != tt.wantContext {
					t.Fatalf("ContextLength = %d, want %d", tt.model.ContextLength, tt.wantContext)
				}
				if tt.wantCompletion > 0 && tt.model.MaxCompletionTokens != tt.wantCompletion {
					t.Fatalf("MaxCompletionTokens = %d, want %d", tt.model.MaxCompletionTokens, tt.wantCompletion)
				}
				assertThinkingLevelsEqual(t, tt.model.Thinking, tt.wantThinkingLevels)
				if tt.model.Thinking.DynamicAllowed != tt.wantDynamicThinking {
					t.Fatalf(
						"Thinking.DynamicAllowed = %v, want %v", tt.model.Thinking.DynamicAllowed,
						tt.wantDynamicThinking,
					)
				}
			},
		)
	}

	vertexFlashLite := findModelByID(GetGeminiVertexModels(), "gemini-3.1-flash-lite")
	if vertexFlashLite == nil {
		t.Fatal("Vertex catalog must expose gemini-3.1-flash-lite")
	}
	if vertexFlashLite.InputTokenLimit != 1048576 || vertexFlashLite.OutputTokenLimit != 65536 {
		t.Fatalf(
			"Vertex Gemini 3.1 Flash Lite token limits = %d/%d, want 1048576/65536",
			vertexFlashLite.InputTokenLimit,
			vertexFlashLite.OutputTokenLimit,
		)
	}
	vertexModels := GetGeminiVertexModels()
	for _, modelID := range []string{"gemini-3.1-flash-image", "gemini-3-pro-image"} {
		model := findModelByID(vertexModels, modelID)
		if model == nil {
			t.Fatalf("Vertex catalog must expose %s", modelID)
		}
		if model.InputTokenLimit != 1048576 || model.OutputTokenLimit != 65536 {
			t.Fatalf(
				"Vertex %s token limits = %d/%d, want 1048576/65536", modelID, model.InputTokenLimit,
				model.OutputTokenLimit,
			)
		}
	}
	for _, modelID := range []string{
		"gemini-3-pro-preview",
		"gemini-3-flash-preview",
		"gemini-3.1-flash-image-preview",
		"gemini-3-pro-image-preview",
		"gemini-3.1-flash-lite-preview",
	} {
		if findModelByID(vertexModels, modelID) != nil {
			t.Fatalf("Vertex catalog must not expose %s", modelID)
		}
	}
	models := getModels()
	if models == nil || findModelByID(models.Gemini, "gemini-3.1-flash-lite-preview") == nil {
		t.Fatal("embedded Gemini catalog must retain gemini-3.1-flash-lite-preview")
	}

	antigravity := GetAntigravityModelConfig()
	flash36 := antigravity["gemini-3.6-flash-high"]
	if flash36 == nil {
		t.Fatal("missing Antigravity gemini-3.6-flash-high metadata")
	}
	if flash36.MaxCompletionTokens != 65536 {
		t.Fatalf("Antigravity Gemini 3.6 MaxCompletionTokens = %d, want 65536", flash36.MaxCompletionTokens)
	}
	assertThinkingLevelsEqual(t, flash36.Thinking, []string{"minimal", "low", "medium", "high"})
	if _, ok := antigravity["gemini-3.5-flash-lite"]; ok {
		t.Fatal("gemini-3.5-flash-lite must remain deleted from Antigravity metadata")
	}
}

func TestCodexModelsIncludeOfficialGPTImage25Variants(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("codex")
	for _, modelID := range []string{
		"gpt-image-2.5-flare",
		"gpt-image-2.5-flare-2026-09-08",
		"gpt-image-2.5-sunburst",
		"gpt-image-2.5-sunburst-2026-09-08",
	} {
		model := findModelByID(models, modelID)
		if model == nil {
			t.Fatalf("missing %s", modelID)
		}
		if model.OwnedBy != "openai" || model.Type != "openai" || model.Version != modelID {
			t.Fatalf("%s identity = %+v", modelID, model)
		}
		if canonical := CanonicalCodexImageModel(strings.ToUpper(modelID)); canonical != modelID {
			t.Fatalf("CanonicalCodexImageModel(%q) = %q, want %q", strings.ToUpper(modelID), canonical, modelID)
		}
	}

	if model := LookupStaticModelInfo("gpt-image-2.5"); model != nil {
		t.Fatalf("unsupported gpt-image-2.5 alias = %+v, want nil", model)
	}
	if IsCodexImageModel("gpt-image-2.5") {
		t.Fatal("gpt-image-2.5 must not be classified as a built-in image model")
	}
}

func findModelByID(models []*ModelInfo, modelID string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return model
		}
	}
	return nil
}

func TestAntigravityStaticConfigExcludesDeprecatedGeminiProVariants(t *testing.T) {
	cfg := GetAntigravityModelConfig()
	if _, ok := cfg["gemini-3-pro-high"]; ok {
		t.Fatal("gemini-3-pro-high should not be in Antigravity static config")
	}
	if _, ok := cfg["gemini-3-pro-low"]; ok {
		t.Fatal("gemini-3-pro-low should not be in Antigravity static config")
	}
}

func TestXAIModelsSimplifyComposerThinking(t *testing.T) {
	model := LookupStaticModelInfo("grok-composer-2.5-fast")
	if model == nil {
		t.Fatal("missing grok-composer-2.5-fast")
	}
	if model.ContextLength != 200000 {
		t.Fatalf("ContextLength = %d, want 200000", model.ContextLength)
	}
	if model.Thinking != nil {
		t.Fatalf("Thinking = %#v, want nil", model.Thinking)
	}
}

func TestCodexStaticTiersIncludeGPT6AstraByPlan(t *testing.T) {
	tiers := []struct {
		name   string
		models []*ModelInfo
		want   bool
	}{
		{name: "free", models: GetCodexFreeModels(), want: false},
		{name: "plus", models: GetCodexPlusModels(), want: true},
		{name: "team", models: GetCodexTeamModels(), want: true},
		{name: "pro", models: GetCodexProModels(), want: true},
	}
	for _, tier := range tiers {
		t.Run(
			tier.name, func(t *testing.T) {
				model := findModelByID(tier.models, "gpt-6-astra")
				if (model != nil) != tier.want {
					t.Fatalf("gpt-6-astra present = %t, want %t", model != nil, tier.want)
				}
				if !tier.want {
					return
				}
				if model.DisplayName != "GPT 6.0 Astra" || model.Version != "gpt-6.0" {
					t.Fatalf("Astra identity = (%q, %q)", model.DisplayName, model.Version)
				}
				if model.ContextLength != 272000 || model.MaxCompletionTokens != 128000 {
					t.Fatalf(
						"Astra token limits = (%d, %d)",
						model.ContextLength,
						model.MaxCompletionTokens,
					)
				}
				assertStringSliceEqual(t, model.SupportedParameters, []string{"tools"})
				assertThinkingLevelsEqual(t, model.Thinking, []string{"low", "medium", "high", "xhigh", "max"})
				assertStringSliceEqual(t, model.SupportedInputModalities, []string{"text", "image"})
				assertStringSliceEqual(t, model.SupportedOutputModalities, []string{"text"})
				if model.Config == nil ||
					model.Config.OverrideHeader["user-agent"] != "codex-tui/0.153.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.153.0)" ||
					model.Config.OverrideHeader["originator"] != "codex-tui" ||
					model.Config.OverrideHeader["version"] != "0.153.0" {
					t.Fatalf("Astra header override = %#v", model.Config)
				}
			},
		)
	}
}

func TestOpenAIModelsIncludeGPT56CodexTiers(t *testing.T) {
	tests := []struct {
		id     string
		levels []string
	}{
		{id: "gpt-5.6-sol", levels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "gpt-5.6-terra", levels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "gpt-5.6-luna", levels: []string{"low", "medium", "high", "xhigh", "max"}},
	}

	for _, tt := range tests {
		t.Run(
			tt.id, func(t *testing.T) {
				model := LookupStaticModelInfo(tt.id)
				if model == nil {
					t.Fatalf("missing %s", tt.id)
				}
				if model.ContextLength != 372000 {
					t.Fatalf("ContextLength = %d, want 372000", model.ContextLength)
				}
				if model.MaxCompletionTokens != 128000 {
					t.Fatalf("MaxCompletionTokens = %d, want 128000", model.MaxCompletionTokens)
				}
				assertStringSliceEqual(t, model.SupportedParameters, []string{"tools"})
				assertThinkingLevelsEqual(t, model.Thinking, tt.levels)
			},
		)
	}
}

func TestModelOverrideHeadersFromEmbeddedModels(t *testing.T) {
	const wantUA = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	GetGlobalRegistry().RegisterClient("test-model-override-shadow", "codex", []*ModelInfo{{ID: "gpt-5.6-luna"}})
	defer GetGlobalRegistry().UnregisterClient("test-model-override-shadow")

	assertGPT56ModelOverrideHeaders(t, ModelOverrideHeaders("gpt-5.6-luna"), wantUA)
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

func TestModelOverrideHeadersFallsBackToEmbeddedCatalog(t *testing.T) {
	const wantUA = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	original := getModels()
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &staticModelsJSON{CodexPro: []*ModelInfo{{ID: "gpt-5.6-luna"}}}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(
		func() {
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = original
			modelsCatalogStore.mu.Unlock()
		},
	)

	assertGPT56ModelOverrideHeaders(t, ModelOverrideHeaders("gpt-5.6-luna"), wantUA)
}

func assertGPT56ModelOverrideHeaders(t *testing.T, got map[string]string, wantUA string) {
	t.Helper()
	if got == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil, want headers")
	}
	if got["user-agent"] != wantUA {
		t.Fatalf("user-agent = %q, want %q", got["user-agent"], wantUA)
	}
	if got["originator"] != "codex-tui" {
		t.Fatalf("originator = %q, want codex-tui", got["originator"])
	}
	if got["version"] != "0.144.0" {
		t.Fatalf("version = %q, want 0.144.0", got["version"])
	}
}

func TestXAIModelsIncludeGrok45(t *testing.T) {
	model := LookupStaticModelInfo("grok-4.5")
	if model == nil {
		t.Fatal("missing grok-4.5")
	}
	if model.ContextLength != 500000 {
		t.Fatalf("ContextLength = %d, want 500000", model.ContextLength)
	}
	if model.MaxCompletionTokens != 65536 {
		t.Fatalf("MaxCompletionTokens = %d, want 65536", model.MaxCompletionTokens)
	}
	if model.Thinking == nil {
		t.Fatal("Thinking = nil, want Grok 4.5 thinking support")
	}
	if model.Thinking.ZeroAllowed {
		t.Fatal("Thinking.ZeroAllowed = true, want false")
	}
	assertThinkingLevelsEqual(t, model.Thinking, []string{"low", "medium", "high"})
}

func assertStringSliceEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("slice length = %d, want %d; got=%#v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("slice[%d] = %q, want %q; got=%#v", index, got[index], want[index], got)
		}
	}
}

func assertThinkingLevelsEqual(t *testing.T, thinking *ThinkingSupport, want []string) {
	t.Helper()
	if thinking == nil {
		t.Fatal("Thinking = nil")
	}
	assertStringSliceEqual(t, thinking.Levels, want)
}
