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

func TestKimiModelsKeepK3256KAsLookupOnly(t *testing.T) {
	tests := []struct {
		id             string
		contextLength  int
		thinkingLevels []string
	}{
		{id: "kimi-k2.7-code-highspeed", contextLength: 262144, thinkingLevels: []string{"low", "high"}},
		{id: "kimi-k3", contextLength: 1048576, thinkingLevels: []string{"low", "high", "max"}},
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
				assertThinkingLevelsEqual(t, model.Thinking, test.thinkingLevels)
			},
		)
	}

	if model := findModelByID(GetKimiModels(), "kimi-k3-256k"); model != nil {
		t.Fatalf("Kimi default inventory contains lookup-only model: %+v", model)
	}
	legacy := LookupStaticModelInfoByChannel("kimi-k3-256k", "kimi")
	if legacy == nil || legacy.ContextLength != 262144 {
		t.Fatalf("Kimi K3 256K legacy lookup = %+v", legacy)
	}
}

func TestCatalogIncludesCurrentClaudeGeminiAndKimiModels(t *testing.T) {
	lookup := func(channel, modelID string) *ModelInfo {
		t.Helper()
		model := LookupStaticModelInfoByChannel(modelID, channel)
		if model == nil {
			t.Fatalf("%s model %q is missing", channel, modelID)
		}
		return model
	}

	for _, modelID := range []string{"claude-fable-5-1", "claude-opus-5-5"} {
		model := lookup("claude", modelID)
		if model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 {
			t.Fatalf("%s token limits = %d/%d, want 1000000/128000", modelID, model.ContextLength, model.MaxCompletionTokens)
		}
		assertThinkingLevelsEqual(t, model.Thinking, []string{"low", "medium", "high", "xhigh", "max"})
		if model.Thinking.ZeroAllowed || !model.Thinking.DynamicAllowed {
			t.Fatalf("%s thinking flags = %+v, want always-on adaptive thinking", modelID, model.Thinking)
		}
	}

	for _, channel := range []string{"gemini", "vertex", "aistudio"} {
		gemini := lookup(channel, "gemini-3.8-flash")
		if gemini.ContextLength != 1048576 || gemini.MaxCompletionTokens != 65536 {
			t.Fatalf(
				"%s Gemini 3.8 token limits = %d/%d, want 1048576/65536",
				channel,
				gemini.ContextLength,
				gemini.MaxCompletionTokens,
			)
		}
		assertThinkingLevelsEqual(t, gemini.Thinking, []string{"low", "medium", "high"})

		image := lookup(channel, "gemini-3.1-flash-image")
		if image.InputTokenLimit != 131072 || image.OutputTokenLimit != 32768 {
			t.Fatalf("%s Gemini 3.1 Flash Image limits = %d/%d", channel, image.InputTokenLimit, image.OutputTokenLimit)
		}
		assertThinkingLevelsEqual(t, image.Thinking, []string{"minimal", "high"})
		assertStringSliceEqual(t, image.SupportedInputModalities, []string{"text", "image", "video", "pdf"})
		assertStringSliceEqual(t, image.SupportedOutputModalities, []string{"text", "image"})

		lite := lookup(channel, "gemini-3.1-flash-lite-image")
		if lite.InputTokenLimit != 65536 || lite.OutputTokenLimit != 4096 {
			t.Fatalf("%s Gemini 3.1 Flash-Lite Image limits = %d/%d", channel, lite.InputTokenLimit, lite.OutputTokenLimit)
		}
		assertThinkingLevelsEqual(t, lite.Thinking, []string{"minimal", "high"})
		assertStringSliceEqual(t, lite.SupportedOutputModalities, []string{"text", "image"})
		if lite.NativeCapabilities != nil && lite.NativeCapabilities.WebSearch != nil &&
			*lite.NativeCapabilities.WebSearch {
			t.Fatalf("%s Gemini 3.1 Flash-Lite Image unexpectedly supports search grounding", channel)
		}
	}
	assertThinkingLevelsEqual(
		t,
		lookup("antigravity", "gemini-3.8-flash-high").Thinking,
		[]string{"low", "medium", "high"},
	)

	for _, channel := range []string{"gemini", "vertex", "gemini-cli", "aistudio"} {
		for _, modelID := range []string{
			"gemini-3-pro-preview",
			"gemini-3.1-flash-image-preview",
			"gemini-3.1-flash-lite-preview",
		} {
			if model := LookupStaticModelInfoByChannel(modelID, channel); model != nil {
				t.Fatalf("retired %s model %q remains: %+v", channel, modelID, model)
			}
		}
	}

	for _, modelID := range []string{
		"kimi-k2.8",
		"k2.8",
		"kimi-k2.8-preview",
		"k2.8-preview",
		"kimi-k2.8-code",
		"k2.8-code",
	} {
		if model := LookupStaticModelInfo(modelID); model != nil {
			t.Fatalf("unsupported Kimi model %q remains: %+v", modelID, model)
		}
	}
	assertThinkingLevelsEqual(t, lookup("kimi", "kimi-k2.5").Thinking, []string{"low", "high"})

	for _, supported := range []struct {
		channel string
		modelID string
	}{
		{channel: "claude", modelID: "claude-opus-5"},
		{channel: "codex", modelID: "gpt-5.6-sol"},
		{channel: "xai", modelID: "grok-4.6"},
	} {
		model := lookup(supported.channel, supported.modelID)
		if model.NativeCapabilities == nil || model.NativeCapabilities.WebSearch == nil ||
			!*model.NativeCapabilities.WebSearch {
			t.Fatalf(
				"%s model %q native web search metadata = %+v, want true",
				supported.channel,
				supported.modelID,
				model.NativeCapabilities,
			)
		}
	}

	kimi := lookup("kimi", "kimi-k2.5")
	capability := ResolveResponsesWebSearchCapability([]NativeCapabilityRoute{
		{Provider: "kimi", NativeCapabilities: kimi.NativeCapabilities},
	})
	if capability == nil || *capability {
		t.Fatalf("Kimi K2.5 web search capability = %v, want false", capability)
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
	for _, test := range []struct {
		modelID string
		input   int
		output  int
	}{
		{modelID: "gemini-3.1-flash-image", input: 131072, output: 32768},
		{modelID: "gemini-3.1-flash-lite-image", input: 65536, output: 4096},
		{modelID: "gemini-3-pro-image", input: 1048576, output: 65536},
	} {
		model := findModelByID(vertexModels, test.modelID)
		if model == nil {
			t.Fatalf("Vertex catalog must expose %s", test.modelID)
		}
		if model.InputTokenLimit != test.input || model.OutputTokenLimit != test.output {
			t.Fatalf(
				"Vertex %s token limits = %d/%d, want %d/%d",
				test.modelID,
				model.InputTokenLimit,
				model.OutputTokenLimit,
				test.input,
				test.output,
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
	if models == nil {
		t.Fatal("embedded Gemini catalog is unavailable")
	}
	for _, modelID := range []string{
		"gemini-3-pro-preview",
		"gemini-3.1-flash-image-preview",
		"gemini-3.1-flash-lite-preview",
	} {
		if model := findModelByID(models.Gemini, modelID); model != nil {
			t.Fatalf("embedded Gemini catalog retains retired model %s", modelID)
		}
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

func TestFinalCatalogExcludesWithdrawnModelsAndWorkMode(t *testing.T) {
	for _, modelID := range []string{
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.6-sol-wm",
		"gpt-5.6-sol-work-mode",
	} {
		if model := LookupStaticModelInfoByChannel(modelID, "codex"); model != nil {
			t.Errorf("withdrawn Codex model %q = %+v, want nil", modelID, model)
		}
	}
	for _, modelID := range []string{
		"gemini-3-flash-agent",
		"gemini-3.5-flash-extra-low",
		"gemini-3.5-flash-low",
	} {
		if model := LookupStaticModelInfoByChannel(modelID, "antigravity"); model != nil {
			t.Errorf("withdrawn Antigravity model %q = %+v, want nil", modelID, model)
		}
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
				if model.Config != nil {
					t.Fatalf("Astra config = %#v, want no catalog header override", model.Config)
				}
			},
		)
	}
}

func TestCodexStaticTiersPlaceGPT6AndExcludeSpark(t *testing.T) {
	tiers := []struct {
		name    string
		models  []*ModelInfo
		wantSol bool
	}{
		{name: "free", models: GetCodexFreeModels()},
		{name: "team", models: GetCodexTeamModels(), wantSol: true},
		{name: "plus", models: GetCodexPlusModels(), wantSol: true},
		{name: "pro", models: GetCodexProModels(), wantSol: true},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			if spark := findModelByID(tier.models, "gpt-5.3-codex-spark"); spark != nil {
				t.Fatalf("retired Spark model remains in %s inventory: %+v", tier.name, spark)
			}
			sol := findModelByID(tier.models, "gpt-6-sol")
			if (sol != nil) != tier.wantSol {
				t.Fatalf("gpt-6-sol present = %t, want %t", sol != nil, tier.wantSol)
			}
			luna := findModelByID(tier.models, "gpt-6-luna")
			if luna == nil {
				t.Fatal("gpt-6-luna is missing")
			}
			for _, model := range []*ModelInfo{sol, luna} {
				if model == nil {
					continue
				}
				if model.ContextLength != 272000 || model.MaxCompletionTokens != 128000 {
					t.Fatalf("%s token limits = %d/%d", model.ID, model.ContextLength, model.MaxCompletionTokens)
				}
				assertThinkingLevelsEqual(t, model.Thinking, []string{"low", "medium", "high", "xhigh", "max"})
				assertStringSliceEqual(t, model.SupportedInputModalities, []string{"text", "image"})
			}
		})
	}

	legacy := LookupStaticModelInfoByChannel("gpt-5.3-codex-spark", "codex")
	if legacy == nil || legacy.ContextLength != 128000 {
		t.Fatalf("Spark legacy lookup = %+v", legacy)
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
				if model.ContextLength != 921000 {
					t.Fatalf("ContextLength = %d, want 921000", model.ContextLength)
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
	const wantUA = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	GetGlobalRegistry().RegisterClient("test-model-override-shadow", "codex", []*ModelInfo{{ID: "gpt-5.6-luna"}})
	defer GetGlobalRegistry().UnregisterClient("test-model-override-shadow")

	assertGPT56ModelOverrideHeaders(t, ModelOverrideHeaders("gpt-5.6-luna"), wantUA)
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

func TestModelOverrideHeadersFallsBackToEmbeddedCatalog(t *testing.T) {
	const wantUA = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
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

func TestEmbeddedStaticModelDefinitionsIgnoreRemoteCatalog(t *testing.T) {
	original := getModels()
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &staticModelsJSON{Kimi: []*ModelInfo{{ID: "kimi-k2.8"}}}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(
		func() {
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = original
			modelsCatalogStore.mu.Unlock()
		},
	)

	dynamic := GetStaticModelDefinitionsByChannel("kimi")
	if len(dynamic) != 1 || dynamic[0] == nil || dynamic[0].ID != "kimi-k2.8" {
		t.Fatalf("dynamic Kimi catalog = %+v, want remote kimi-k2.8", dynamic)
	}
	embedded := GetEmbeddedStaticModelDefinitionsByChannel("kimi")
	if findModelByID(embedded, "kimi-k2.8") != nil {
		t.Fatalf("embedded Kimi catalog contains remote-only kimi-k2.8: %+v", embedded)
	}
	if findModelByID(embedded, "kimi-k3") == nil {
		t.Fatalf("embedded Kimi catalog is missing kimi-k3: %+v", embedded)
	}
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
	if version, exists := got["version"]; exists {
		t.Fatalf("version = %q, want no version override", version)
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
