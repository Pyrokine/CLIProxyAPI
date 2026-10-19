package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCodexClientApplyPatchCatalogMatchesNativeRouting(t *testing.T) {
	for _, test := range []struct {
		name      string
		providers []string
		want      any
	}{
		{name: "native", providers: []string{"codex"}, want: "freeform"},
		{name: "mixed", providers: []string{"codex", "gemini"}},
		{name: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := BuildResponseForClient([]map[string]any{{"id": "gpt-6-astra"}}, func(string) []string {
				return test.providers
			}, false, "0.156.0")
			models, ok := response["models"].([]map[string]any)
			if !ok || len(models) != 1 {
				t.Fatalf("models = %#v, want one model", response["models"])
			}
			if got := models[0]["apply_patch_tool_type"]; got != test.want {
				t.Fatalf("apply_patch_tool_type = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestCodexClientMarshalCompactDisablesHTMLEscaping(t *testing.T) {
	body, err := MarshalCompact(map[string]any{"model": "<model>&"})
	if err != nil {
		t.Fatalf("MarshalCompact() error = %v", err)
	}
	if string(body) != `{"model":"<model>&"}` || strings.HasSuffix(string(body), "\n") {
		t.Fatalf("MarshalCompact() = %q, want unescaped single-line JSON", body)
	}
}

func TestCodexClientNonTemplateCatalogFitsClientLimit(t *testing.T) {
	available := []map[string]any{{"id": "gpt-5.5"}}
	for index := range 120 {
		available = append(available, map[string]any{"id": fmt.Sprintf("custom-sync-model-%d", index)})
	}
	response := BuildResponseForClient(available, nil, false, "0.156.0")
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != len(available) {
		t.Fatalf("models count = %d, want %d", len(models), len(available))
	}
	var custom map[string]any
	for _, model := range models {
		if model["slug"] == "custom-sync-model-0" {
			custom = model
			break
		}
	}
	if custom == nil {
		t.Fatal("custom model missing from catalog")
	}
	for _, field := range []string{"apply_patch_tool_type", "upgrade", "availability_nux"} {
		value, exists := custom[field]
		if !exists || value != nil {
			t.Fatalf("custom %s = %#v, want present null", field, value)
		}
	}
	instructions, ok := custom["base_instructions"].(string)
	if !ok || instructions == "" || len(instructions) > 200 {
		t.Fatalf("custom base_instructions length = %d, want compact non-empty instructions", len(instructions))
	}
	messages, ok := custom["model_messages"].(map[string]any)
	if !ok || messages["instructions_template"] != instructions {
		t.Fatalf("custom model_messages = %#v, want compact instructions_template", custom["model_messages"])
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if len(body) >= 1<<20 {
		t.Fatalf("catalog size = %d, want below Codex's 1 MiB limit", len(body))
	}
}

func TestCodexClientCustomInstructionsRemainConfigured(t *testing.T) {
	const instructions = "Configured instructions for this model"
	response := BuildResponseForClient([]map[string]any{{
		"id": "custom-instructions-model", "base_instructions": instructions,
	}}, nil, false, "0.156.0")
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one custom model", response["models"])
	}
	if got := models[0]["base_instructions"]; got != instructions {
		t.Fatalf("base_instructions = %q, want configured instructions", got)
	}
	messages, ok := models[0]["model_messages"].(map[string]any)
	if !ok || messages["instructions_template"] != instructions {
		t.Fatalf("model_messages = %#v, want configured instructions_template", models[0]["model_messages"])
	}
}

func TestCodexClientDevinDisplayName(t *testing.T) {
	response := BuildResponseForClient([]map[string]any{
		{"id": "devin/swe-2", "display_name": "SWE-2"},
		{"id": "tenant/devin/swe-3", "display_name": "SWE-3 (devin)"},
		{"id": "test-devin-provider-model", "display_name": "Alias"},
		{"id": "test-standard-provider-model", "display_name": "Standard"},
	}, func(id string) []string {
		if id == "test-devin-provider-model" {
			return []string{"devin"}
		}
		return []string{"openai"}
	}, false, "0.156.0")
	models, ok := response["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models = %#v", response["models"])
	}
	want := map[string]string{
		"devin/swe-2":                  "SWE-2 (Devin)",
		"tenant/devin/swe-3":           "SWE-3 (Devin)",
		"test-devin-provider-model":    "Alias (Devin)",
		"test-standard-provider-model": "Standard",
	}
	for _, model := range models {
		slug, _ := model["slug"].(string)
		if expected, found := want[slug]; found {
			if got := model["display_name"]; got != expected {
				t.Fatalf("%s display_name = %q, want %q", slug, got, expected)
			}
			delete(want, slug)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing models: %v", want)
	}
}
