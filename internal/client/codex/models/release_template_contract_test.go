package models

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCodexClientFinalReleaseTemplateFieldsReachResponse(t *testing.T) {
	var manifest codexClientModelsPayload
	if err := json.Unmarshal(registry.GetCodexClientModelsJSON(), &manifest); err != nil {
		t.Fatal(err)
	}
	templates := make(map[string]map[string]any)
	for _, entry := range manifest.Models {
		templates[entry["slug"].(string)] = entry
	}
	ids := []string{"codex-auto-review", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"}
	available := make([]map[string]any, 0, len(ids))
	registered := make([]*registry.ModelInfo, 0, len(ids))
	for _, id := range ids {
		available = append(available, map[string]any{"id": id})
		info := registry.LookupStaticModelInfoByChannel(id, "codex")
		if info == nil {
			info = &registry.ModelInfo{ID: id, Type: "codex", Object: "model"}
		}
		registered = append(registered, info)
	}
	r := registry.GetGlobalRegistry()
	r.RegisterClient("final-release-template-contract", "codex", registered)
	t.Cleanup(func() { r.UnregisterClient("final-release-template-contract") })
	response := BuildResponseForClient(available, func(string) []string { return []string{"codex"} }, false, "0.159.0")
	entries, ok := response["models"].([]map[string]any)
	if !ok || len(entries) != len(ids) {
		t.Fatalf("response models = %T (%d)", response["models"], len(entries))
	}
	for _, entry := range entries {
		id := entry["slug"].(string)
		template := templates[id]
		for _, key := range []string{
			"base_instructions", "model_messages", "available_in_plans", "available_access_programs",
			"supports_experimental_context", "supports_reasoning_effort_updates", "priority", "description", "upgrade",
		} {
			got, gotPresent := entry[key]
			want, wantPresent := template[key]
			if gotPresent != wantPresent || !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s differs from the dedicated template", id, key)
			}
		}
		for _, key := range []string{"supports_experimental_context", "supports_reasoning_effort_updates"} {
			if value, present := template[key]; !present || value != false {
				t.Errorf("%s.%s = %v, present=%t", id, key, value, present)
			}
		}
		plans, _ := entry["available_in_plans"].([]any)
		for _, plan := range []string{"ent26", "law", "promax"} {
			if !slices.Contains(plans, any(plan)) {
				t.Errorf("%s missing final release plan %s", id, plan)
			}
		}
		if instructions, _ := entry["base_instructions"].(string); len(instructions) < 15000 {
			t.Errorf("%s instructions truncated to %d bytes", id, len(instructions))
		}
		if id == "gpt-5.5" {
			upgrade, _ := entry["upgrade"].(map[string]any)
			if upgrade["model"] != "gpt-5.6-sol" || upgrade["retirement_at"] != "2026-10-14T19:00:00Z" {
				t.Errorf("GPT-5.5 upgrade = %#v", upgrade)
			}
		}
	}
}
