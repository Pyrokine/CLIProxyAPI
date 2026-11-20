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

func TestRuntimePluginEnabledDefaults(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{name: "omitted"},
		{name: "disabled", enabled: new(false)},
		{name: "enabled", enabled: new(true), want: true},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				runtime, err := runtimeConfigFromConfig(
					&config.Config{
						Plugins: config.PluginsConfig{
							Configs: map[string]config.PluginInstanceConfig{
								"fixture": {Enabled: testCase.enabled},
							},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				item, exists := runtime.Items["fixture"]
				if !exists || item.Enabled != testCase.want {
					t.Fatalf("plugin enabled = %v, exists = %v, want %v", item.Enabled, exists, testCase.want)
				}
				var published struct {
					Enabled *bool `yaml:"enabled"`
				}
				if err := yaml.Unmarshal(item.ConfigYAML, &published); err != nil {
					t.Fatal(err)
				}
				if published.Enabled == nil || *published.Enabled != testCase.want {
					t.Fatalf("runtime YAML does not publish enabled=%v: %s", testCase.want, item.ConfigYAML)
				}
			},
		)
	}
	item := defaultRuntimeItemConfig("unconfigured")
	if item.Enabled || !strings.Contains(string(item.ConfigYAML), "enabled: false") {
		t.Fatalf("unconfigured plugin must remain disabled: %#v", item)
	}
}

func TestNormalizedPluginConfigKeepsIndependentAliasGraph(t *testing.T) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("mode: &modeAnchor first\nmode-alias: *modeAnchor\n"), &document); err != nil {
		t.Fatal(err)
	}
	item := config.PluginInstanceConfig{Raw: *document.Content[0]}
	node := normalizedConfigNode(item, false)
	mode := yamlMappingValue(node, "mode")
	alias := yamlMappingValue(node, "mode-alias")
	if mode == nil || alias == nil || alias.Alias != mode {
		t.Fatal("normalized plugin config lost its internal alias relation")
	}
	mode.Value = "changed"
	var values map[string]any
	if err := node.Decode(&values); err != nil {
		t.Fatal(err)
	}
	if values["mode"] != "changed" || values["mode-alias"] != "changed" || values["enabled"] != false {
		t.Fatalf("normalized plugin values=%#v", values)
	}
	if source := yamlMappingValue(&item.Raw, "mode"); source == nil || source.Value != "first" {
		t.Fatal("normalizing plugin config changed its source")
	}
	var published map[string]any
	if err := yaml.Unmarshal(runtimeConfigYAML(item, false), &published); err != nil {
		t.Fatal(err)
	}
	if published["mode"] != "first" || published["mode-alias"] != "first" || published["enabled"] != false {
		t.Fatalf("published plugin values=%#v", published)
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
