package models

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCodexClientHomeRoutesIntersectExplicitCapabilities(t *testing.T) {
	templates, _, err := loadCodexClientModelTemplates()
	if err != nil {
		t.Fatal(err)
	}
	templateIDs := make([]string, 0, len(templates))
	for id := range templates {
		templateIDs = append(templateIDs, id)
	}
	slices.Sort(templateIDs)
	if len(templateIDs) == 0 {
		t.Fatal("model templates are empty")
	}
	for _, id := range []string{"home-multi-route-capability-fixture", templateIDs[0]} {
		for _, testCase := range []struct {
			name       string
			modalities []string
			thinking   *registry.ThinkingSupport
			wantModes  []string
			wantLevels []string
		}{
			{
				name: "shared capabilities", modalities: []string{"text"},
				thinking: &registry.ThinkingSupport{Levels: []string{"low"}}, wantModes: []string{"text"},
				wantLevels: []string{"low"},
			},
			{
				name: "no common modalities", modalities: []string{"audio"},
				thinking: &registry.ThinkingSupport{Levels: []string{"low"}}, wantLevels: []string{"low"},
			},
			{name: "explicitly no reasoning", modalities: []string{"text"}, wantModes: []string{"text"}},
		} {
			for _, reverse := range []bool{false, true} {
				t.Run(
					id+"/"+testCase.name+map[bool]string{false: "/forward", true: "/reverse"}[reverse],
					func(t *testing.T) {
						first := map[string]any{
							"id":                       id,
							"display_name":             "first route",
							"supportedInputModalities": []string{"text", "image"},
							"thinking":                 &registry.ThinkingSupport{Levels: []string{"low", "high"}},
						}
						second := map[string]any{
							"id":                       id,
							"display_name":             "second route",
							"supportedInputModalities": testCase.modalities,
							"thinking":                 testCase.thinking,
						}
						routes := []map[string]any{first, second}
						if reverse {
							routes[0], routes[1] = routes[1], routes[0]
						}
						before, err := json.Marshal(routes)
						if err != nil {
							t.Fatal(err)
						}
						response := BuildResponseForClient(routes, nil, false, "cpa")
						entries, ok := response["models"].([]map[string]any)
						if !ok || len(entries) != 1 {
							t.Fatalf("Home routes did not produce one public model: %#v", response)
						}
						assertCodexClientModalities(t, entries[0], testCase.wantModes)
						assertCodexClientReasoningLevels(t, entries[0], testCase.wantLevels)
						if got := entries[0]["display_name"]; got != routes[0]["display_name"] {
							t.Fatalf("display name = %v, want first route %v", got, routes[0]["display_name"])
						}
						after, err := json.Marshal(routes)
						if err != nil {
							t.Fatal(err)
						}
						if string(before) != string(after) {
							t.Fatalf("client model construction mutated Home routes: %s", after)
						}
					},
				)
			}
		}
	}
}
