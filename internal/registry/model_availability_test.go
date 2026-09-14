package registry

import "testing"

func TestModelAvailabilitySnapshotUsesActualClientState(t *testing.T) {
	t.Parallel()

	r := newTestModelRegistry()
	model := &ModelInfo{ID: "runtime-model", DisplayName: "Runtime Model"}
	r.RegisterClient("available", "codex", []*ModelInfo{model, model})
	r.RegisterClient("cooldown", "openai", []*ModelInfo{model})
	r.RegisterClient("suspended", "codex", []*ModelInfo{model})
	r.SetModelQuotaExceeded("cooldown", model.ID)
	r.SuspendClientModel("suspended", model.ID, "maintenance")

	snapshot := r.GetModelAvailabilitySnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %#v, want one model", snapshot)
	}
	got := snapshot[0]
	if got.ModelID != model.ID || got.ConfiguredClients != 3 || got.AvailableClients != 1 ||
		got.QuotaCooldownClients != 1 || got.SuspendedClients != 1 {
		t.Fatalf("availability = %#v", got)
	}
	if got.PreferredProvider != "codex" {
		t.Fatalf("PreferredProvider = %q, want codex", got.PreferredProvider)
	}
	if len(got.ConfiguredProviders) != 2 || got.ConfiguredProviders[0] != "codex" || got.ConfiguredProviders[1] != "openai" {
		t.Fatalf("ConfiguredProviders = %#v", got.ConfiguredProviders)
	}
	if len(got.AvailableProviders) != 1 || got.AvailableProviders[0] != "codex" {
		t.Fatalf("AvailableProviders = %#v", got.AvailableProviders)
	}
	if got.Info == nil || got.Info.DisplayName != model.DisplayName {
		t.Fatalf("Info = %#v", got.Info)
	}
}

func TestModelAvailabilitySnapshotTreatsQuotaSuspensionAsCooldown(t *testing.T) {
	t.Parallel()

	r := newTestModelRegistry()
	r.RegisterClient("cooldown", "kimi", []*ModelInfo{{ID: "models/kimi-k3"}})
	r.SuspendClientModel("cooldown", "models/kimi-k3", "quota")

	snapshot := r.GetModelAvailabilitySnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %#v, want one model", snapshot)
	}
	got := snapshot[0]
	if modelAvailabilityKey(got.ModelID) != "kimi-k3" || got.AvailableClients != 0 ||
		got.QuotaCooldownClients != 1 || got.SuspendedClients != 0 || len(got.AvailableProviders) != 0 {
		t.Fatalf("availability = %#v", got)
	}
}
