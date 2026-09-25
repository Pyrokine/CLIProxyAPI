package registry

import (
	"sync"
	"testing"
)

func TestModelRegistryProjectionGenerationOrdersAtomicBatch(t *testing.T) {
	r := newTestModelRegistry()
	clientID := "projection-generation-client"
	canonicalModel := "projection-canonical"
	routeModel := "projection-route"
	r.RegisterClient(clientID, "codex", []*ModelInfo{
		{ID: canonicalModel},
		{ID: routeModel},
	})

	models, epoch := r.GetModelsAndEpochForClient(clientID)
	if epoch != 1 || len(models) != 2 {
		t.Fatalf("GetModelsAndEpochForClient() = (%#v, %d), want two models at epoch 1", models, epoch)
	}
	if !r.ApplyClientModelProjections(clientID, epoch, 10, []ClientModelProjection{
		{ModelID: canonicalModel, Suspended: true, SuspendReason: "quota", QuotaExceeded: true},
		{ModelID: routeModel, Suspended: true, SuspendReason: "quota", QuotaExceeded: true},
	}) {
		t.Fatal("ApplyClientModelProjections() rejected current epoch and generation")
	}
	for _, modelID := range []string{canonicalModel, routeModel} {
		if !r.IsModelSuspendedForClient(clientID, modelID) ||
			!r.IsModelQuotaExceededForClient(clientID, modelID) {
			t.Fatalf("model %q did not receive the atomic projection", modelID)
		}
	}

	if r.ApplyClientModelProjections(clientID, epoch, 9, []ClientModelProjection{
		{ModelID: canonicalModel},
		{ModelID: routeModel},
	}) {
		t.Fatal("ApplyClientModelProjections() accepted a stale generation")
	}
	for _, modelID := range []string{canonicalModel, routeModel} {
		if !r.IsModelSuspendedForClient(clientID, modelID) ||
			!r.IsModelQuotaExceededForClient(clientID, modelID) {
			t.Fatalf("stale generation changed model %q", modelID)
		}
	}

	if !r.ApplyClientModelProjections(clientID, epoch, 11, []ClientModelProjection{
		{ModelID: canonicalModel},
		{ModelID: routeModel},
	}) {
		t.Fatal("ApplyClientModelProjections() rejected a newer generation")
	}
	for _, modelID := range []string{canonicalModel, routeModel} {
		if r.IsModelSuspendedForClient(clientID, modelID) ||
			r.IsModelQuotaExceededForClient(clientID, modelID) {
			t.Fatalf("newer generation did not clear model %q", modelID)
		}
	}
}

func TestModelRegistryProjectionRejectsOldEpochAfterReregistration(t *testing.T) {
	r := newTestModelRegistry()
	clientID := "projection-epoch-client"
	modelID := "projection-epoch-model"
	r.RegisterClient(clientID, "codex", []*ModelInfo{{ID: modelID}})
	epoch1 := r.ClientRegistrationEpoch(clientID)
	if !r.ApplyClientModelProjections(clientID, epoch1, 100, []ClientModelProjection{
		{ModelID: modelID, Suspended: true, SuspendReason: "old"},
	}) {
		t.Fatal("initial projection was rejected")
	}

	r.UnregisterClient(clientID)
	epoch2 := r.ClientRegistrationEpoch(clientID)
	if epoch2 <= epoch1 {
		t.Fatalf("unregister epoch = %d, want greater than %d", epoch2, epoch1)
	}
	r.RegisterClient(clientID, "codex", []*ModelInfo{{ID: modelID}})
	epoch3 := r.ClientRegistrationEpoch(clientID)
	if epoch3 <= epoch2 {
		t.Fatalf("re-register epoch = %d, want greater than %d", epoch3, epoch2)
	}

	if r.ApplyClientModelProjections(clientID, epoch1, 99999, []ClientModelProjection{
		{ModelID: modelID, Suspended: true, SuspendReason: "stale"},
	}) {
		t.Fatal("old registration epoch overrode the current registration")
	}
	if r.IsModelSuspendedForClient(clientID, modelID) {
		t.Fatal("old registration epoch restored stale suspension")
	}
	if !r.ApplyClientModelProjections(clientID, epoch3, 1, []ClientModelProjection{
		{ModelID: modelID, Suspended: true, SuspendReason: "current"},
	}) {
		t.Fatal("current registration epoch was rejected")
	}
	if !r.IsModelSuspendedForClient(clientID, modelID) {
		t.Fatal("current registration epoch did not publish suspension")
	}
}

func TestModelRegistryProjectionConcurrentGenerationsKeepNewest(t *testing.T) {
	r := newTestModelRegistry()
	clientID := "projection-concurrent-client"
	modelID := "projection-concurrent-model"
	r.RegisterClient(clientID, "codex", []*ModelInfo{{ID: modelID}})
	epoch := r.ClientRegistrationEpoch(clientID)

	var wg sync.WaitGroup
	for generation := uint64(1); generation <= 64; generation++ {
		generation := generation
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ApplyClientModelProjections(clientID, epoch, generation, []ClientModelProjection{
				{
					ModelID:       modelID,
					Suspended:     generation%2 == 0,
					SuspendReason: "newest",
					QuotaExceeded: generation%3 == 1,
				},
			})
		}()
	}
	wg.Wait()

	if !r.IsModelSuspendedForClient(clientID, modelID) ||
		!r.IsModelQuotaExceededForClient(clientID, modelID) {
		t.Fatal("a stale concurrent generation overrode generation 64")
	}
}

func TestModelRegistryProjectionRejectsInvalidBatchWithoutPartialMutation(t *testing.T) {
	r := newTestModelRegistry()
	clientID := "projection-validation-client"
	modelID := "projection-owned-model"
	r.RegisterClient(clientID, "openai", []*ModelInfo{{ID: modelID}})
	epoch := r.ClientRegistrationEpoch(clientID)

	for _, test := range []struct {
		name        string
		projections []ClientModelProjection
	}{
		{
			name: "unregistered model",
			projections: []ClientModelProjection{
				{ModelID: modelID, Suspended: true},
				{ModelID: "projection-unowned-model", Suspended: true},
			},
		},
		{
			name: "duplicate model",
			projections: []ClientModelProjection{
				{ModelID: modelID, Suspended: true},
				{ModelID: modelID, QuotaExceeded: true},
			},
		},
		{
			name:        "empty batch",
			projections: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if r.ApplyClientModelProjections(clientID, epoch, 5, test.projections) {
				t.Fatal("ApplyClientModelProjections() accepted an invalid batch")
			}
			if r.IsModelSuspendedForClient(clientID, modelID) ||
				r.IsModelQuotaExceededForClient(clientID, modelID) {
				t.Fatal("invalid batch partially changed the registered model")
			}
		})
	}

	if !r.ApplyClientModelProjections(clientID, epoch, 5, []ClientModelProjection{
		{ModelID: modelID, Suspended: true, QuotaExceeded: true},
	}) {
		t.Fatal("invalid batch advanced generation and blocked a valid batch")
	}
}
