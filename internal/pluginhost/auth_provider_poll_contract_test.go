package pluginhost

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestPollLoginRejectsRetiredOwnerBeforeNativeCall(t *testing.T) {
	plugin := validTestPlugin("alpha")
	plugin.Capabilities.AuthProvider = fakeAuthProvider{
		identifier: "plugin-provider",
		pollLogin: func(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
		},
	}
	loader := newTestSymbolLoader()
	loader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: plugin, reconfigureResult: plugin})
	host := newForTest(loader)
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	if err := host.ApplyConfig(t.Context(), pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1)); err != nil {
		t.Fatal(err)
	}
	record := host.authProviderRecord("plugin-provider")
	if record == nil {
		t.Fatal("auth provider was not published")
	}
	if !host.UnloadPlugin("alpha") {
		t.Fatal("auth provider was not unloaded")
	}
	if _, handled, err := host.callPollLogin(
		t.Context(), *record, "plugin-provider", "state", nil,
	); err != nil || handled {
		t.Fatalf("retired poll was dispatched: handled = %t, error = %v", handled, err)
	}
}

func TestPollLoginMetadataIsIndependent(t *testing.T) {
	metadata := map[string]any{"nested": map[string]any{"scopes": []string{"read", "write"}}}
	host := newHostWithRecords(
		capabilityRecord{
			id: "alpha",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "plugin-provider",
						pollLogin: func(
							_ context.Context,
							req pluginapi.AuthLoginPollRequest,
						) (pluginapi.AuthLoginPollResponse, error) {
							if req.Provider != "plugin-provider" || req.State != "state" {
								t.Fatalf("poll identity = %q/%q", req.Provider, req.State)
							}
							req.Metadata["nested"].(map[string]any)["scopes"].([]string)[0] = "changed"
							return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
						},
					},
				},
			},
		},
	)
	if _, handled, err := host.PollLogin(t.Context(), "plugin-provider", "state", metadata); err != nil || !handled {
		t.Fatalf("poll result: handled = %t, error = %v", handled, err)
	}
	if got := metadata["nested"].(map[string]any)["scopes"].([]string)[0]; got != "read" {
		t.Fatalf("plugin mutated caller metadata: %q", got)
	}
}
