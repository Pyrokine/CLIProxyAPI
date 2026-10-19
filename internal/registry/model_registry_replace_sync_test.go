package registry

import "testing"

func TestReplaceClientModelsPreservesSchedulingStateAndGeneration(t *testing.T) {
	r := newTestModelRegistry()
	const client = "replacement-client"
	r.RegisterClient(client, "antigravity", []*ModelInfo{{ID: "keep"}, {ID: "keep"}, {ID: "remove"}})
	epoch := r.ClientRegistrationEpoch(client)
	if !r.ApplyClientModelProjections(client, epoch, 11, []ClientModelProjection{{
		ModelID: "keep", QuotaExceeded: true, Suspended: true, SuspendReason: "rate_limit",
	}}) {
		t.Fatal("initial scheduling state was rejected")
	}
	currentEpoch, applied := r.ReplaceClientModels(client, "antigravity", epoch, []*ModelInfo{{ID: "keep"}, {ID: "add"}})
	if !applied || currentEpoch <= epoch {
		t.Fatalf("replacement = (%d, %t), want newer epoch", currentEpoch, applied)
	}
	if !r.IsModelQuotaExceededForClient(client, "keep") || !r.IsModelSuspendedForClient(client, "keep") {
		t.Fatal("replacement lost scheduling state for a retained model")
	}
	if r.ClientSupportsModel(client, "remove") || !r.ClientSupportsModel(client, "add") {
		t.Fatal("replacement published the wrong model set")
	}
	if r.ApplyClientModelProjections(client, currentEpoch, 10, []ClientModelProjection{{ModelID: "keep"}}) {
		t.Fatal("older auth projection was accepted after a catalog refresh")
	}
	if !r.ApplyClientModelProjections(client, currentEpoch, 12, []ClientModelProjection{{ModelID: "keep"}}) {
		t.Fatal("newer auth projection was rejected")
	}
	if r.IsModelQuotaExceededForClient(client, "keep") || r.IsModelSuspendedForClient(client, "keep") {
		t.Fatal("newer auth projection did not clear scheduling state")
	}
	if _, applied := r.ReplaceClientModels(client, "antigravity", epoch, []*ModelInfo{{ID: "remove"}}); applied {
		t.Fatal("stale catalog refresh replaced current models")
	}
}

func TestReplaceClientModelsEmptyAndProviderChange(t *testing.T) {
	r := newTestModelRegistry()
	const client = "replacement-provider-client"
	r.RegisterClient(client, "antigravity", []*ModelInfo{{ID: "keep"}})
	epoch := r.ClientRegistrationEpoch(client)
	r.SuspendClientModel(client, "keep", "rate_limit")
	r.SetModelQuotaExceeded(client, "keep")
	if _, applied := r.ReplaceClientModels(client, "gemini", epoch, []*ModelInfo{{ID: "keep"}}); !applied {
		t.Fatal("provider change was rejected")
	}
	if r.IsModelSuspendedForClient(client, "keep") || r.IsModelQuotaExceededForClient(client, "keep") {
		t.Fatal("new provider retained old provider state")
	}
	epoch = r.ClientRegistrationEpoch(client)
	if _, applied := r.ReplaceClientModels(client, "gemini", epoch, nil); !applied {
		t.Fatal("authoritative empty catalog was rejected")
	}
	if r.ClientSupportsModel(client, "keep") {
		t.Fatal("empty catalog did not remove model")
	}
	if _, applied := r.ReplaceClientModels(client, "gemini", epoch, []*ModelInfo{{ID: "keep"}}); applied {
		t.Fatal("stale catalog resurrected a removed model")
	}
	epoch = r.ClientRegistrationEpoch(client)
	if _, applied := r.ReplaceClientModels(client, "gemini", epoch, []*ModelInfo{{ID: "keep"}}); !applied {
		t.Fatal("fresh catalog could not restore a model")
	}
}
