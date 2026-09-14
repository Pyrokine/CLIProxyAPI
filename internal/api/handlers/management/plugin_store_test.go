package management

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
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
	unloadErr    error
	unloadCalls  int
	registered   []pluginhost.RegisteredPluginInfo
	identity     pluginhost.PluginRuntimeIdentity
	onUnload     func()
}

func (h *fakePluginRuntimeHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo {
	return h.registered
}

func (h *fakePluginRuntimeHost) PluginBusy(string) bool {
	return h.busy
}

func (h *fakePluginRuntimeHost) PluginRuntimeIdentity(id string) pluginhost.PluginRuntimeIdentity {
	identity := h.identity
	if identity.ID == "" {
		identity = pluginhost.PluginRuntimeIdentity{ID: id, OperationEpoch: 1}
	}
	return identity
}

func (h *fakePluginRuntimeHost) UnloadPluginWithError(string) (bool, error) {
	return h.unload()
}

func (h *fakePluginRuntimeHost) UnloadPluginIfCurrentWithError(
	identity pluginhost.PluginRuntimeIdentity,
) (bool, error) {
	if current := h.PluginRuntimeIdentity(identity.ID); current != identity {
		return false, pluginhost.ErrPluginRuntimeSuperseded
	}
	return h.unload()
}

func (h *fakePluginRuntimeHost) unload() (bool, error) {
	h.unloadCalls++
	if h.unloadResult {
		h.busy = false
	}
	if h.onUnload != nil {
		h.onUnload()
	}
	return h.unloadResult, h.unloadErr
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

func TestListPluginStoreReleasesUsesConfiguredGitHubAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PLUGIN_STORE_TOKEN", "secret-token")

	registryURL := "https://registry.example/registry.json"
	releasesURL := "https://api.github.com/repos/author-name/sample-provider/releases?per_page=50"
	registryJSON := `{
		"schema_version": 2,
		"plugins": [{
			"id": "sample-provider",
			"name": "Sample Provider",
			"description": "Sample provider",
			"author": "author-name",
			"version": "0.2.0",
			"repository": "https://github.com/author-name/sample-provider",
			"install": {"type": "github-release"}
		}]
	}`
	releasesJSON := `[
		{
			"tag_name": "v0.2.0",
			"name": "Version 0.2.0",
			"published_at": "2026-07-31T00:00:00Z",
			"prerelease": false,
			"draft": false,
			"html_url": "https://github.com/author-name/sample-provider/releases/tag/v0.2.0",
			"assets": [{"name": "sample-provider_0.2.0_linux_amd64.zip"}]
		},
		{"tag_name": "v0.3.0-draft", "draft": true, "assets": []}
	]`
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			StoreAuth: []sdkpluginstore.AuthConfig{
				{
					Match:    "https://api.github.com/repos/author-name/sample-provider/releases/",
					ApplyTo:  []string{sdkpluginstore.RequestKindMetadata},
					Type:     sdkpluginstore.AuthTypeGitHubToken,
					TokenEnv: "PLUGIN_STORE_TOKEN",
				},
			},
		},
	}
	h := &Handler{
		cfg:                    cfg,
		pluginStoreRegistryURL: registryURL,
		pluginStoreHTTPClient: fakePluginStoreHTTPClient(
			func(req *http.Request) (*http.Response, error) {
				var data string
				switch req.URL.String() {
				case registryURL:
					data = registryJSON
				case releasesURL:
					if gotAuth := req.Header.Get("Authorization"); gotAuth != "Bearer secret-token" {
						t.Fatalf("Authorization = %q, want Bearer secret-token", gotAuth)
					}
					data = releasesJSON
				default:
					t.Fatalf("unexpected plugin store URL %s", req.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(data)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			},
		),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugin-store/sample-provider/releases", nil)

	h.ListPluginStoreReleases(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var releases []pluginStoreReleaseEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &releases); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("release count = %d, want 1 published release", len(releases))
	}
	if releases[0].TagName != "v0.2.0" || releases[0].Name != "Version 0.2.0" {
		t.Fatalf("release = %#v", releases[0])
	}
	if len(releases[0].AssetNames) != 1 || releases[0].AssetNames[0] != "sample-provider_0.2.0_linux_amd64.zip" {
		t.Fatalf("release assets = %#v", releases[0].AssetNames)
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

func TestPluginStoreInstallSourceStatusTracksConfiguredSource(t *testing.T) {
	sources := []pluginstore.Source{
		{ID: "official", URL: "https://official.example/registry.json"},
		{ID: "community", URL: "https://community.example/registry.json"},
	}
	status := pluginLocalStatus{
		Installed:          true,
		Configured:         true,
		StoreManaged:       true,
		InstalledSourceID:  "official",
		InstalledSourceURL: "https://official.example/registry.json",
	}

	installedSourceID, sourceStatus, allowUpdate := pluginStoreInstallSourceStatus(
		status, sources, "official", 2,
	)
	if installedSourceID != "official" || sourceStatus != "matched" || !allowUpdate {
		t.Fatalf(
			"matched status = (%q, %q, %v), want (official, matched, true)",
			installedSourceID,
			sourceStatus,
			allowUpdate,
		)
	}
	installedSourceID, sourceStatus, allowUpdate = pluginStoreInstallSourceStatus(
		status, sources, "community", 2,
	)
	if installedSourceID != "official" || sourceStatus != "different" || allowUpdate {
		t.Fatalf(
			"different status = (%q, %q, %v), want (official, different, false)",
			installedSourceID,
			sourceStatus,
			allowUpdate,
		)
	}
}

func TestInstallPluginFromStoreRejectsImplicitSourceSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	communityURL := "https://community.example/registry.json"
	registryJSON := `{"schema_version":2,"plugins":[{"id":"sample-provider","name":"Sample Provider","description":"Sample provider","author":"author-name","version":"0.2.0","install":{"type":"direct","artifacts":[{"goos":"linux","goarch":"amd64","url":"https://community.example/sample.zip","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}]}}]}`
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled:      true,
			Dir:          t.TempDir(),
			StoreSources: []string{communityURL},
			Configs: map[string]config.PluginInstanceConfig{
				"sample-provider": pluginConfigFromYAML(
					t,
					fmt.Sprintf(
						"enabled: true\nstore:\n  id: sample-provider\n  source-id: %s\n  source-url: %s\n",
						pluginstore.DefaultSourceID,
						pluginstore.DefaultRegistryURL,
					),
				),
			},
		},
	}
	h := &Handler{
		cfg: cfg,
		pluginStoreHTTPClient: fakePluginStoreHTTPClient(
			func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != pluginstore.DefaultRegistryURL && req.URL.String() != communityURL {
					t.Fatalf("unexpected plugin store URL %s", req.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(registryJSON)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			},
		),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	communitySourceID := pluginstore.SourceID(communityURL)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/plugin-store/sample-provider/install?source="+communitySourceID,
		nil,
	)

	h.installPluginFromStore(c, "linux", "amd64")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "plugin_store_source_conflict") ||
		!strings.Contains(rec.Body.String(), pluginstore.DefaultSourceID) {
		t.Fatalf("body = %s, want installed source conflict", rec.Body.String())
	}
}

func TestValidatePluginStoreInstallSourceRejectsUnknownManagedSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	configs := map[string]config.PluginInstanceConfig{
		"sample-provider": pluginConfigFromYAML(
			t,
			"enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
		),
	}

	if validatePluginStoreInstallSource(
		c,
		configs,
		[]pluginstore.Source{pluginstore.DefaultSource()},
		"sample-provider",
		pluginstore.DefaultSourceID,
	) {
		t.Fatal("unknown managed source was accepted")
	}
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "plugin_store_installed_source_unknown") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestInstallPluginFromStoreRejectsConfigChangeAfterArtifactPreparation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pluginsDir := t.TempDir()
	registryURL := "https://registry.example/registry.json"
	artifactURL := "https://registry.example/sample-provider.zip"
	archiveData := pluginStoreTestArchive(t, "sample-provider.so", "plugin-data")
	checksum := sha256.Sum256(archiveData)
	registryJSON := fmt.Sprintf(
		`{"schema_version":2,"plugins":[{"id":"sample-provider","name":"Sample Provider","description":"Sample provider","author":"author-name","version":"0.2.0","install":{"type":"direct","artifacts":[{"goos":"linux","goarch":"amd64","url":%q,"sha256":%q}]}}]}`,
		artifactURL,
		hex.EncodeToString(checksum[:]),
	)
	initialConfig := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     pluginsDir,
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}
	changedConfig := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     pluginsDir,
			StoreAuth: []sdkpluginstore.AuthConfig{
				{Match: "https://registry.example/", Type: sdkpluginstore.AuthTypeNone},
			},
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}
	externalStarted := make(chan struct{})
	releaseExternal := make(chan struct{})
	externalDone := make(chan error, 1)
	var h *Handler
	h = &Handler{
		cfg:                    initialConfig,
		pluginStoreRegistryURL: registryURL,
		pluginStoreHTTPClient: fakePluginStoreHTTPClient(
			func(req *http.Request) (*http.Response, error) {
				var data []byte
				switch req.URL.String() {
				case registryURL:
					data = []byte(registryJSON)
				case artifactURL:
					if !h.commitMu.TryLock() {
						t.Fatal("artifact download ran while the config commit lock was held")
					}
					h.commitMu.Unlock()
					go func() {
						externalDone <- h.ApplyExternalConfigUpdate(
							func() error {
								close(externalStarted)
								<-releaseExternal
								h.SetConfig(changedConfig)
								return nil
							},
						)
					}()
					<-externalStarted
					data = archiveData
				default:
					t.Fatalf("unexpected plugin store URL %s", req.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(data)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			},
		),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/plugin-store/sample-provider", nil)
	installDone := make(chan struct{})
	go func() {
		h.installPluginFromStore(c, "linux", "amd64")
		close(installDone)
	}()
	<-externalStarted
	select {
	case <-installDone:
		t.Fatal("plugin install completed while the external config update held the commit sequence")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseExternal)
	if errExternal := <-externalDone; errExternal != nil {
		t.Fatalf("ApplyExternalConfigUpdate() error = %v", errExternal)
	}
	select {
	case <-installDone:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin install did not resume after the external config update")
	}

	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "plugin_store_config_changed") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	targetPath := filepath.Join(pluginsDir, "linux", "amd64", "sample-provider-v0.2.0.so")
	if _, errStat := os.Stat(targetPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("plugin artifact was committed after config change: %v", errStat)
	}
	if h.configSnapshot() != changedConfig {
		t.Fatal("hot-reloaded config was replaced by the stale install snapshot")
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
	if _, ok := h.configSnapshot().Plugins.Configs["sample-provider"]; ok {
		t.Fatalf("plugin config was not removed")
	}
	if !strings.Contains(rec.Body.String(), `"configured_removed":true`) {
		t.Fatalf("delete response missing configured_removed: %s", rec.Body.String())
	}
}

func TestDeletePluginRemovesAllArtifactVersions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	paths := []string{
		filepath.Join(archDir, "sample-provider-v0.1.0"+testPluginExtension()),
		filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension()),
		filepath.Join(pluginsDir, "sample-provider-v0.3.0"+testPluginExtension()),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o755); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
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
	for _, path := range paths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("plugin artifact %s remains after deletion: %v", path, errStat)
		}
	}
}

func TestDeletePluginRejectsSymlinkPlatformDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated Windows privileges")
	}
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(plugins) error = %v", err)
	}
	outside := t.TempDir()
	archDir := filepath.Join(outside, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(outside arch) error = %v", err)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if err := os.WriteFile(pluginPath, []byte("plugin"), 0o755); err != nil {
		t.Fatalf("WriteFile(plugin) error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(pluginsDir, runtime.GOOS)); err != nil {
		t.Fatalf("Symlink(platform) error = %v", err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("plugins:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     pluginsDir,
				Configs: map[string]config.PluginInstanceConfig{
					"sample-provider": pluginConfigFromYAML(
						t, "enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
					),
				},
			},
		},
		configFilePath: configPath,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.DeletePlugin(c)

	if rec.Code != http.StatusInternalServerError ||
		!strings.Contains(rec.Body.String(), "plugin_discovery_failed") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := h.configSnapshot().Plugins.Configs["sample-provider"]; !ok {
		t.Fatal("plugin config was removed after unsafe discovery")
	}
	if data, errRead := os.ReadFile(pluginPath); errRead != nil || string(data) != "plugin" {
		t.Fatalf("plugin artifact changed: data=%q error=%v", data, errRead)
	}
}

func TestDeleteUnconfiguredPluginDoesNotWaitForRuntimeSync(t *testing.T) {
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
	syncCalls := 0
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     pluginsDir,
				Configs: map[string]config.PluginInstanceConfig{},
			},
		},
		pluginRuntimeSync: func(context.Context, *config.Config) error {
			syncCalls++
			return errors.New("unconfigured deletion must not run full runtime sync")
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.DeletePlugin(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if syncCalls != 0 {
		t.Fatalf("plugin runtime sync calls = %d, want 0", syncCalls)
	}
	if _, errStat := os.Stat(pluginPath); !os.IsNotExist(errStat) {
		t.Fatalf("plugin artifact remains after deletion: %v", errStat)
	}
}

func TestDeletePluginReportsArtifactCommitFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission failure injection is Unix-specific")
	}
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
	if errWrite := os.WriteFile(configPath, []byte("plugins:\n  enabled: true\n"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
	}
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     pluginsDir,
				Configs: map[string]config.PluginInstanceConfig{
					"sample-provider": pluginConfigFromYAML(
						t, "enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
					),
				},
			},
		},
		configFilePath: configPath,
		configCommitted: func(_ context.Context, commit ConfigCommit) error {
			if !commit.DeferPluginRuntimeSync {
				t.Fatal("plugin deletion did not defer full runtime sync")
			}
			if errChmod := os.Chmod(pluginsDir, 0o555); errChmod != nil {
				t.Fatalf("Chmod() error = %v", errChmod)
			}
			return nil
		},
	}
	t.Cleanup(func() { _ = os.Chmod(pluginsDir, 0o755) })

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.DeletePlugin(c)

	if errChmod := os.Chmod(pluginsDir, 0o755); errChmod != nil {
		t.Fatalf("restore plugin directory permissions: %v", errChmod)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"plugin_delete_failed"`) {
		t.Fatalf("delete response missing artifact failure: %s", rec.Body.String())
	}
}

func TestDeletePluginUnloadsRuntimeAfterDeletingFileAndConfig(t *testing.T) {
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
	host.onUnload = func() {
		if _, errStat := os.Stat(pluginPath); !os.IsNotExist(errStat) {
			t.Fatalf("plugin artifact was visible during runtime unload: %v", errStat)
		}
		if _, configured := h.configSnapshot().Plugins.Configs["sample-provider"]; configured {
			t.Fatal("plugin config remained during runtime unload")
		}
	}

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
	if _, ok := h.configSnapshot().Plugins.Configs["sample-provider"]; ok {
		t.Fatalf("plugin config was not removed")
	}
	if !strings.Contains(rec.Body.String(), `"runtime_unloaded":true`) {
		t.Fatalf("delete response missing runtime_unloaded: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"restart_required":false`) {
		t.Fatalf("delete response missing restart_required false: %s", rec.Body.String())
	}
}

func TestDeletePluginIgnoresUnrelatedConfigGenerationChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(archDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if errWrite := os.WriteFile(pluginPath, []byte("plugin"), 0o755); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
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
	host := &fakePluginRuntimeHost{
		busy:         true,
		unloadResult: true,
		identity: pluginhost.PluginRuntimeIdentity{
			ID: "sample-provider", Path: pluginPath, Version: "0.2.0", OperationEpoch: 1,
		},
	}
	h := &Handler{cfg: cfg, configFilePath: configPath, pluginHost: host}
	h.configCommitted = func(context.Context, ConfigCommit) error {
		return h.ApplyExternalConfigUpdate(
			func() error {
				h.mu.Lock()
				h.cfg.Debug = !h.cfg.Debug
				h.configGeneration++
				h.mu.Unlock()
				return nil
			},
		)
	}

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
	if _, errStat := os.Stat(pluginPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("plugin artifact was restored after unrelated config update: %v", errStat)
	}
	if _, configured := h.configSnapshot().Plugins.Configs["sample-provider"]; configured {
		t.Fatal("plugin config returned after committed deletion")
	}
}

func TestDeletePluginDoesNotUnloadRuntimeFromSupersedingRoot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	rootA := filepath.Join(root, "plugins-a")
	rootB := filepath.Join(root, "plugins-b")
	for _, pluginsDir := range []string{rootA, rootB} {
		archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
		if errMkdir := os.MkdirAll(archDir, 0o755); errMkdir != nil {
			t.Fatalf("MkdirAll(%s) error = %v", archDir, errMkdir)
		}
		pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
		if errWrite := os.WriteFile(pluginPath, []byte(pluginsDir), 0o755); errWrite != nil {
			t.Fatalf("WriteFile(%s) error = %v", pluginPath, errWrite)
		}
	}
	rootAPath := filepath.Join(rootA, runtime.GOOS, runtime.GOARCH, "sample-provider-v0.2.0"+testPluginExtension())
	rootBPath := filepath.Join(rootB, runtime.GOOS, runtime.GOARCH, "sample-provider-v0.2.0"+testPluginExtension())
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+rootA+"\n"), 0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
	}
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     rootA,
			Configs: map[string]config.PluginInstanceConfig{
				"sample-provider": pluginConfigFromYAML(
					t, "enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n",
				),
			},
		},
	}
	host := &fakePluginRuntimeHost{
		busy:         true,
		unloadResult: true,
		identity: pluginhost.PluginRuntimeIdentity{
			ID: "sample-provider", Path: rootAPath, Version: "0.2.0", OperationEpoch: 1,
		},
	}
	h := &Handler{cfg: cfg, configFilePath: configPath, pluginHost: host}
	h.configCommitted = func(context.Context, ConfigCommit) error {
		return h.ApplyExternalConfigUpdate(
			func() error {
				h.mu.Lock()
				h.cfg = &config.Config{
					Plugins: config.PluginsConfig{
						Enabled: true,
						Dir:     rootB,
						Configs: map[string]config.PluginInstanceConfig{},
					},
				}
				h.configGeneration++
				h.mu.Unlock()
				host.identity = pluginhost.PluginRuntimeIdentity{
					ID: "sample-provider", Path: rootBPath, Version: "0.2.0", OperationEpoch: 2,
				}
				return nil
			},
		)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.DeletePlugin(c)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "plugin_delete_superseded") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 0 {
		t.Fatalf("UnloadPlugin calls = %d, want 0 for superseding root", host.unloadCalls)
	}
	if _, errStat := os.Stat(rootAPath); errStat != nil {
		t.Fatalf("root A artifact was not restored: %v", errStat)
	}
	if _, errStat := os.Stat(rootBPath); errStat != nil {
		t.Fatalf("root B artifact changed: %v", errStat)
	}
}

func TestDeletePluginDoesNotUnloadSupersedingRuntimeIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(archDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if errWrite := os.WriteFile(pluginPath, []byte("plugin"), 0o755); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
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
	host := &fakePluginRuntimeHost{
		busy:         true,
		unloadResult: true,
		identity: pluginhost.PluginRuntimeIdentity{
			ID: "sample-provider", Path: pluginPath, Version: "0.2.0", OperationEpoch: 1,
		},
	}
	h := &Handler{
		cfg: cfg, configFilePath: configPath, pluginHost: host,
		configCommitted: func(context.Context, ConfigCommit) error {
			host.identity = pluginhost.PluginRuntimeIdentity{
				ID: "sample-provider", Path: filepath.Join(root, "replacement.so"),
				Version: "0.2.0", OperationEpoch: 2,
			}
			return nil
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.DeletePlugin(c)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "plugin_delete_requires_restart") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 0 {
		t.Fatalf("UnloadPlugin calls = %d, want 0 for superseding runtime identity", host.unloadCalls)
	}
	if _, errStat := os.Stat(pluginPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("deleted artifact remains: %v", errStat)
	}
}

func TestDeletePluginDetachesRuntimeAfterCommittedSyncFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(archDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if errWrite := os.WriteFile(pluginPath, []byte("plugin"), 0o755); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
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
	h := &Handler{
		cfg:            cfg,
		configFilePath: configPath,
		pluginHost:     host,
		configCommitted: func(_ context.Context, commit ConfigCommit) error {
			if !commit.DeferPluginRuntimeSync {
				t.Fatal("plugin deletion did not defer full runtime sync")
			}
			return errors.New("runtime sync failed")
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.runtimeCommitMu.Lock()
	deleteDone := make(chan struct{})
	go func() {
		h.DeletePlugin(c)
		close(deleteDone)
	}()
	select {
	case <-deleteDone:
	case <-time.After(time.Second):
		t.Fatal("DeletePlugin waited for a blocked full runtime callback")
	}
	h.runtimeCommitMu.Unlock()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 1 || host.busy {
		t.Fatalf("runtime detach calls = %d, busy = %v", host.unloadCalls, host.busy)
	}
	if _, errStat := os.Stat(pluginPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("plugin artifact remains: %v", errStat)
	}
	if _, configured := h.configSnapshot().Plugins.Configs["sample-provider"]; configured {
		t.Fatal("plugin config remained after committed deletion")
	}
	if !strings.Contains(rec.Body.String(), `"restart_required":false`) {
		t.Fatalf("delete response = %s", rec.Body.String())
	}
}

func TestDeletePluginFinalizesBeforeRuntimeUnloadMutatesRoot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(archDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if errWrite := os.WriteFile(pluginPath, []byte("plugin"), 0o755); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(
		configPath, []byte("plugins:\n  enabled: true\n  dir: "+pluginsDir+"\n"), 0o600,
	); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
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
	backupDir := pluginsDir + ".backup"
	var hookErr error
	host := &fakePluginRuntimeHost{busy: true, unloadResult: true}
	host.onUnload = func() {
		if errRename := os.Rename(pluginsDir, backupDir); errRename != nil {
			hookErr = errRename
			return
		}
		hookErr = os.WriteFile(pluginsDir, []byte("blocks root recreation"), 0o600)
	}
	runtimeSyncCalls := 0
	h := &Handler{
		cfg:            cfg,
		configFilePath: configPath,
		pluginHost:     host,
		configCommitted: func(_ context.Context, commit ConfigCommit) error {
			if !commit.DeferPluginRuntimeSync {
				t.Fatal("plugin deletion did not defer full runtime sync")
			}
			runtimeSyncCalls++
			return nil
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)
	h.DeletePlugin(c)

	if hookErr != nil {
		t.Fatalf("unload hook error = %v", hookErr)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 1 || runtimeSyncCalls != 1 {
		t.Fatalf("unload calls = %d, runtime sync calls = %d", host.unloadCalls, runtimeSyncCalls)
	}
	if _, configured := h.configSnapshot().Plugins.Configs["sample-provider"]; configured {
		t.Fatal("plugin config remained after committed deletion")
	}
}

func TestDeletePluginRequiresRestartWhenCommittedRuntimeUnloadFails(t *testing.T) {
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
	host := &fakePluginRuntimeHost{
		busy:         true,
		unloadResult: true,
		unloadErr:    context.DeadlineExceeded,
	}
	h := &Handler{cfg: cfg, configFilePath: configPath, pluginHost: host}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)

	h.DeletePlugin(c)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if host.unloadCalls != 1 || host.busy {
		t.Fatalf("UnloadPlugin calls = %d, busy = %v, want 1/false", host.unloadCalls, host.busy)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("plugin file remains after committed deletion: %v", err)
	}
	if _, ok := h.configSnapshot().Plugins.Configs["sample-provider"]; ok {
		t.Fatalf("plugin config remains after committed deletion")
	}
	if !strings.Contains(rec.Body.String(), `"error":"plugin_delete_requires_restart"`) {
		t.Fatalf("delete response missing conflict error: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"restart_required":true`) {
		t.Fatalf("delete response missing restart_required true: %s", rec.Body.String())
	}
}

func TestInstallPluginRollsBackArtifactWhenConfigPersistenceFails(t *testing.T) {
	testCases := []struct {
		name       string
		existing   bool
		wantConfig bool
	}{
		{name: "new artifact", existing: false, wantConfig: false},
		{name: "replaced artifact", existing: true, wantConfig: true},
	}

	for _, tc := range testCases {
		t.Run(
			tc.name, func(t *testing.T) {
				root := t.TempDir()
				pluginsDir := filepath.Join(root, "plugins")
				targetDir := filepath.Join(pluginsDir, "linux", "amd64")
				if err := os.MkdirAll(targetDir, 0o755); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				targetPath := filepath.Join(targetDir, "sample-provider-v0.2.0.so")
				if tc.existing {
					if err := os.WriteFile(targetPath, []byte("old-plugin"), 0o755); err != nil {
						t.Fatalf("WriteFile(old plugin) error = %v", err)
					}
				}

				archiveData := pluginStoreTestArchive(t, "sample-provider.so", "new-plugin")
				checksum := sha256.Sum256(archiveData)
				registryURL := "https://registry.example/registry.json"
				artifactURL := "https://registry.example/sample-provider-v0.2.0.zip"
				registry := fmt.Sprintf(
					`{"schema_version":2,"plugins":[{"id":"sample-provider","name":"Sample Provider","description":"Sample provider","author":"author-name","version":"0.2.0","install":{"type":"direct","artifacts":[{"goos":"linux","goarch":"amd64","url":%q,"sha256":%q}]}}]}`,
					artifactURL,
					hex.EncodeToString(checksum[:]),
				)
				client := fakePluginStoreHTTPClient(
					func(req *http.Request) (*http.Response, error) {
						var data []byte
						switch req.URL.String() {
						case registryURL:
							data = []byte(registry)
						case artifactURL:
							data = archiveData
						default:
							t.Fatalf("unexpected plugin store URL %s", req.URL)
						}
						return &http.Response{
							StatusCode: http.StatusOK,
							Body:       io.NopCloser(bytes.NewReader(data)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					},
				)
				configs := map[string]config.PluginInstanceConfig{}
				if tc.wantConfig {
					configs["sample-provider"] = pluginConfigFromYAML(
						t,
						fmt.Sprintf(
							"enabled: true\nstore:\n  id: sample-provider\n  version: v0.2.0\n  source-id: %s\n  source-url: %s\n",
							pluginstore.DefaultSourceID,
							registryURL,
						),
					)
				}
				cfg := &config.Config{
					Plugins: config.PluginsConfig{
						Enabled: true,
						Dir:     pluginsDir,
						Configs: configs,
					},
				}
				blockedParent := filepath.Join(root, "not-a-directory")
				if err := os.WriteFile(blockedParent, []byte("blocked"), 0o600); err != nil {
					t.Fatalf("WriteFile(blocking file) error = %v", err)
				}
				h := &Handler{
					cfg:                    cfg,
					configFilePath:         filepath.Join(blockedParent, "config.yaml"),
					pluginStoreRegistryURL: registryURL,
					pluginStoreHTTPClient:  client,
					pluginReleaseCache:     make(map[string]pluginReleaseCacheEntry),
				}

				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
				c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/plugin-store/sample-provider", nil)

				h.installPluginFromStore(c, "linux", "amd64")

				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
				}
				data, errRead := os.ReadFile(targetPath)
				if tc.existing {
					if errRead != nil || string(data) != "old-plugin" {
						t.Fatalf("restored plugin = %q, error = %v; want old-plugin", data, errRead)
					}
				} else if !os.IsNotExist(errRead) {
					t.Fatalf("new plugin remains after rollback: data=%q error=%v", data, errRead)
				}
				_, configured := h.configSnapshot().Plugins.Configs["sample-provider"]
				if configured != tc.wantConfig {
					t.Fatalf("configured = %v, want %v", configured, tc.wantConfig)
				}
			},
		)
	}
}

func TestDeletePluginRestoresArtifactWhenConfigPersistenceFails(t *testing.T) {
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	if err := os.WriteFile(pluginPath, []byte("plugin"), 0o755); err != nil {
		t.Fatalf("WriteFile(plugin) error = %v", err)
	}
	blockedParent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocking file) error = %v", err)
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
	h := &Handler{cfg: cfg, configFilePath: filepath.Join(blockedParent, "config.yaml")}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = []gin.Param{{Key: "id", Value: "sample-provider"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample-provider", nil)

	h.DeletePlugin(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if data, err := os.ReadFile(pluginPath); err != nil || string(data) != "plugin" {
		t.Fatalf("restored plugin = %q, error = %v; want plugin", data, err)
	}
	if _, configured := h.configSnapshot().Plugins.Configs["sample-provider"]; !configured {
		t.Fatal("plugin config was removed after persistence failure")
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

func pluginStoreTestArchive(t *testing.T, name string, data string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatalf("Create zip entry: %v", err)
	}
	if _, err = entry.Write([]byte(data)); err != nil {
		t.Fatalf("Write zip entry: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}
	return buffer.Bytes()
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
