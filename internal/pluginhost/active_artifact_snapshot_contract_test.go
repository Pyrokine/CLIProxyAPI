package pluginhost

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestRegisterPluginCapabilitiesUsesActiveArtifactSnapshot(t *testing.T) {
	for _, capability := range []string{"models", "executor"} {
		for _, state := range []string{
			"active", "different path", "different active version", "different file version", "removed",
		} {
			t.Run(
				fmt.Sprintf("%s/%s", capability, state), func(t *testing.T) {
					calls := 0
					record := capabilityRecord{
						id: "artifact-plugin", path: "testdata/artifact.plugin", version: "1.0.0",
						meta: pluginapi.Metadata{Name: "artifact-plugin", Version: "1.0.0"},
					}
					if capability == "models" {
						record.plugin.Capabilities.ModelRegistrar = modelRegistrarFunc(
							func(
								context.Context, pluginapi.ModelRegistrationRequest,
							) (pluginapi.ModelRegistrationResponse, error) {
								calls++
								return pluginapi.ModelRegistrationResponse{
									Provider: "artifact-provider",
									Models:   []pluginapi.ModelInfo{{ID: "artifact-model"}},
								}, nil
							},
						)
					} else {
						record.plugin.Capabilities.Executor = &fakeExecutor{
							identifierFunc: func() string {
								calls++
								return "artifact-provider"
							},
						}
					}
					host := newHostWithRecords(record)
					host.mu.Lock()
					host.rebuildActivePluginMapsLocked([]capabilityRecord{record})
					switch state {
					case "different path":
						host.activePluginPaths[record.id] = "testdata/other-artifact.plugin"
					case "different active version":
						host.activePluginVersions[record.id] = "2.0.0"
					case "different file version":
						host.pluginFileVersions[cleanPluginPath(record.path)] = "2.0.0"
					case "removed":
						host.rebuildActivePluginMapsLocked(nil)
					}
					host.mu.Unlock()
					models := newFakeModelRegistry()
					manager := newFakeExecutorManager()
					published := false
					if capability == "models" {
						host.RegisterModels(t.Context(), models)
						_, published = models.clients["plugin:artifact-plugin:artifact-provider"]
					} else {
						host.RegisterExecutors(manager, models)
						_, published = manager.Executor("artifact-provider")
					}
					want := state == "active"
					if published != want || (calls > 0) != want {
						t.Fatalf("published=%t callback calls=%d, want active=%t", published, calls, want)
					}
				},
			)
		}
	}
}
