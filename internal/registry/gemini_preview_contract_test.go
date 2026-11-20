package registry

import (
	"reflect"
	"testing"
)

func TestGemini31FlashLitePreviewPublishedChannels(t *testing.T) {
	const modelID = "gemini-3.1-flash-lite-preview"
	webSearch := true
	for _, channel := range []string{"gemini", "gemini-cli", "aistudio"} {
		t.Run(
			channel, func(t *testing.T) {
				levels := []string{"minimal", "low", "medium", "high"}
				if channel == "gemini" {
					levels = []string{"minimal", "high"}
				}
				want := &ModelInfo{
					ID: modelID, Object: "model", Created: 1776288000,
					OwnedBy: "google", Type: "gemini",
					DisplayName: "Gemini 3.1 Flash Lite Preview",
					Name:        "models/" + modelID, Version: "3.1",
					Description:     "Our smallest and most cost effective model, built for at scale usage.",
					InputTokenLimit: 1048576, OutputTokenLimit: 65536,
					SupportedGenerationMethods: []string{
						"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
					},
					Thinking: &ThinkingSupport{
						Min: 128, Max: 32768, DynamicAllowed: true, Levels: levels,
					},
					SupportedInputModalities:  []string{"text", "image", "audio", "video"},
					SupportedOutputModalities: []string{"text"},
					NativeCapabilities:        &NativeCapabilities{WebSearch: &webSearch},
				}
				for _, source := range []struct {
					name   string
					models []*ModelInfo
				}{
					{name: "embedded", models: GetEmbeddedStaticModelDefinitionsByChannel(channel)},
					{name: "catalog", models: GetStaticModelDefinitionsByChannel(channel)},
				} {
					t.Run(
						source.name, func(t *testing.T) {
							model := findModelByID(source.models, modelID)
							if channel != "gemini-cli" {
								if model != nil {
									t.Fatalf("retired Developer API preview remains in %s defaults", channel)
								}
								return
							}
							if !reflect.DeepEqual(model, want) {
								t.Fatalf("preview metadata = %+v, want %+v", model, want)
							}
						},
					)
				}
				if model := LookupStaticModelInfoByChannel(modelID, channel); !reflect.DeepEqual(model, want) {
					t.Fatalf("exact preview lookup = %+v, want %+v", model, want)
				}
			},
		)
	}
}
