package cliproxy

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestBuildOpenAICompatibilityConfigModelsMarksImageModel(t *testing.T) {
	models := buildOpenAICompatibilityConfigModels(
		&config.OpenAICompatibility{
			Name: "provider",
			Models: []config.OpenAICompatibilityModel{
				{Name: "gpt-text", Alias: "text"},
				{Name: "gpt-image", Alias: "image", Image: true},
			},
		},
	)
	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2", len(models))
	}
	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	if byID["text"].Type != "openai-compatibility" {
		t.Fatalf("text model type = %q", byID["text"].Type)
	}
	if byID["image"].Type != registry.OpenAIImageModelType {
		t.Fatalf("image model type = %q", byID["image"].Type)
	}
}
