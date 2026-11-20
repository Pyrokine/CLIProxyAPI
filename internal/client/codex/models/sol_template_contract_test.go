package models

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCodexClientSolUsesDedicatedTemplate(t *testing.T) {
	model := registry.LookupStaticModelInfoByChannel("gpt-6.1-sol", "codex")
	if model == nil {
		t.Fatal("Sol static entry missing")
	}
	r := registry.GetGlobalRegistry()
	r.RegisterClient("sol-template-contract", "codex", []*registry.ModelInfo{model})
	t.Cleanup(func() { r.UnregisterClient("sol-template-contract") })
	response := BuildResponseForClient(
		[]map[string]any{{"id": "gpt-6.1-sol"}}, func(string) []string { return []string{"codex"} }, false, "0.159.0",
	)
	entries := response["models"].([]map[string]any)
	if len(entries) != 1 {
		t.Fatalf("Sol entries = %d", len(entries))
	}
	entry := entries[0]
	if entry["slug"] != "gpt-6.1-sol" || entry["prefer_websockets"] != true || entry["use_responses_lite"] != true ||
		entry["minimal_client_version"] != "0.153.0" {
		t.Fatalf("Sol dedicated capability fields were replaced")
	}
	instructions, _ := entry["base_instructions"].(string)
	if len(instructions) <= 200 || len(entry["service_tiers"].([]any)) == 0 {
		t.Fatalf(
			"Sol instructions or Fast tier lost: instructions=%d tiers=%v", len(instructions), entry["service_tiers"],
		)
	}
}
