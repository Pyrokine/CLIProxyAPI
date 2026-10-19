package models

import "testing"

func TestCodexClientApplyPatchCapabilityUsesTextMetadata(t *testing.T) {
	for _, tc := range []struct {
		id         string
		modalities any
		want       bool
	}{
		{id: "gpt-6-sol", modalities: []string{"text", "image"}, want: true},
		{id: "fixture-model", modalities: []any{"text"}, want: true},
		{id: "hidden-text", modalities: []string{"text"}, want: true},
		{id: "audio-only", modalities: []string{"audio"}},
		{id: "unknown"},
		{id: "empty", modalities: []string{}},
		{id: "gpt-image-2.5-flare", modalities: []string{"text"}},
		{id: "prefix/gpt-image-2.5-sunburst", modalities: []string{"text"}},
		{id: "a/b/GROK-IMAGINE-IMAGE-2.0", modalities: []string{"text"}},
		{id: "grok-imagine-video-1.5-preview", modalities: []string{"text"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			entry := map[string]any{"input_modalities": tc.modalities, "visibility": "hide", "apply_patch_tool_type": "freeform"}
			calls := 0
			applyCodexClientApplyPatchCapability(entry, tc.id, func(id string) bool {
				calls++
				if id != tc.id {
					t.Fatalf("lookup ID = %q, want %q", id, tc.id)
				}
				return true
			})
			if got := entry["apply_patch_tool_type"]; tc.want && got != "freeform" || !tc.want && got != nil {
				t.Fatalf("capability = %#v, want enabled=%t", got, tc.want)
			}
			if tc.want && calls != 1 || !tc.want && calls != 0 {
				t.Fatalf("lookup calls = %d, want enabled=%t", calls, tc.want)
			}
		})
	}
}

func TestCodexClientApplyPatchCapabilityCoversCatalogPaths(t *testing.T) {
	for _, id := range []string{"gpt-6-sol", "prefix/gpt-6-sol", "fixture-alias"} {
		for _, enabled := range []bool{false, true} {
			payload := BuildResponseForClientWithCPACapabilities(
				[]map[string]any{{"id": id, "input_modalities": []string{"text"}}},
				func(string) []string { return []string{"fixture-provider"} },
				nil, false, "cpa", func(model string) bool { return enabled && model == id },
			)
			entries, ok := payload["models"].([]map[string]any)
			if !ok || len(entries) != 1 {
				t.Fatalf("catalog = %#v", payload)
			}
			got, exists := entries[0]["apply_patch_tool_type"]
			if !exists || enabled && got != "freeform" || !enabled && got != nil {
				t.Fatalf("%s enabled=%t capability=%#v exists=%t", id, enabled, got, exists)
			}
		}
	}
	legacy := BuildResponseForClient([]map[string]any{{"id": "gpt-6-sol"}}, func(string) []string { return []string{"codex"} }, false, "0.157.0")
	if got := legacy["models"].([]map[string]any)[0]["apply_patch_tool_type"]; got != "freeform" {
		t.Fatalf("legacy builder behavior = %#v, want freeform", got)
	}
}
