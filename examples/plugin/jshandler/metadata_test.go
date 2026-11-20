package main

import "testing"

func TestPluginRegistrationUsesForkRepository(t *testing.T) {
	plugin, err := buildPlugin([]byte("enabled: false\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://github.com/Pyrokine/CLIProxyAPI"
	if got := plugin.Metadata.GitHubRepository; got != want {
		t.Fatalf("repository = %q, want %q", got, want)
	}
}
