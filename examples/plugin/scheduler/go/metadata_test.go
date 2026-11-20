package main

import (
	"encoding/json"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestSchedulerExampleOriginalDecisions(t *testing.T) {
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	request := pluginapi.SchedulerPickRequest{
		Provider: "openai", Providers: []string{"openai", "codex"}, Model: "gpt-5.4", Stream: true,
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Test": {"one", "two"}}},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-1", Provider: "openai", Priority: 10, Status: "ready"},
			{ID: "auth-2", Provider: "codex", Priority: 20, Status: "ready"},
		},
	}
	pickJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range []struct {
		name, config, authID, delegate, reject string
		handled                                bool
	}{
		{name: "select candidate", config: "auth_id: ' auth-2 '\n", authID: "auth-2", handled: true},
		{name: "missing candidate", config: "auth_id: absent\n"},
		{name: "empty reconfigure resets selection"},
		{
			name: "fill first", config: "delegate: fill-first\n", delegate: pluginapi.SchedulerBuiltinFillFirst,
			handled: true,
		},
		{
			name: "round robin", config: "delegate: round-robin\n", delegate: pluginapi.SchedulerBuiltinRoundRobin,
			handled: true,
		},
		{name: "invalid delegate", config: "delegate: unknown\nauth_id: auth-2\n"},
		{
			name: "merged host configuration",
			config: "defaults: &defaults {auth_id: auth-2}\n<<: *defaults\n" +
				"priority: 0\nopaque_int: 9223372036854775807\n",
			authID: "auth-2", handled: true,
		},
		{name: "legacy YAML boolean", config: "deny: yes\n", reject: "scheduler_denied"},
		{
			name: "deny takes precedence", config: "deny: true\ndelegate: fill-first\nauth_id: auth-2\n",
			reject: "scheduler_denied",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				configuration, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(tc.config)})
				if err != nil {
					t.Fatal(err)
				}
				method := pluginabi.MethodPluginReconfigure
				if index == 0 {
					method = pluginabi.MethodPluginRegister
				}
				registered, err := handleMethod(method, configuration)
				if err != nil {
					t.Fatal(err)
				}
				var registrationEnvelope envelope
				if err := json.Unmarshal(registered, &registrationEnvelope); err != nil {
					t.Fatal(err)
				}
				var capabilities registration
				if err := json.Unmarshal(registrationEnvelope.Result, &capabilities); err != nil {
					t.Fatal(err)
				}
				if !registrationEnvelope.OK || capabilities.SchemaVersion != pluginabi.SchemaVersion ||
					!capabilities.Capabilities.Scheduler {
					t.Fatalf("scheduler registration changed: %s", registered)
				}
				raw, err := handleMethod(pluginabi.MethodSchedulerPick, pickJSON)
				if err != nil {
					t.Fatal(err)
				}
				var responseEnvelope envelope
				if err := json.Unmarshal(raw, &responseEnvelope); err != nil {
					t.Fatal(err)
				}
				if tc.reject != "" {
					if responseEnvelope.OK || responseEnvelope.Error == nil ||
						responseEnvelope.Error.Code != tc.reject {
						t.Fatalf("scheduler rejection changed: %s", raw)
					}
					return
				}
				var response pluginapi.SchedulerPickResponse
				if err := json.Unmarshal(responseEnvelope.Result, &response); err != nil {
					t.Fatal(err)
				}
				if !responseEnvelope.OK || response.Handled != tc.handled || response.AuthID != tc.authID ||
					response.DelegateBuiltin != tc.delegate {
					t.Fatalf("scheduler decision changed: %s", raw)
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
