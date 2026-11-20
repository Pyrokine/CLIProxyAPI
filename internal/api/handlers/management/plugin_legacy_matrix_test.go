package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func TestListPluginsReadsPublicRawFieldsWithDisabledDefaults(t *testing.T) {
	for _, state := range []struct {
		name, enabled string
		nilEnabled    bool
		want          bool
	}{
		{name: "omitted"},
		{name: "false", enabled: "enabled: false\n"},
		{name: "true", enabled: "enabled: true\n", want: true},
		{name: "nil", nilEnabled: true},
	} {
		for _, globalEnabled := range []bool{false, true} {
			name := state.name + "/global-disabled"
			if globalEnabled {
				name = state.name + "/global-enabled"
			}
			t.Run(
				name, func(t *testing.T) {
					item := pluginConfigFromYAML(
						t,
						state.enabled+
							("priority: 7\nmode: safe\nnested:\n  label: visible\nsecret-key: "+
								"synthetic-private-value\n"),
					)
					if state.nilEnabled {
						item.Enabled = nil
					}
					before, err := encodeConfigYAML(&item.Raw)
					if err != nil {
						t.Fatal(err)
					}
					cfg := &config.Config{
						Plugins: config.PluginsConfig{
							Enabled: globalEnabled,
							Dir:     t.TempDir(),
							Configs: map[string]config.PluginInstanceConfig{"alpha": item},
						},
					}
					h := &Handler{
						cfg: cfg, pluginHost: &fakePluginRuntimeHost{
							registered: []pluginhost.RegisteredPluginInfo{
								{
									ID: "alpha",
									Metadata: pluginapi.Metadata{
										ConfigFields: []pluginapi.ConfigField{
											{Name: "mode", Type: pluginapi.ConfigFieldTypeEnum},
											{Name: "nested", Type: pluginapi.ConfigFieldTypeObject},
										},
									},
								},
							},
						},
					}
					read := func() pluginListEntry {
						t.Helper()
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugins", nil)
						h.ListPlugins(c)
						if recorder.Code != http.StatusOK || bytes.Contains(
							recorder.Body.Bytes(), []byte("synthetic-private-value"),
						) {
							t.Fatalf("public config status=%d body=%s", recorder.Code, recorder.Body.String())
						}
						var response pluginListResponse
						if err := json.Unmarshal(
							recorder.Body.Bytes(), &response,
						); err != nil || len(response.Plugins) != 1 {
							t.Fatalf("public config decode=%v entries=%d", err, len(response.Plugins))
						}
						if response.PluginsDir != cfg.Plugins.Dir {
							t.Fatalf("directory=%q want=%q", response.PluginsDir, cfg.Plugins.Dir)
						}
						return response.Plugins[0]
					}
					entry := read()
					if entry.ID != "alpha" || !entry.Configured || !entry.Registered ||
						entry.Enabled != state.want ||
						entry.EffectiveEnabled != (globalEnabled && state.want) ||
						entry.Config["mode"] != "safe" ||
						entry.Config["priority"] != float64(7) {
						t.Fatalf("public state=%+v", entry)
					}
					if entry.Config["nested"].(map[string]any)["label"] != "visible" {
						t.Fatal("declared public Raw object was omitted")
					}
					entry.Config["nested"].(map[string]any)["label"] = "changed response"
					if read().Config["nested"].(map[string]any)["label"] != "visible" {
						t.Fatal("response object changed the published Raw configuration")
					}
					original := cfg.Plugins.Configs["alpha"]
					after, err := encodeConfigYAML(&original.Raw)
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("public GET changed Raw YAML")
					}
				},
			)
		}
	}
}

func TestPluginRuntimeSnapshotIsolatesEnabledRawAndAliasNodes(t *testing.T) {
	item := pluginConfigFromYAML(t, "enabled: true\nshared: &settings\n  label: visible\nnested: *settings\n")
	cfg := &config.Config{
		Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{"alpha": item}},
	}
	before, err := encodeConfigYAML(&item.Raw)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pluginRuntimeConfigSnapshot(cfg)
	copied := snapshot.Plugins.Configs["alpha"]
	*copied.Enabled = false
	shared := yamlMappingValue(&copied.Raw, "shared")
	sharedLabel := yamlMappingValue(shared, "label")
	if sharedLabel == nil {
		t.Fatal("snapshot shared label is missing")
	}
	sharedLabel.Value = "changed snapshot"
	alias := yamlMappingValue(&copied.Raw, "nested")
	originalAlias := yamlMappingValue(&item.Raw, "nested")
	if alias == nil || originalAlias == nil || alias.Alias == nil || originalAlias.Alias == nil ||
		alias.Alias == originalAlias.Alias {
		t.Fatal("snapshot alias target still belongs to the source configuration")
	}
	aliasLabel := yamlMappingValue(alias.Alias, "label")
	originalLabel := yamlMappingValue(originalAlias.Alias, "label")
	if aliasLabel == nil || originalLabel == nil {
		t.Fatal("alias target label is missing")
	}
	aliasLabel.Value = "changed alias target"
	if originalLabel.Value != "visible" {
		t.Fatal("snapshot alias mutation changed the source alias target")
	}
	snapshot.Plugins.Configs["beta"] = copied
	delete(snapshot.Plugins.Configs, "alpha")
	original := cfg.Plugins.Configs["alpha"]
	after, err := encodeConfigYAML(&original.Raw)
	if err != nil || !bytes.Equal(
		before, after,
	) || original.Enabled == nil || !*original.Enabled || len(cfg.Plugins.Configs) != 1 {
		t.Fatal("runtime snapshot mutation changed the published config")
	}
	if snapshot := pluginRuntimeConfigSnapshot(nil); snapshot != nil {
		t.Fatal("nil config produced a runtime snapshot")
	}
	var decoded config.PluginInstanceConfig
	if err := yaml.Unmarshal(before, &decoded); err != nil || decoded.Enabled == nil || !*decoded.Enabled {
		t.Fatal("original Raw configuration no longer decodes independently")
	}
}
