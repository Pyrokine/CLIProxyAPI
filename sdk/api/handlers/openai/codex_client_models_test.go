package openai

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCodexClientModelsAddsMetadataForCustomModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-codex-client-models"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:            "custom-codex-max",
				Object:        "model",
				OwnedBy:       "test",
				Type:          "openai",
				DisplayName:   "Custom Codex Max",
				Description:   "Custom model from registry",
				ContextLength: 123456,
				Thinking:      &registry.ThinkingSupport{Levels: []string{"none", "max"}},
			},
		},
	)
	t.Cleanup(
		func() {
			modelRegistry.UnregisterClient(clientID)
		},
	)

	resp := CodexClientModelsResponse(
		[]map[string]any{
			{"id": "gpt-5.5"},
			{"id": "custom-codex-max", "display_name": "Fallback", "description": "Fallback"},
		},
	)
	models, ok := resp["models"].([]map[string]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v, want non-empty []map[string]any", resp["models"])
	}

	bySlug := make(map[string]map[string]any, len(models))
	for _, model := range models {
		slug, _ := model["slug"].(string)
		bySlug[slug] = model
	}
	custom := bySlug["custom-codex-max"]
	if custom == nil {
		t.Fatalf("custom model missing from response: %#v", bySlug)
	}

	if got, _ := custom["display_name"].(string); got != "Custom Codex Max" {
		t.Fatalf("display_name = %q, want Custom Codex Max", got)
	}
	if got, _ := custom["description"].(string); got != "Custom model from registry" {
		t.Fatalf("description = %q, want Custom model from registry", got)
	}
	if got := codexClientTestInt(custom["context_window"]); got != 123456 {
		t.Fatalf("context_window = %v, want 123456", custom["context_window"])
	}
	serviceTiers, ok := custom["service_tiers"].([]any)
	if !ok || len(serviceTiers) != 0 {
		t.Fatalf("service_tiers = %#v, want empty array", custom["service_tiers"])
	}

	templates, _, err := loadCodexClientModelTemplates()
	if err != nil {
		t.Fatalf("loadCodexClientModelTemplates() error = %v", err)
	}
	if got, want := codexClientTestInt(custom["priority"]), maxCodexClientTemplatePriority(templates)+100; got != want {
		t.Fatalf("priority = %v, want %d", custom["priority"], want)
	}
	assertCodexClientReasoningLevels(t, custom, []string{"none", "max"})
}

func codexClientTestInt(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func assertCodexClientReasoningLevels(t *testing.T, model map[string]any, want []string) {
	t.Helper()
	rawLevels, ok := model["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %#v, want array", model["supported_reasoning_levels"])
	}
	if len(rawLevels) != len(want) {
		t.Fatalf("supported_reasoning_levels length = %d, want %d", len(rawLevels), len(want))
	}
	for index, rawLevel := range rawLevels {
		level, ok := rawLevel.(map[string]any)
		if !ok {
			t.Fatalf("supported_reasoning_levels[%d] = %#v, want object", index, rawLevel)
		}
		if got, _ := level["effort"].(string); got != want[index] {
			t.Fatalf("supported_reasoning_levels[%d].effort = %q, want %q", index, got, want[index])
		}
	}
}
