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
				ID:                       "custom-codex-max",
				Object:                   "model",
				OwnedBy:                  "test",
				Type:                     "openai",
				DisplayName:              "Custom Codex Max",
				Description:              "Custom model from registry",
				ContextLength:            123456,
				SupportedInputModalities: []string{"TEXT", "Image", "audio", "image"},
				Thinking:                 &registry.ThinkingSupport{Levels: []string{"none", "max"}},
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
	assertCodexClientModalities(t, custom, []string{"text", "image"})
	if got, _ := custom["supports_image_detail_original"].(bool); !got {
		t.Fatalf("supports_image_detail_original = %#v, want true", custom["supports_image_detail_original"])
	}
}

func TestCodexClientModelsHidesImageModelsAndClearsModalities(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-codex-client-image-models"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:                       "custom-codex-image",
				Object:                   "model",
				OwnedBy:                  "test",
				Type:                     registry.OpenAIImageModelType,
				DisplayName:              "Custom Codex Image",
				Description:              "Custom image model from registry",
				SupportedInputModalities: []string{"text", "image"},
			},
		},
	)
	t.Cleanup(
		func() {
			modelRegistry.UnregisterClient(clientID)
		},
	)

	resp := CodexClientModelsResponse([]map[string]any{{"id": "custom-codex-image"}})
	models, ok := resp["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", resp["models"])
	}

	model := models[0]
	if got, _ := model["visibility"].(string); got != "hide" {
		t.Fatalf("visibility = %q, want hide", got)
	}
	if _, ok := model["input_modalities"]; ok {
		t.Fatalf("input_modalities = %#v, want absent", model["input_modalities"])
	}
	if _, ok := model["supports_image_detail_original"]; ok {
		t.Fatalf("supports_image_detail_original = %#v, want absent", model["supports_image_detail_original"])
	}
}

func TestCodexClientModelsUsesUpdatedGPT55Template(t *testing.T) {
	bySlug := codexClientModelsBySlug(t, []map[string]any{{"id": "gpt-5.5"}})
	model := bySlug["gpt-5.5"]
	if model == nil {
		t.Fatalf("gpt-5.5 missing from response: %#v", bySlug)
	}

	if got, _ := model["comp_hash"].(string); got != "2911" {
		t.Fatalf("comp_hash = %q, want 2911", got)
	}
	if got, _ := model["use_responses_lite"].(bool); got {
		t.Fatalf("use_responses_lite = %v, want false", got)
	}
	if got, _ := model["include_skills_usage_instructions"].(bool); !got {
		t.Fatalf("include_skills_usage_instructions = %v, want true", got)
	}
	if got := model["availability_nux"]; got != nil {
		t.Fatalf("availability_nux = %#v, want nil", got)
	}
	if got := model["tool_mode"]; got != nil {
		t.Fatalf("tool_mode = %#v, want nil", got)
	}
	if got := model["multi_agent_version"]; got != nil {
		t.Fatalf("multi_agent_version = %#v, want nil", got)
	}
	if got := model["auto_review_model_override"]; got != nil {
		t.Fatalf("auto_review_model_override = %#v, want nil", got)
	}
	if got := codexClientTestInt(model["context_window"]); got != 272000 {
		t.Fatalf("context_window = %v, want 272000", model["context_window"])
	}
	if got, _ := model["default_reasoning_level"].(string); got != "medium" {
		t.Fatalf("default_reasoning_level = %q, want medium", got)
	}
	assertCodexClientReasoningLevels(t, model, []string{"low", "medium", "high", "xhigh"})
}

func TestCodexClientModelsUsesGPT56Templates(t *testing.T) {
	bySlug := codexClientModelsBySlug(
		t, []map[string]any{
			{"id": "gpt-5.6-sol"},
			{"id": "gpt-5.6-terra"},
			{"id": "gpt-5.6-luna"},
		},
	)
	tests := []struct {
		slug              string
		displayName       string
		priority          int
		defaultReasoning  string
		multiAgentVersion string
		levels            []string
	}{
		{
			slug:              "gpt-5.6-sol",
			displayName:       "GPT-5.6-Sol",
			priority:          1,
			defaultReasoning:  "low",
			multiAgentVersion: "v2",
			levels:            []string{"low", "medium", "high", "xhigh", "max", "ultra"},
		},
		{
			slug:              "gpt-5.6-terra",
			displayName:       "GPT-5.6-Terra",
			priority:          2,
			defaultReasoning:  "medium",
			multiAgentVersion: "v2",
			levels:            []string{"low", "medium", "high", "xhigh", "max", "ultra"},
		},
		{
			slug:              "gpt-5.6-luna",
			displayName:       "GPT-5.6-Luna",
			priority:          3,
			defaultReasoning:  "medium",
			multiAgentVersion: "v1",
			levels:            []string{"low", "medium", "high", "xhigh", "max"},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.slug, func(t *testing.T) {
				model := bySlug[tt.slug]
				if model == nil {
					t.Fatalf("%s missing from response: %#v", tt.slug, bySlug)
				}
				if got, _ := model["display_name"].(string); got != tt.displayName {
					t.Fatalf("display_name = %q, want %q", got, tt.displayName)
				}
				if got := codexClientTestInt(model["priority"]); got != tt.priority {
					t.Fatalf("priority = %v, want %d", model["priority"], tt.priority)
				}
				if got, _ := model["default_reasoning_level"].(string); got != tt.defaultReasoning {
					t.Fatalf("default_reasoning_level = %q, want %q", got, tt.defaultReasoning)
				}
				if got, _ := model["comp_hash"].(string); got != "3000" {
					t.Fatalf("comp_hash = %q, want 3000", got)
				}
				if got, _ := model["use_responses_lite"].(bool); !got {
					t.Fatalf("use_responses_lite = %v, want true", got)
				}
				if got, ok := model["include_skills_usage_instructions"].(bool); !ok || got {
					t.Fatalf("include_skills_usage_instructions = %#v, want false", model["include_skills_usage_instructions"])
				}
				if got, _ := model["minimal_client_version"].(string); got != "0.144.0" {
					t.Fatalf("minimal_client_version = %q, want 0.144.0", got)
				}
				if got, _ := model["tool_mode"].(string); got != "code_mode_only" {
					t.Fatalf("tool_mode = %q, want code_mode_only", got)
				}
				if got, _ := model["multi_agent_version"].(string); got != tt.multiAgentVersion {
					t.Fatalf("multi_agent_version = %q, want %q", got, tt.multiAgentVersion)
				}
				if got := codexClientTestInt(model["context_window"]); got != 372000 {
					t.Fatalf("context_window = %v, want 372000", model["context_window"])
				}
				if got := codexClientTestInt(model["max_context_window"]); got != 372000 {
					t.Fatalf("max_context_window = %v, want 372000", model["max_context_window"])
				}
				assertCodexClientReasoningLevels(t, model, tt.levels)
			},
		)
	}
}

func TestCodexClientModelsResponse_GPT56SolExcludesFreePlans(t *testing.T) {
	resp := CodexClientModelsResponse([]map[string]any{{"id": "gpt-5.6-sol"}})
	models, ok := resp["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models type = %T, want []map[string]any", resp["models"])
	}
	if len(models) != 1 {
		t.Fatalf("models length = %d, want 1", len(models))
	}
	plans, ok := models[0]["available_in_plans"].([]any)
	if !ok {
		t.Fatalf("available_in_plans = %T, want []any", models[0]["available_in_plans"])
	}
	for _, rawPlan := range plans {
		plan, _ := rawPlan.(string)
		if plan == "free" || plan == "free_workspace" {
			t.Fatalf("available_in_plans contains %q: %#v", plan, plans)
		}
	}
}

func TestCodexClientModelsResponse_PreservesUltraReasoningEffort(t *testing.T) {
	resp := CodexClientModelsResponse([]map[string]any{{"id": "gpt-5.6-sol"}})
	models, ok := resp["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models type = %T, want []map[string]any", resp["models"])
	}

	var sol map[string]any
	for _, entry := range models {
		if stringModelValue(entry, "slug") == "gpt-5.6-sol" {
			sol = entry
			break
		}
	}
	if sol == nil {
		t.Fatal("expected codex client entry for gpt-5.6-sol")
	}

	levels, ok := sol["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %T, want []any", sol["supported_reasoning_levels"])
	}
	for _, rawLevel := range levels {
		level, ok := rawLevel.(map[string]any)
		if ok && stringModelValue(level, "effort") == "ultra" {
			return
		}
	}

	t.Fatalf("supported_reasoning_levels = %#v, want ultra", levels)
}

func codexClientModelsBySlug(t *testing.T, input []map[string]any) map[string]map[string]any {
	t.Helper()
	resp := CodexClientModelsResponse(input)
	models, ok := resp["models"].([]map[string]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v, want non-empty []map[string]any", resp["models"])
	}
	bySlug := make(map[string]map[string]any, len(models))
	for _, model := range models {
		slug, _ := model["slug"].(string)
		bySlug[slug] = model
	}
	return bySlug
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

func assertCodexClientModalities(t *testing.T, model map[string]any, want []string) {
	t.Helper()
	rawModalities, ok := model["input_modalities"].([]any)
	if !ok {
		t.Fatalf("input_modalities = %#v, want array", model["input_modalities"])
	}
	if len(rawModalities) != len(want) {
		t.Fatalf("input_modalities length = %d, want %d", len(rawModalities), len(want))
	}
	for index, rawModality := range rawModalities {
		if got, _ := rawModality.(string); got != want[index] {
			t.Fatalf("input_modalities[%d] = %q, want %q", index, got, want[index])
		}
	}
}
