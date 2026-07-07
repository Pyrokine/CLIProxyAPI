package management

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

type fakePluginStoreHTTPClient func(*http.Request) (*http.Response, error)

func (f fakePluginStoreHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakePluginRuntimeHost struct {
	busy         bool
	unloadResult bool
	unloadCalls  int
	registered   []pluginhost.RegisteredPluginInfo
}

func (h *fakePluginRuntimeHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo {
	return h.registered
}

func (h *fakePluginRuntimeHost) PluginBusy(string) bool {
	return h.busy
}

func (h *fakePluginRuntimeHost) UnloadPlugin(string) bool {
	h.unloadCalls++
	if h.unloadResult {
		h.busy = false
	}
	return h.unloadResult
}

func TestListPluginStoreReturnsRegistryEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg:                    &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: t.TempDir()}},
		pluginStoreRegistryURL: "https://registry.example/registry.json",
		pluginStoreHTTPClient: fakePluginStoreHTTPClient(
			func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://registry.example/registry.json" {
					t.Fatalf("request url = %s", req.URL.String())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(
						strings.NewReader(
							`{
					"schema_version": 2,
					"plugins": [{
						"id": "sample-provider",
						"name": "Sample Provider",
						"description": "Adds <b>sample</b> provider support.",
						"author": "author-name",
						"version": "0.2.0",
						"install": {"type": "direct", "artifacts": [{
							"goos": "linux",
							"goarch": "amd64",
							"url": "https://registry.example/sample-provider-v0.2.0.zip",
							"sha256": "0000000000000000000000000000000000000000000000000000000000000000"
						}]}
					}]
				}`,
						),
					),
					Header:  http.Header{"Content-Type": []string{"application/json"}},
					Request: req,
				}, nil
			},
		),
		pluginReleaseCache: make(map[string]pluginReleaseCacheEntry),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugin-store", nil)

	h.ListPluginStore(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"sample-provider"`) {
		t.Fatalf("plugin store response missing plugin: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"plugins_enabled":true`) {
		t.Fatalf("plugin store response missing enabled state: %s", rec.Body.String())
	}
	var response pluginStoreListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if len(response.Plugins) != 1 {
		t.Fatalf("plugins length = %d, want 1", len(response.Plugins))
	}
	if got := response.Plugins[0].Description; got != "Adds <b>sample</b> provider support." {
		t.Fatalf("plugin description = %q, want raw JSON value", got)
	}
}

func TestPluginStoreDesiredVersionReadsStoreManifest(t *testing.T) {
	item := pluginConfigFromYAML(
		t, `
enabled: true
store:
  id: sample-provider
  version: v0.2.0
`,
	)
	if got := pluginStoreDesiredVersion(item); got != "0.2.0" {
		t.Fatalf("pluginStoreDesiredVersion() = %q, want 0.2.0", got)
	}
}

func TestPluginStoreManifestYAMLNodeRoundTrip(t *testing.T) {
	manifest := pluginstore.Manifest{
		ID: "sample-provider", Version: "0.2.0", SourceURL: "https://registry.example/registry.json",
	}
	node, err := pluginStoreManifestYAMLNode(manifest)
	if err != nil {
		t.Fatalf("pluginStoreManifestYAMLNode() error = %v", err)
	}
	var decoded pluginstore.Manifest
	if err := node.Decode(&decoded); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.ID != manifest.ID || decoded.Version != manifest.Version || decoded.SourceURL != manifest.SourceURL {
		t.Fatalf("decoded manifest = %#v, want %#v", decoded, manifest)
	}
}

func TestDeletePluginRemovesFileAndConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if err := os.WriteFile(pluginPath, []byte("plugin"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     pluginsDir,
			Configs: map[string]config.PluginInstanceConfig{
				"sample-provider": pluginConfigFromYAML(
					t, "enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
				),
			},
		},
	}
	h := &Handler{cfg: cfg, configFilePath: configPath}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)

	h.DeletePlugin(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("plugin file still exists or stat failed with unexpected error: %v", err)
	}
	if _, ok := cfg.Plugins.Configs["sample-provider"]; ok {
		t.Fatalf("plugin config was not removed")
	}
	if !strings.Contains(rec.Body.String(), `"configured_removed":true`) {
		t.Fatalf("delete response missing configured_removed: %s", rec.Body.String())
	}
}

func TestDeletePluginUnloadsRuntimeBeforeDeletingFileAndConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if err := os.WriteFile(pluginPath, []byte("plugin"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     pluginsDir,
			Configs: map[string]config.PluginInstanceConfig{
				"sample-provider": pluginConfigFromYAML(
					t, "enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
				),
			},
		},
	}
	host := &fakePluginRuntimeHost{busy: true, unloadResult: true}
	h := &Handler{cfg: cfg, configFilePath: configPath, pluginHost: host}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)

	h.DeletePlugin(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 1 {
		t.Fatalf("UnloadPlugin calls = %d, want 1", host.unloadCalls)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("plugin file still exists or stat failed with unexpected error: %v", err)
	}
	if _, ok := cfg.Plugins.Configs["sample-provider"]; ok {
		t.Fatalf("plugin config was not removed")
	}
	if !strings.Contains(rec.Body.String(), `"runtime_unloaded":true`) {
		t.Fatalf("delete response missing runtime_unloaded: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"restart_required":false`) {
		t.Fatalf("delete response missing restart_required false: %s", rec.Body.String())
	}
}

func TestDeletePluginKeepsBusyRuntimeWhenUnloadFails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if err := os.WriteFile(pluginPath, []byte("plugin"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     pluginsDir,
			Configs: map[string]config.PluginInstanceConfig{
				"sample-provider": pluginConfigFromYAML(
					t, "enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
				),
			},
		},
	}
	host := &fakePluginRuntimeHost{busy: true, unloadResult: false}
	h := &Handler{cfg: cfg, configFilePath: configPath, pluginHost: host}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)

	h.DeletePlugin(c)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 1 {
		t.Fatalf("UnloadPlugin calls = %d, want 1", host.unloadCalls)
	}
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("plugin file was removed or stat failed: %v", err)
	}
	if _, ok := cfg.Plugins.Configs["sample-provider"]; !ok {
		t.Fatalf("plugin config was removed")
	}
	if !strings.Contains(rec.Body.String(), `"error":"plugin_delete_requires_restart"`) {
		t.Fatalf("delete response missing conflict error: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"restart_required":true`) {
		t.Fatalf("delete response missing restart_required true: %s", rec.Body.String())
	}
}

func TestPreparePluginStoreOverwriteUnloadsBusyPlugin(t *testing.T) {
	host := &fakePluginRuntimeHost{busy: true, unloadResult: true}

	if err := preparePluginStoreOverwrite("sample-provider", host); err != nil {
		t.Fatalf("preparePluginStoreOverwrite() error = %v", err)
	}
	if host.unloadCalls != 1 {
		t.Fatalf("UnloadPlugin calls = %d, want 1", host.unloadCalls)
	}
	if host.busy {
		t.Fatal("host.busy = true, want false")
	}
}

func TestPreparePluginStoreOverwriteRejectsStillBusyPlugin(t *testing.T) {
	host := &fakePluginRuntimeHost{busy: true, unloadResult: false}

	err := preparePluginStoreOverwrite("sample-provider", host)
	if err == nil {
		t.Fatal("preparePluginStoreOverwrite() error = nil, want error")
	}
	if host.unloadCalls != 1 {
		t.Fatalf("UnloadPlugin calls = %d, want 1", host.unloadCalls)
	}
	if !strings.Contains(err.Error(), "loaded plugin") {
		t.Fatalf("preparePluginStoreOverwrite() error = %v, want loaded plugin", err)
	}
}

func pluginConfigFromYAML(t *testing.T, raw string) config.PluginInstanceConfig {
	t.Helper()
	var cfg struct {
		Plugin config.PluginInstanceConfig `yaml:"plugin"`
	}
	if err := yaml.Unmarshal([]byte("plugin:\n"+indentYAML(raw)), &cfg); err != nil {
		t.Fatalf("Unmarshal plugin config: %v", err)
	}
	return cfg.Plugin
}

func indentYAML(raw string) string {
	lines := strings.Split(strings.Trim(raw, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func testPluginExtension() string {
	switch runtime.GOOS {
	case "darwin":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}
