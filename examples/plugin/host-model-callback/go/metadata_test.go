package main

import "testing"

func TestPluginRegistrationUsesForkRepository(t *testing.T) {
	const want = "https://github.com/Pyrokine/CLIProxyAPI"
	if got := pluginRegistration().Metadata.GitHubRepository; got != want {
		t.Fatalf("repository = %q, want %q", got, want)
	}
}
