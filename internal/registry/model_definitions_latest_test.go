package registry

import "testing"

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
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

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
