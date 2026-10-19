package cliproxy

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestModelRegistryAdapterPreservesConfigurationUpdateCapability(t *testing.T) {
	const clientID = "sdk-configuration-update-capability"
	modelRegistry := GlobalModelRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	modelRegistry.RegisterClient(clientID, "codex", []*ModelInfo{
		{ID: "enabled", SupportConfigurationUpdate: true},
		{ID: "disabled", SupportConfigurationUpdate: false},
	})
	for _, test := range []struct {
		id      string
		enabled bool
	}{
		{id: "enabled", enabled: true},
		{id: "disabled", enabled: false},
	} {
		internal := registry.GetGlobalRegistry().GetModelInfoForClient(clientID, test.id)
		if internal == nil || internal.SupportConfigurationUpdate != test.enabled {
			t.Errorf("internal model %q = %+v, want capability %t", test.id, internal, test.enabled)
		}
		models := modelRegistry.GetModelsForClient(clientID)
		found := false
		for _, model := range models {
			if model.ID == test.id {
				found = true
				if model.SupportConfigurationUpdate != test.enabled {
					t.Errorf("SDK model %q capability = %t, want %t", test.id, model.SupportConfigurationUpdate, test.enabled)
				}
			}
		}
		if !found {
			t.Errorf("SDK model %q not returned", test.id)
		}
	}
}

func TestModelRegistryAdapterPublishesVersionedProjection(t *testing.T) {
	clientID := "sdk-projection-client"
	modelID := "sdk-projection-model"
	modelRegistry := GlobalModelRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	modelRegistry.RegisterClient(clientID, "codex", []*ModelInfo{{ID: modelID}})
	models, epoch := modelRegistry.GetModelsAndEpochForClient(clientID)
	if len(models) != 1 || models[0] == nil || models[0].ID != modelID || epoch == 0 {
		t.Fatalf("GetModelsAndEpochForClient() = (%#v, %d)", models, epoch)
	}
	if modelRegistry.ClientRegistrationEpoch(clientID) != epoch {
		t.Fatalf("ClientRegistrationEpoch() != snapshot epoch %d", epoch)
	}
	if !modelRegistry.ApplyClientModelProjections(clientID, epoch, 1, []ClientModelProjection{
		{ModelID: modelID, Suspended: true, SuspendReason: "quota", QuotaExceeded: true},
	}) {
		t.Fatal("ApplyClientModelProjections() rejected adapter projection")
	}

	inner := registry.GetGlobalRegistry()
	if !inner.IsModelSuspendedForClient(clientID, modelID) ||
		!inner.IsModelQuotaExceededForClient(clientID, modelID) {
		t.Fatal("adapter projection did not reach the internal registry")
	}
}
