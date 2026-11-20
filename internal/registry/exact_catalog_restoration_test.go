package registry

import (
	"encoding/json"
	"testing"
)

func TestCatalogRestoresExactSolAndAntigravityModels(t *testing.T) {
	for tier, models := range map[string][]*ModelInfo{
		"codex-team": GetCodexTeamModels(), "codex-plus": GetCodexPlusModels(), "codex-pro": GetCodexProModels(),
	} {
		model := findModelByID(models, "gpt-6.1-sol")
		if model == nil || model.ContextLength != 500000 || model.MaxCompletionTokens != 128000 {
			t.Fatalf("Sol %s limits = %+v", tier, model)
		}
	}
	var templates codexClientModelsPayload
	if err := json.Unmarshal(GetCodexClientModelsJSON(), &templates); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range templates.Models {
		if model["slug"] == "gpt-6.1-sol" {
			found = true
			if model["prefer_websockets"] != true || model["use_responses_lite"] != true ||
				model["minimal_client_version"] != "0.153.0" {
				t.Fatalf("Sol dedicated template fields changed")
			}
		}
	}
	if !found {
		t.Fatal("dedicated Sol client template missing")
	}
	for alias, id := range map[string]string{
		"claude-opus-5-5-high":     "claude-opus-5-5-high",
		"claude-sonnet-5-5-high":   "claude-sonnet-5-5-high",
		"claude-opus-4-6-thinking": "claude-opus-5-5-high",
		"claude-opus-5-5-thinking": "claude-opus-5-5-high",
		"claude-sonnet-4-6":        "claude-sonnet-5-5-high",
		"claude-sonnet-5-5":        "claude-sonnet-5-5-high",
	} {
		model := LookupStaticModelInfoByChannel(alias, "antigravity")
		if model == nil || model.ID != id || model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 ||
			model.InputTokenLimit != 1000000 ||
			model.OutputTokenLimit != 128000 {
			t.Fatalf("Antigravity alias %s = %+v", alias, model)
		}
	}
	if model := LookupStaticModelInfoByChannel(
		"claude-sonnet-5-5", "claude",
	); model == nil || model.ID != "claude-sonnet-5-5" {
		t.Fatalf("Antigravity alias changed native Claude identity: %+v", model)
	}
}

func TestCatalogImage20ReleaseAndAntigravitySearchDefaults(t *testing.T) {
	model := LookupStaticModelInfoByChannel("grok-imagine-image-2.0", "xai")
	if model == nil || model.Created != 1786060800 {
		t.Fatalf("Image 2.0 release timestamp = %+v", model)
	}
	for _, model := range getAntigravityModels() {
		if model.NativeCapabilities != nil && model.NativeCapabilities.WebSearch != nil || model.SupportsWebSearch {
			t.Fatalf("static Antigravity search must retain unknown evidence state: %s", model.ID)
		}
	}
}
