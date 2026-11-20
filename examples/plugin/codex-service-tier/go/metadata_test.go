package main

import (
	"encoding/json"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestFastConfigurationYAMLCompatibility(t *testing.T) {
	previous := fastEnabled.Load()
	t.Cleanup(func() { fastEnabled.Store(previous) })
	for _, tc := range []struct {
		name, config string
		enabled      bool
	}{
		{name: "empty reconfigure"},
		{name: "explicit false", config: "fast: false\n"},
		{name: "explicit true", config: "fast: true\n", enabled: true},
		{name: "legacy boolean", config: "fast: yes\n", enabled: true},
		{
			name: "merged host configuration",
			config: "defaults: &defaults {fast: true}\n<<: *defaults\n" +
				"priority: 0\nopaque_int: 9223372036854775807\n",
			enabled: true,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(tc.config)})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if errConfigure := configure(raw); errConfigure != nil {
					t.Fatal(errConfigure)
				}
				request := pluginapi.RequestTransformRequest{ToFormat: "codex", Model: "gpt-5.5"}
				if got := shouldSetPriorityServiceTier(request); got != tc.enabled {
					t.Fatalf("priority selection = %v, want %v", got, tc.enabled)
				}
			},
		)
	}
}

func TestPluginRegistrationUsesForkRepository(t *testing.T) {
	const want = "https://github.com/Pyrokine/CLIProxyAPI"
	if got := pluginRegistration().Metadata.GitHubRepository; got != want {
		t.Fatalf("repository = %q, want %q", got, want)
	}
}
