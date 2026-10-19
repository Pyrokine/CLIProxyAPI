package cliproxy

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestExternalModelRefreshRetainsQuotaUntilCredentialChanges(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		source   modelDiscoverySource
	}{
		{"antigravity", "antigravity", modelDiscoverySourceAntigravity},
		{"plugin", "openai-compatibility", modelDiscoverySourcePlugin},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := &ModelInfo{ID: "retained-" + test.name}
			recoverAt := time.Now().Add(time.Hour)
			auth := &coreauth.Auth{
				ID: "refresh-retains-quota-" + test.name, Provider: test.provider, Status: coreauth.StatusActive,
				ModelStates: map[string]*coreauth.ModelState{model.ID: {
					Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: recoverAt,
					Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: recoverAt},
				}},
			}
			service := newModelRefreshTestService(t, &config.Config{}, auth)
			modelRegistry := registry.GetGlobalRegistry()
			entry := modelRefreshCacheEntry{
				source: test.source, outcome: modelDiscoveryAuthoritativePositive,
				provider: test.provider, models: []*ModelInfo{model, {ID: "removed-" + test.name}},
			}
			service.projectModelDiscoveryLocked(auth, service.cfg, entry, modelRegistry.ClientRegistrationEpoch(auth.ID))
			if !modelRegistry.ClientSupportsModel(auth.ID, model.ID) {
				t.Fatal("initial discovery did not publish the model")
			}
			if !modelRegistry.IsModelSuspendedForClient(auth.ID, model.ID) || !modelRegistry.IsModelQuotaExceededForClient(auth.ID, model.ID) {
				t.Fatal("initial discovery did not project credential cooldown and quota")
			}
			entry.models = []*ModelInfo{model, {ID: "added-" + test.name}}
			service.projectModelDiscoveryLocked(auth, service.cfg, entry, modelRegistry.ClientRegistrationEpoch(auth.ID))
			if !modelRegistry.IsModelSuspendedForClient(auth.ID, model.ID) || !modelRegistry.IsModelQuotaExceededForClient(auth.ID, model.ID) {
				t.Fatal("catalog refresh cleared credential cooldown or quota")
			}
			if modelRegistry.ClientSupportsModel(auth.ID, "removed-"+test.name) ||
				!modelRegistry.ClientSupportsModel(auth.ID, "added-"+test.name) {
				t.Fatal("catalog refresh did not replace the model set")
			}
			modelRegistry.RegisterClient(auth.ID, test.provider, toRegistryModelInfos(entry.models))
			if modelRegistry.IsModelSuspendedForClient(auth.ID, model.ID) || modelRegistry.IsModelQuotaExceededForClient(auth.ID, model.ID) {
				t.Fatal("ordinary credential registration did not reset scheduling state")
			}
			staleEpoch := modelRegistry.ClientRegistrationEpoch(auth.ID)
			modelRegistry.RegisterClient(auth.ID, test.provider, []*registry.ModelInfo{{ID: "new-auth-" + test.name}})
			var applied bool
			if test.source == modelDiscoverySourceAntigravity {
				applied = service.registerAntigravityModels(auth, service.cfg, entry.models, staleEpoch)
			} else {
				applied = service.registerPluginDiscoveredModels(auth, service.cfg, test.provider, entry.models, staleEpoch)
			}
			if service.projectModelDiscoveryLocked(auth, service.cfg, entry, staleEpoch) {
				t.Fatal("discovery projection accepted a stale registration epoch")
			}
			if applied || !modelRegistry.ClientSupportsModel(auth.ID, "new-auth-"+test.name) ||
				modelRegistry.ClientSupportsModel(auth.ID, model.ID) {
				t.Fatal("stale discovery replaced the newer credential's model set")
			}
		})
	}
}
