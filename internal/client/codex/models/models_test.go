package models

import (
	"encoding/json"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCodexClientModelsRequestMetadataOverridesRegistry(t *testing.T) {
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
				MaxCompletionTokens:      11111,
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

	resp := buildTestResponse(
		[]map[string]any{
			{"id": "gpt-5.5"},
			{
				"id":                       "custom-codex-max",
				"display_name":             "Request Display",
				"description":              "Request Description",
				"context_length":           222222,
				"max_completion_tokens":    65432,
				"supportedInputModalities": []string{"text"},
				"thinking":                 map[string]any{"levels": []string{"low", "high"}},
			},
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

	if got, _ := custom["display_name"].(string); got != "Request Display" {
		t.Fatalf("display_name = %q, want Request Display", got)
	}
	if got, _ := custom["description"].(string); got != "Request Description" {
		t.Fatalf("description = %q, want Request Description", got)
	}
	if got := codexClientTestInt(custom["context_window"]); got != 222222 {
		t.Fatalf("context_window = %v, want 222222", custom["context_window"])
	}
	if got := codexClientTestInt(custom["max_tokens"]); got != 65432 {
		t.Fatalf("max_tokens = %v, want 65432", custom["max_tokens"])
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
	assertCodexClientReasoningLevels(t, custom, []string{"low", "high"})
	assertCodexClientModalities(t, custom, []string{"text"})
	if _, exists := custom["supports_image_detail_original"]; exists {
		t.Fatalf("supports_image_detail_original = %#v, want absent", custom["supports_image_detail_original"])
	}
}

func TestCodexClientModelsAppliesConfiguredMaxContextLengthToTemplate(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-codex-client-context-override"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:               "gpt-5.5",
				Object:           "model",
				OwnedBy:          "test",
				Type:             "openai",
				DisplayName:      "Configured GPT",
				ContextLength:    262144,
				MaxContextLength: 262144,
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	models := codexClientModelsBySlug(t, modelRegistry.GetAvailableModels("openai"))
	model := models["gpt-5.5"]
	if model == nil {
		t.Fatalf("gpt-5.5 missing from response: %#v", models)
	}
	if got, _ := model["display_name"].(string); got != "Configured GPT" {
		t.Fatalf("display_name = %q, want Configured GPT", got)
	}
	for _, field := range []string{"context_window", "max_context_window", "max_context_length"} {
		if got := codexClientTestInt(model[field]); got != 262144 {
			t.Fatalf("%s = %v, want 262144", field, model[field])
		}
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

	resp := buildTestResponse([]map[string]any{{"id": "custom-codex-image"}})
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

func TestCodexClientModelsHidesOfficialGPTImageModels(t *testing.T) {
	imageModelIDs := registry.CodexImageModelIDs()
	input := make([]map[string]any, 0, len(imageModelIDs)*2)
	for _, modelID := range imageModelIDs {
		input = append(
			input,
			map[string]any{"id": modelID},
			map[string]any{"id": "tenant/" + modelID},
		)
	}
	models := codexClientModelsBySlug(t, input)
	for _, modelID := range imageModelIDs {
		for _, publicID := range []string{modelID, "tenant/" + modelID} {
			model := models[publicID]
			if model == nil {
				t.Fatalf("%s missing from response", publicID)
			}
			if visibility, _ := model["visibility"].(string); visibility != "hide" {
				t.Fatalf("%s visibility = %q, want hide", publicID, visibility)
			}
		}
	}
}

func TestCodexClientModelsHidesSpeechModels(t *testing.T) {
	modelIDs := []string{"grok-tts", "grok-voice-tts-1.0", "xai/grok-tts", "xai/grok-voice-tts-1.0", "tenant/GROK-TTS"}
	input := make([]map[string]any, 0, len(modelIDs)+1)
	for _, modelID := range append(modelIDs, "grok-4") {
		input = append(input, map[string]any{"id": modelID, "supportedInputModalities": []string{"text"}})
	}
	response := BuildResponseForClientWithCPACapabilities(input, nil, nil, false, "", func(string) bool { return true })
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != len(input) {
		t.Fatalf("models = %#v, want %d models", response["models"], len(input))
	}
	for _, model := range models {
		modelID, _ := model["slug"].(string)
		t.Run(modelID, func(t *testing.T) {
			wantVisibility, wantPatch := "hide", any(nil)
			if modelID == "grok-4" {
				wantVisibility, wantPatch = "list", "freeform"
			}
			if model["visibility"] != wantVisibility {
				t.Fatalf("visibility = %v, want %s", model["visibility"], wantVisibility)
			}
			if model["apply_patch_tool_type"] != wantPatch {
				t.Fatalf("apply_patch_tool_type = %v, want %v", model["apply_patch_tool_type"], wantPatch)
			}
		})
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
			priority:          6,
			defaultReasoning:  "low",
			multiAgentVersion: "v2",
			levels:            []string{"low", "medium", "high", "xhigh", "max", "ultra"},
		},
		{
			slug:              "gpt-5.6-terra",
			displayName:       "GPT-5.6-Terra",
			priority:          7,
			defaultReasoning:  "medium",
			multiAgentVersion: "v2",
			levels:            []string{"low", "medium", "high", "xhigh", "max", "ultra"},
		},
		{
			slug:              "gpt-5.6-luna",
			displayName:       "GPT-5.6-Luna",
			priority:          8,
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
					t.Fatalf(
						"include_skills_usage_instructions = %#v, want false",
						model["include_skills_usage_instructions"],
					)
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
				if got := codexClientTestInt(model["context_window"]); got != 272000 {
					t.Fatalf("context_window = %v, want 272000", model["context_window"])
				}
				if got := codexClientTestInt(model["max_context_window"]); got != 872000 {
					t.Fatalf("max_context_window = %v, want 872000", model["max_context_window"])
				}
				assertCodexClientReasoningLevels(t, model, tt.levels)
			},
		)
	}
}

func TestCodexClientModelsUsesGPT6AstraTemplate(t *testing.T) {
	bySlug := codexClientModelsBySlug(t, []map[string]any{{"id": "gpt-6-astra"}})
	model := bySlug["gpt-6-astra"]
	if model == nil {
		t.Fatalf("gpt-6-astra missing from response: %#v", bySlug)
	}
	if got, _ := model["display_name"].(string); got != "GPT-6-Astra" {
		t.Fatalf("display_name = %q, want GPT-6-Astra", got)
	}
	if got := codexClientTestInt(model["priority"]); got != 1 {
		t.Fatalf("priority = %v, want 1", model["priority"])
	}
	if got, _ := model["default_reasoning_level"].(string); got != "medium" {
		t.Fatalf("default_reasoning_level = %q, want medium", got)
	}
	if got, _ := model["minimal_client_version"].(string); got != "0.153.0" {
		t.Fatalf("minimal_client_version = %q, want 0.153.0", got)
	}
	if got := codexClientTestInt(model["context_window"]); got != 272000 {
		t.Fatalf("context_window = %v, want 272000", model["context_window"])
	}
	if got := codexClientTestInt(model["max_context_window"]); got != 872000 {
		t.Fatalf("max_context_window = %v, want 872000", model["max_context_window"])
	}
	if got, _ := model["multi_agent_version"].(string); got != "v2" {
		t.Fatalf("multi_agent_version = %q, want v2", got)
	}
	if got, _ := model["supported_in_api"].(bool); !got {
		t.Fatalf("supported_in_api = %v, want true", got)
	}
	assertCodexClientReasoningLevels(t, model, []string{"low", "medium", "high", "xhigh", "max", "ultra"})
}

func TestCodexClientModelsUsesGPTReserveTemplate(t *testing.T) {
	model := codexClientModelsBySlug(t, []map[string]any{{"id": "gpt-reserve"}})["gpt-reserve"]
	if model == nil {
		t.Fatal("gpt-reserve missing from response")
	}
	if got, _ := model["visibility"].(string); got != "hide" {
		t.Fatalf("visibility = %q, want hide", got)
	}
	if got := codexClientTestInt(model["context_window"]); got != 272000 {
		t.Fatalf("context_window = %v, want 272000", model["context_window"])
	}
	if got := codexClientTestInt(model["max_context_window"]); got != 872000 {
		t.Fatalf("max_context_window = %v, want 872000", model["max_context_window"])
	}
	if got, _ := model["minimal_client_version"].(string); got != "0.144.0" {
		t.Fatalf("minimal_client_version = %q, want 0.144.0", got)
	}
	assertCodexClientReasoningLevels(t, model, []string{"low", "medium", "high", "xhigh", "max"})
}

func TestCodexClientTemplatesExcludeWithdrawnModels(t *testing.T) {
	templates, _, err := loadCodexClientModelTemplates()
	if err != nil {
		t.Fatalf("loadCodexClientModelTemplates() error = %v", err)
	}
	for _, slug := range []string{
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.6-sol-wm",
		"gpt-5.6-sol-work-mode",
	} {
		if _, exists := templates[slug]; exists {
			t.Fatalf("withdrawn template %q is still present", slug)
		}
	}
}

func TestCodexClientModelsResponse_GPT56SolIncludesFinalFreePlans(t *testing.T) {
	resp := buildTestResponse([]map[string]any{{"id": "gpt-5.6-sol"}})
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
	seen := make(map[string]bool, len(plans))
	for _, rawPlan := range plans {
		plan, _ := rawPlan.(string)
		seen[plan] = true
	}
	for _, plan := range []string{"free", "free_workspace"} {
		if !seen[plan] {
			t.Fatalf("available_in_plans = %#v, want %q", plans, plan)
		}
	}
}

func TestCodexClientModelsSearchToolRequiresTemplateAndCodexProvider(t *testing.T) {
	providers := map[string][]string{
		"new-codex-model": {"codex"},
		"gpt-5.5":         {"openai-compatible-deepseek"},
		"gpt-5.4":         {"codex", "xai"},
		"gpt-5.6-sol":     {"codex"},
	}
	resp := buildTestResponseWithProviders(
		[]map[string]any{
			{"id": "new-codex-model"},
			{"id": "gpt-5.5"},
			{"id": "gpt-5.4"},
			{"id": "gpt-5.6-sol"},
		},
		func(id string) []string { return providers[id] },
		false,
	)
	models, ok := resp["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models type = %T, want []map[string]any", resp["models"])
	}

	bySlug := make(map[string]map[string]any, len(models))
	for _, model := range models {
		bySlug[stringModelValue(model, "slug")] = model
	}
	if got, ok := bySlug["gpt-5.6-sol"]["supports_search_tool"].(bool); !ok || !got {
		t.Errorf("gpt-5.6-sol supports_search_tool = %#v, want true", bySlug["gpt-5.6-sol"]["supports_search_tool"])
	}
	for _, slug := range []string{"new-codex-model", "gpt-5.5", "gpt-5.4"} {
		if got, ok := bySlug[slug]["supports_search_tool"].(bool); !ok || got {
			t.Errorf("%s supports_search_tool = %#v, want false", slug, bySlug[slug]["supports_search_tool"])
		}
	}
}

func TestCodexClientTemplateAliasUsesCanonicalMetadataWithoutInventingCapabilities(t *testing.T) {
	const (
		clientID = "test-codex-client-template-alias"
		modelID  = "tenant/codex-alias"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{{
		ID:                      modelID,
		CatalogName:             "gpt-5.6-sol",
		ExplicitThinking:        true,
		ExplicitInputModalities: true,
		SupportedInputModalities: []string{
			"text",
		},
	}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	response := buildTestResponseWithProviders(
		[]map[string]any{{"id": modelID}},
		modelRegistry.GetModelProviders,
		false,
	)
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", response["models"])
	}
	model := models[0]
	if got, _ := model["slug"].(string); got != modelID {
		t.Fatalf("slug = %q, want %q", got, modelID)
	}
	if got := codexClientTestInt(model["context_window"]); got != 272000 {
		t.Fatalf("context_window = %v, want 272000", model["context_window"])
	}
	if got := codexClientTestInt(model["max_context_window"]); got != 872000 {
		t.Fatalf("max_context_window = %v, want 872000", model["max_context_window"])
	}
	assertCodexClientReasoningLevels(t, model, []string{})
	assertCodexClientModalities(t, model, []string{"text"})
	if got, _ := model["prefer_websockets"].(bool); got {
		t.Fatalf("prefer_websockets = %v, want false", got)
	}
	for _, field := range []string{"apply_patch_tool_type", "upgrade", "availability_nux"} {
		value, exists := model[field]
		if !exists || value != nil {
			t.Fatalf("%s = %#v, want present null", field, value)
		}
	}
}

func TestCodexClientUnknownModelKeepsCapabilitiesUnknown(t *testing.T) {
	model := codexClientModelsBySlug(t, []map[string]any{{"id": "unknown-compatible-model"}})["unknown-compatible-model"]
	if model == nil {
		t.Fatal("unknown-compatible-model missing from response")
	}
	assertCodexClientReasoningLevels(t, model, []string{})
	assertCodexClientModalities(t, model, []string{})
}

func TestApplyCPAWebSearchCapabilityThreeStates(t *testing.T) {
	trueValue := true
	falseValue := false
	tests := []struct {
		name          string
		clientVersion string
		capability    *bool
		wantPresent   bool
		wantValue     bool
	}{
		{name: "true", clientVersion: "cpa", capability: &trueValue, wantPresent: true, wantValue: true},
		{name: "false", clientVersion: "cpa", capability: &falseValue, wantPresent: true},
		{name: "unknown", clientVersion: "cpa"},
		{name: "non CPA client", clientVersion: "0.154.0", capability: &trueValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := map[string]any{"cpa_capabilities": map[string]any{"web_search": true}}
			applyCPAWebSearchCapability(
				entry,
				"model",
				func(string) *bool { return tt.capability },
				tt.clientVersion,
			)
			capabilities, present := entry["cpa_capabilities"].(map[string]any)
			if present != tt.wantPresent {
				t.Fatalf("cpa_capabilities present = %t, want %t; entry=%#v", present, tt.wantPresent, entry)
			}
			if present {
				if got, _ := capabilities["web_search"].(bool); got != tt.wantValue {
					t.Fatalf("web_search = %v, want %v", got, tt.wantValue)
				}
			}
		})
	}
}

func TestCodexClientCPAWebSearchCapabilitiesUseExactRegisteredRoutes(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	trueValue, falseValue := true, false
	modelRegistry.RegisterClient("test-cpa-capability-true", "codex", []*registry.ModelInfo{
		{
			ID:          "public-capable",
			CatalogName: "upstream-capable",
			NativeCapabilities: &registry.NativeCapabilities{
				WebSearch: &trueValue,
			},
		},
		{
			ID: "public-mixed",
			NativeCapabilities: &registry.NativeCapabilities{
				WebSearch: &trueValue,
			},
		},
	})
	modelRegistry.RegisterClient("test-cpa-capability-unknown", "claude", []*registry.ModelInfo{{ID: "public-mixed"}})
	modelRegistry.RegisterClient("test-cpa-capability-false", "codex", []*registry.ModelInfo{{
		ID: "public-unsupported",
		NativeCapabilities: &registry.NativeCapabilities{
			WebSearch: &falseValue,
		},
	}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("test-cpa-capability-true")
		modelRegistry.UnregisterClient("test-cpa-capability-unknown")
		modelRegistry.UnregisterClient("test-cpa-capability-false")
	})

	response := buildTestResponseWithCPACapabilities(
		[]map[string]any{
			{"id": "public-capable"},
			{"id": "public-mixed"},
			{"id": "public-unsupported"},
		},
		modelRegistry.GetModelProviders,
		modelRegistry.GetResponsesWebSearchCapability,
		false,
		"cpa",
	)
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 3 {
		t.Fatalf("models = %#v, want three models", response["models"])
	}
	bySlug := make(map[string]map[string]any, len(models))
	for _, model := range models {
		bySlug[stringModelValue(model, "slug")] = model
	}
	assertCapability := func(modelID string, want bool, wantPresent bool) {
		t.Helper()
		raw, present := bySlug[modelID]["cpa_capabilities"]
		if present != wantPresent {
			t.Fatalf("%s cpa_capabilities presence = %t, want %t", modelID, present, wantPresent)
		}
		if !present {
			return
		}
		capabilities, valid := raw.(map[string]any)
		if !valid || capabilities["web_search"] != want {
			t.Fatalf("%s cpa_capabilities = %#v, want web_search=%t", modelID, raw, want)
		}
	}
	assertCapability("public-capable", true, true)
	assertCapability("public-mixed", false, false)
	assertCapability("public-unsupported", false, true)
	if capability := modelRegistry.GetResponsesWebSearchCapability("upstream-capable"); capability != nil {
		t.Fatalf("upstream alias capability = %v, want unknown for an unregistered public ID", *capability)
	}
}

func TestResolveResponsesWebSearchCapabilityThreeStates(t *testing.T) {
	trueValue := true
	falseValue := false
	capabilities := func(value *bool) *registry.NativeCapabilities {
		return &registry.NativeCapabilities{WebSearch: value}
	}
	tests := []struct {
		name   string
		routes []registry.NativeCapabilityRoute
		want   *bool
	}{
		{name: "true", routes: []registry.NativeCapabilityRoute{{Provider: "codex", NativeCapabilities: capabilities(&trueValue)}}, want: &trueValue},
		{name: "explicit false", routes: []registry.NativeCapabilityRoute{{Provider: "codex", NativeCapabilities: capabilities(&falseValue)}}, want: &falseValue},
		{name: "unsupported path", routes: []registry.NativeCapabilityRoute{{Provider: "kimi", NativeCapabilities: capabilities(&trueValue)}}, want: &falseValue},
		{name: "unknown metadata", routes: []registry.NativeCapabilityRoute{{Provider: "codex"}}},
		{name: "unknown path", routes: []registry.NativeCapabilityRoute{{Provider: "new-provider", NativeCapabilities: capabilities(&trueValue)}}},
		{name: "true plus unknown", routes: []registry.NativeCapabilityRoute{{Provider: "codex", NativeCapabilities: capabilities(&trueValue)}, {Provider: "claude"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registry.ResolveResponsesWebSearchCapability(tt.routes)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("capability = %v, want unknown", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("capability = %v, want %v", got, *tt.want)
			}
		})
	}
}

func TestModelInfoNativeCapabilitiesAreInternalJSONMetadata(t *testing.T) {
	var info registry.ModelInfo
	if err := json.Unmarshal([]byte(`{"id":"model","native_capabilities":{"web_search":false}}`), &info); err != nil {
		t.Fatalf("unmarshal model info: %v", err)
	}
	if info.NativeCapabilities == nil || info.NativeCapabilities.WebSearch == nil || *info.NativeCapabilities.WebSearch {
		t.Fatalf("native capability = %+v, want explicit false", info.NativeCapabilities)
	}
	encoded, err := json.Marshal(&info)
	if err != nil {
		t.Fatalf("marshal model info: %v", err)
	}
	var public map[string]any
	if err = json.Unmarshal(encoded, &public); err != nil {
		t.Fatalf("decode public model info: %v", err)
	}
	if _, exposed := public["native_capabilities"]; exposed {
		t.Fatalf("public model info exposed native_capabilities: %s", encoded)
	}
}

func TestCodexClientModelsAcceptsMinimalReasoning(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-codex-client-minimal-reasoning"
	modelRegistry.RegisterClient(
		clientID, "openai", []*registry.ModelInfo{
			{
				ID:       "custom-codex-minimal",
				Object:   "model",
				OwnedBy:  "test",
				Type:     "openai",
				Thinking: &registry.ThinkingSupport{Levels: []string{"minimal", "low"}},
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	model := codexClientModelsBySlug(t, modelRegistry.GetAvailableModels("openai"))["custom-codex-minimal"]
	if model == nil {
		t.Fatal("custom-codex-minimal missing from response")
	}
	assertCodexClientReasoningLevels(t, model, []string{"minimal", "low"})
}

func TestCodexClientModelsResponse_PreservesUltraReasoningEffort(t *testing.T) {
	resp := buildTestResponse([]map[string]any{{"id": "gpt-5.6-sol"}})
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

func TestCodexClientModelsResponseFiltersExtendedReasoningLevelsByClientVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    []string
	}{
		{name: "legacy", version: "0.137.0", want: []string{"low", "medium", "high", "xhigh"}},
		{name: "legacy prerelease", version: "v0.143.9-beta.1", want: []string{"low", "medium", "high", "xhigh"}},
		{name: "boundary", version: "0.144.0", want: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{name: "missing", want: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{name: "invalid", version: "latest", want: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := buildTestResponseForClient([]map[string]any{{"id": "gpt-5.6-sol"}}, tt.version, false)
			models, ok := resp["models"].([]map[string]any)
			if !ok || len(models) != 1 {
				t.Fatalf("models = %#v, want one model", resp["models"])
			}
			assertCodexClientReasoningLevels(t, models[0], tt.want)
		})
	}
}

func TestCodexClientModelsResponsePreservesExplicitEmptyReasoningLevels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-codex-client-empty-reasoning"
	modelRegistry.RegisterClient(clientID, "openai", []*registry.ModelInfo{
		{
			ID:       "custom-codex-budget-only",
			Object:   "model",
			OwnedBy:  "test",
			Type:     "openai",
			Thinking: &registry.ThinkingSupport{Min: 1024, Max: 64000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:       "custom-codex-empty-levels",
			Object:   "model",
			OwnedBy:  "test",
			Type:     "openai",
			Thinking: &registry.ThinkingSupport{Levels: []string{}},
		},
	})
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	models := codexClientModelsBySlug(t, []map[string]any{
		{"id": "custom-codex-budget-only"},
		{"id": "custom-codex-empty-levels"},
	})
	for _, id := range []string{"custom-codex-budget-only", "custom-codex-empty-levels"} {
		model := models[id]
		if model == nil {
			t.Fatalf("%s missing from response", id)
		}
		encodedLevels, errMarshal := json.Marshal(model["supported_reasoning_levels"])
		if errMarshal != nil {
			t.Fatalf("marshal %s supported_reasoning_levels: %v", id, errMarshal)
		}
		if got := string(encodedLevels); got != "[]" {
			t.Fatalf("%s supported_reasoning_levels JSON = %s, want []", id, got)
		}
		if _, exists := model["default_reasoning_level"]; exists {
			t.Fatalf("%s default_reasoning_level = %#v, want absent", id, model["default_reasoning_level"])
		}
	}
}

func TestSanitizeCodexClientReasoningMetadataPreservesEmptyArray(t *testing.T) {
	tests := []struct {
		name    string
		version string
		levels  []any
	}{
		{name: "empty levels", version: "0.153.3", levels: []any{}},
		{name: "nil levels", version: "0.153.3"},
		{name: "legacy incompatible", version: "0.143.9", levels: []any{map[string]any{"effort": "max"}, map[string]any{"effort": "ultra"}}},
		{name: "invalid levels", version: "0.153.3", levels: []any{nil, map[string]any{"effort": "unknown"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := map[string]any{
				"supported_reasoning_levels": tt.levels,
				"default_reasoning_level":    "max",
			}
			sanitizeCodexClientReasoningMetadata(entry, tt.version)

			encodedEntry, errMarshal := json.Marshal(entry)
			if errMarshal != nil {
				t.Fatalf("marshal model metadata: %v", errMarshal)
			}
			if got, want := string(encodedEntry), `{"supported_reasoning_levels":[]}`; got != want {
				t.Fatalf("model metadata JSON = %s, want %s", got, want)
			}
		})
	}
}

func buildTestResponse(models []map[string]any) map[string]any {
	return BuildResponse(models, nil, false)
}

func buildTestResponseForClient(
	models []map[string]any,
	clientVersion string,
	enabled bool,
) map[string]any {
	return BuildResponseForClient(models, nil, enabled, clientVersion)
}

func buildTestResponseWithProviders(
	models []map[string]any,
	providersForModel ProvidersForModelFunc,
	optimizeMultiAgentV2 bool,
	clientVersions ...string,
) map[string]any {
	clientVersion := ""
	if len(clientVersions) > 0 {
		clientVersion = clientVersions[0]
	}
	return BuildResponseForClient(models, providersForModel, optimizeMultiAgentV2, clientVersion)
}

func buildTestResponseWithCPACapabilities(
	models []map[string]any,
	providersForModel ProvidersForModelFunc,
	webSearchCapabilityForModel WebSearchCapabilityForModelFunc,
	optimizeMultiAgentV2 bool,
	clientVersion string,
) map[string]any {
	return BuildResponseForClientWithCPACapabilities(
		models,
		providersForModel,
		webSearchCapabilityForModel,
		optimizeMultiAgentV2,
		clientVersion,
	)
}

func codexClientModelsBySlug(t *testing.T, input []map[string]any) map[string]map[string]any {
	t.Helper()
	resp := buildTestResponse(input)
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
