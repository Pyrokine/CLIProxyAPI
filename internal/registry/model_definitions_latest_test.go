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
