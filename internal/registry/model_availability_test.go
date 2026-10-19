package registry

import (
	"testing"
	"time"
)

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

func TestCredentialQuotaOverlapKeepsHealthyModelAvailable(t *testing.T) {
	r := newTestModelRegistry()
	model := &ModelInfo{ID: "shared-model", OwnedBy: "openai", Type: "openai"}
	r.RegisterClient("healthy-key", "codex", []*ModelInfo{model})
	r.RegisterClient("exhausted-oauth", "codex", []*ModelInfo{model})
	_, epoch := r.GetModelsAndEpochForClient("exhausted-oauth")
	if !r.ApplyClientModelProjections("exhausted-oauth", epoch, 1, []ClientModelProjection{{
		ModelID: model.ID, Suspended: true, SuspendReason: "credential_quota", QuotaExceeded: true,
	}}) {
		t.Fatal("credential quota projection was rejected")
	}
	if got := len(r.GetAvailableModels("openai")); got != 1 {
		t.Fatalf("public model count = %d, want 1 healthy route", got)
	}
	if got := len(r.GetAvailableModelsByProvider("codex")); got != 1 {
		t.Fatalf("Codex model count = %d, want 1 healthy route", got)
	}
	if got := r.getModelCount(model.ID); got != 1 {
		t.Fatalf("healthy route count = %d, want 1", got)
	}
	r.SuspendClientModel("healthy-key", model.ID, "maintenance")
	if got := len(r.GetAvailableModels("openai")); got != 0 {
		t.Fatalf("public model count after maintenance = %d, want 0", got)
	}
	if got := len(r.GetAvailableModelsByProvider("codex")); got != 0 {
		t.Fatalf("Codex model count after maintenance = %d, want 0", got)
	}
	if got := r.getModelCount(model.ID); got != 0 {
		t.Fatalf("route count after maintenance = %d, want 0", got)
	}
}

func TestCredentialQuotaRefreshKeepsHealthyModelAvailable(t *testing.T) {
	r := newTestModelRegistry()
	model := &ModelInfo{ID: "shared-refresh-model", OwnedBy: "openai", Type: "openai"}
	r.RegisterClient("healthy-key", "codex", []*ModelInfo{model})
	r.RegisterClient("exhausted-oauth", "codex", []*ModelInfo{model})
	projection := []ClientModelProjection{{
		ModelID: model.ID, Suspended: true, SuspendReason: "credential_quota", QuotaExceeded: true,
	}}
	checkAvailable := func(phase string) {
		t.Helper()
		if got := len(r.GetAvailableModels("openai")); got != 1 {
			t.Fatalf("%s: public model count = %d, want 1", phase, got)
		}
		if got := len(r.GetAvailableModelsByProvider("codex")); got != 1 {
			t.Fatalf("%s: provider model count = %d, want 1", phase, got)
		}
		if got := r.getModelCount(model.ID); got != 1 {
			t.Fatalf("%s: healthy route count = %d, want 1", phase, got)
		}
	}
	_, epoch := r.GetModelsAndEpochForClient("exhausted-oauth")
	if !r.ApplyClientModelProjections("exhausted-oauth", epoch, 1, projection) {
		t.Fatal("initial credential quota projection was rejected")
	}
	checkAvailable("initial")

	r.mutex.Lock()
	old := time.Now().Add(-6 * time.Minute)
	r.models[model.ID].QuotaExceededClients["exhausted-oauth"] = &old
	r.mutex.Unlock()
	checkAvailable("quota window expired")

	r.RegisterClient("exhausted-oauth", "codex", []*ModelInfo{model})
	_, epoch = r.GetModelsAndEpochForClient("exhausted-oauth")
	if !r.ApplyClientModelProjections("exhausted-oauth", epoch, 2, projection) {
		t.Fatal("refreshed credential quota projection was rejected")
	}
	checkAvailable("projection refreshed")
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
