package registry

import (
	"slices"
	"testing"
)

func TestXAIModelsFromCatalogFallsBackToEmbeddedChatModels(t *testing.T) {
	models := xaiModelsFromCatalog(&staticModelsJSON{})
	for _, model := range models {
		if model != nil && model.ID == "grok-4.5" {
			return
		}
	}
	t.Fatal("missing embedded Grok chat model when dynamic xAI catalog is empty")
}

func TestEmbeddedXAIModelsIncludeGrok46(t *testing.T) {
	var grok46 *ModelInfo
	for _, model := range GetEmbeddedXAIModels() {
		if model != nil && model.ID == "grok-4.6" {
			grok46 = model
			break
		}
	}
	if grok46 == nil {
		t.Fatal("missing embedded Grok 4.6 model")
	}
	if grok46.ContextLength != 500000 || grok46.MaxCompletionTokens != 65536 {
		t.Fatalf(
			"Grok 4.6 limits = (%d, %d), want (500000, 65536)",
			grok46.ContextLength,
			grok46.MaxCompletionTokens,
		)
	}
	if grok46.Thinking == nil || grok46.Thinking.ZeroAllowed ||
		!slices.Equal(grok46.Thinking.Levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("Grok 4.6 thinking metadata = %#v", grok46.Thinking)
	}
	if !slices.Equal(grok46.SupportedInputModalities, []string{"text", "image"}) ||
		!slices.Equal(grok46.SupportedOutputModalities, []string{"text"}) {
		t.Fatalf(
			"Grok 4.6 modalities = (%v, %v)",
			grok46.SupportedInputModalities,
			grok46.SupportedOutputModalities,
		)
	}
}

func TestWithXAIBuiltinsRegistersVideo15GAOnly(t *testing.T) {
	models := withXAIBuiltins(nil)
	seenGA := false
	for _, model := range models {
		if model == nil {
			continue
		}
		switch model.ID {
		case xaiBuiltinVideo15ModelID:
			seenGA = true
		case "grok-imagine-video-1.5-preview":
			t.Fatal("preview alias must not be exposed by the default xAI catalog")
		}
	}
	if !seenGA {
		t.Fatalf("missing %s builtin model", xaiBuiltinVideo15ModelID)
	}
}
