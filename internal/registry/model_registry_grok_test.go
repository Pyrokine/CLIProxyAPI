package registry

import "testing"

func TestGetAvailableModelsPreservesOriginalMetadataAndAvailability(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"openai-client", "openai", []*ModelInfo{
			{ID: "z-model", DisplayName: "Z Model", ContextLength: 1000},
		},
	)
	r.RegisterClient(
		"claude-client", "claude", []*ModelInfo{
			{
				ID: "a-model", DisplayName: "A Model", ContextLength: 2000,
				Thinking: &ThinkingSupport{Levels: []string{"low", "high"}},
			},
		},
	)
	r.RegisterClient("xai-client", "xai", []*ModelInfo{{ID: "x-model"}})
	r.RegisterClient("suspended-client", "xai", []*ModelInfo{{ID: "hidden-model"}})
	r.SuspendClientModel("suspended-client", "hidden-model", "manual")
	models := r.GetAvailableModels("openai")
	if len(models) != 3 {
		t.Fatalf("available model count = %d, want 3", len(models))
	}
	if models[0]["id"] != "a-model" || models[1]["id"] != "x-model" || models[2]["id"] != "z-model" {
		t.Fatalf(
			"model order = [%v, %v, %v], want [a-model, x-model, z-model]",
			models[0]["id"], models[1]["id"], models[2]["id"],
		)
	}
	if models[0]["display_name"] != "A Model" || models[0]["context_length"] != 2000 ||
		models[2]["display_name"] != "Z Model" || models[2]["context_length"] != 1000 {
		t.Fatalf("available metadata = %#v", models)
	}
	thinking, ok := models[0]["thinking"].(*ThinkingSupport)
	if !ok || thinking == nil || len(thinking.Levels) != 2 || thinking.Levels[1] != "high" {
		t.Fatalf("thinking metadata = %#v", models[0]["thinking"])
	}
	for _, model := range models {
		if model["id"] == "hidden-model" {
			t.Fatalf("suspended model returned: %#v", model)
		}
	}
	thinking.Levels[0] = "mutated"
	fresh := r.GetAvailableModels("openai")
	if got := fresh[0]["thinking"].(*ThinkingSupport).Levels[0]; got != "low" {
		t.Fatalf("snapshot was not cloned: %q", got)
	}
	provider := r.GetAvailableModelsByProvider("claude")
	if len(provider) != 1 || provider[0].ID != "a-model" || provider[0].ContextLength != 2000 ||
		provider[0].Thinking == nil || provider[0].Thinking.Levels[0] != "low" {
		t.Fatalf("provider metadata = %#v", provider)
	}
}

func TestGetAvailableModelsHonorsOriginalQuotaAndSuspensionAvailability(t *testing.T) {
	for _, test := range []struct {
		name               string
		clientCount        int
		quotaExceeded      bool
		quotaSuspended     bool
		manualSuspended    bool
		wantModelAvailable bool
	}{
		{name: "quota cooldown remains listed", quotaExceeded: true, wantModelAvailable: true},
		{name: "quota suspension reason remains listed", quotaSuspended: true, wantModelAvailable: true},
		{
			name: "quota and non-quota suspensions are hidden", clientCount: 2,
			quotaExceeded: true, quotaSuspended: true, manualSuspended: true, wantModelAvailable: false,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				const modelID = "shared-model"
				r := newTestModelRegistry()
				r.RegisterClient("quota-client", "openai", []*ModelInfo{{ID: modelID}})
				if test.clientCount > 1 {
					r.RegisterClient("manual-client", "openai", []*ModelInfo{{ID: modelID}})
				}
				if test.quotaExceeded {
					r.SetModelQuotaExceeded("quota-client", modelID)
				}
				if test.quotaSuspended {
					r.SuspendClientModel("quota-client", modelID, "quota")
				}
				if test.manualSuspended {
					r.SuspendClientModel("manual-client", modelID, "manual")
				}
				infos := r.GetAvailableModelsByProvider("openai")
				gotInfoAvailable := len(infos) == 1 && infos[0] != nil && infos[0].ID == modelID
				if gotInfoAvailable != test.wantModelAvailable {
					t.Fatalf(
						"provider list available=%v want=%v models=%#v",
						gotInfoAvailable, test.wantModelAvailable, infos,
					)
				}
				models := r.GetAvailableModels("openai")
				gotListAvailable := len(models) == 1 && models[0]["id"] == modelID
				if gotListAvailable != test.wantModelAvailable {
					t.Fatalf(
						"GetAvailableModels() available=%v want=%v models=%#v",
						gotListAvailable, test.wantModelAvailable, models,
					)
				}
			},
		)
	}
}
