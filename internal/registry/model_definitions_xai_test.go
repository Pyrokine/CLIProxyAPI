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

func TestEmbeddedXAIModelsIncludeGrok47(t *testing.T) {
	model := findModelByID(GetEmbeddedXAIModels(), "grok-4.7")
	if model == nil {
		t.Fatal("missing embedded Grok 4.7 model")
	}
	if model.ContextLength != 500000 || model.MaxCompletionTokens != 500000 {
		t.Fatalf("Grok 4.7 limits = (%d, %d), want (500000, 500000)", model.ContextLength, model.MaxCompletionTokens)
	}
	if model.Thinking == nil || model.Thinking.ZeroAllowed ||
		!slices.Equal(model.Thinking.Levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("Grok 4.7 thinking metadata = %#v", model.Thinking)
	}
	if !slices.Equal(model.SupportedInputModalities, []string{"text", "image"}) ||
		!slices.Equal(model.SupportedOutputModalities, []string{"text"}) {
		t.Fatalf("Grok 4.7 modalities = (%v, %v)", model.SupportedInputModalities, model.SupportedOutputModalities)
	}
}

func TestWithXAIBuiltinsRegistersCurrentMediaModels(t *testing.T) {
	models := withXAIBuiltins(nil)
	image20 := findModelByID(models, xaiBuiltinImage20ModelID)
	if image20 == nil {
		t.Fatalf("missing %s builtin model", xaiBuiltinImage20ModelID)
	}
	if !slices.Equal(image20.SupportedInputModalities, []string{"text", "image"}) ||
		!slices.Equal(image20.SupportedOutputModalities, []string{"image"}) {
		t.Fatalf("Image 2.0 modalities = (%v, %v)", image20.SupportedInputModalities, image20.SupportedOutputModalities)
	}
	if findModelByID(models, xaiBuiltinImageModelID) == nil {
		t.Fatalf("legacy %s builtin model is missing", xaiBuiltinImageModelID)
	}
	if findModelByID(models, xaiBuiltinVideo15ModelID) == nil {
		t.Fatalf("missing %s builtin model", xaiBuiltinVideo15ModelID)
	}
	if preview := findModelByID(models, "grok-imagine-video-1.5-preview"); preview != nil {
		t.Fatal("preview alias must not be exposed by the default xAI catalog")
	}
}
