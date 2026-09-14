package pluginhost

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

func TestRuntimeConfigFromConfigResolvesTildePluginDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	runtimeConfig, errRuntimeConfig := runtimeConfigFromConfig(
		&config.Config{
			Plugins: config.PluginsConfig{Enabled: true, Dir: "~/.cli-proxy-api/plugins"},
		},
	)
	if errRuntimeConfig != nil {
		t.Fatalf("runtimeConfigFromConfig() error = %v", errRuntimeConfig)
	}
	want := filepath.Join(homeDir, ".cli-proxy-api", "plugins")
	if runtimeConfig.Dir != want {
		t.Fatalf("runtimeConfig.Dir = %q, want %q", runtimeConfig.Dir, want)
	}
}

func TestRuntimeConfigYAMLAddsHostDefaultsToRawPluginConfig(t *testing.T) {
	var node yaml.Node
	if errDecode := yaml.Unmarshal([]byte("config1: true\nconfig2: value\n"), &node); errDecode != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errDecode)
	}
	if len(node.Content) != 1 {
		t.Fatalf("yaml node content length = %d, want 1", len(node.Content))
	}
	item := config.PluginInstanceConfig{
		Priority: 3,
		Raw:      *node.Content[0],
	}

	got := string(runtimeConfigYAML(item, true))
	for _, want := range []string{
		"config1: true",
		"config2: value",
		"enabled: true",
		"priority: 3",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("runtimeConfigYAML() missing %q in:\n%s", want, got)
		}
	}
}
