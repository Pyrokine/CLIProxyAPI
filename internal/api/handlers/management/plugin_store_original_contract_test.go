package management

import (
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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/gin-gonic/gin"
)

func TestPluginStoreManagementSourceErrorsPreservePrivacy(t *testing.T) {
	for _, mode := range []string{"signed request failure", "authenticated response"} {
		for _, action := range []string{
			"catalog", "releases implicit source", "releases explicit source", "install implicit source",
			"install explicit source",
		} {
			t.Run(
				mode+"/"+action, func(t *testing.T) {
					const source = "https://fixture-user:fixture-password@registry.example/" +
						"catalog.json?signature=fixture-signature#fixture-fragment"
					const diagnostic = "fixture authenticated credential echo"
					client := &http.Client{
						Transport: roundTripFunc(
							func(req *http.Request) (*http.Response, error) {
								user, password, authenticated := req.BasicAuth()
								if !authenticated || user != "fixture-user" || password != "fixture-password" {
									t.Errorf(
										"actual Basic authentication=%q/%q/%v",
										user,
										password,
										authenticated,
									)
								}
								if mode == "signed request failure" {
									return nil, errors.New("fixture network failure")
								}
								return &http.Response{
									StatusCode: http.StatusUnauthorized, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(diagnostic)), Request: req,
								}, nil
							},
						),
					}
					h := &Handler{
						cfg:                    &config.Config{Plugins: config.PluginsConfig{Dir: t.TempDir()}},
						pluginStoreRegistryURL: source, pluginStoreHTTPClient: client,
					}
					method, path, handler := http.MethodGet, "/v0/management/plugin-store", h.ListPluginStore
					if action != "catalog" {
						path += "/sample-provider/releases"
						handler = h.ListPluginStoreReleases
						if strings.HasPrefix(action, "install") {
							method, path, handler = http.MethodPost, "/v0/management/plugin-store/"+
								"sample-provider/"+
								"install", h.InstallPluginFromStore
						}
						if strings.Contains(action, "explicit") {
							path += "?source=" + pluginstore.DefaultSourceID
						}
					}
					response := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(response)
					ctx.Params = gin.Params{{Key: "id", Value: "sample-provider"}}
					ctx.Request = httptest.NewRequest(method, path, nil)
					handler(ctx)
					if response.Code != http.StatusBadGateway {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					for _, secret := range []string{
						"fixture-user", "fixture-password", "fixture-signature", "fixture-fragment", diagnostic,
					} {
						if strings.Contains(response.Body.String(), secret) {
							t.Fatalf("management response disclosed %q: %s", secret, response.Body.String())
						}
					}
					want := "fixture network failure"
					if mode == "authenticated response" {
						want = "unexpected status 401"
					}
					if !strings.Contains(response.Body.String(), want) {
						t.Fatalf("safe diagnostic %q missing: %s", want, response.Body.String())
					}
				},
			)
		}
	}
}

func TestPluginStoreDirectManifestPinsRequestedVersionArtifacts(t *testing.T) {
	plugin := pluginstore.Plugin{
		ID: "sample", Name: "Sample", Description: "Sample plugin", Author: "tester", Version: "1.0.0",
		Install: pluginstore.InstallPlan{
			Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{
				{
					GOOS: "linux", GOARCH: "amd64", URL: "https://downloads.example/sample-1.0.0.zip",
					SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Size: 100,
				},
			},
		},
		Versions: []pluginstore.Version{
			{
				Version: "0.9.0",
				Install: pluginstore.InstallPlan{
					Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{
						{
							GOOS: "linux", GOARCH: "amd64", URL: "https://downloads.example/sample-0.9.0.zip",
							SHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", Size: 90,
						},
					},
				},
			},
		},
	}
	manifest, err := pluginStoreDirectManifest(pluginstore.DefaultSource(), plugin, "0.9.0")
	if err != nil {
		t.Fatalf("pluginStoreDirectManifest() error = %v", err)
	}
	if manifest.Version != "0.9.0" || manifest.InstallType() != pluginstore.InstallTypeDirect ||
		manifest.SourceURL != pluginstore.DefaultSource().URL || len(manifest.Install.Artifacts) != 0 {
		t.Fatalf("manifest=%#v, want historical version/source identity without stored artifact URLs", manifest)
	}
}

func TestListPluginStoreUsesVersionFromInstalledFilename(t *testing.T) {
	t.Parallel()
	pluginsDir := t.TempDir()
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(archDir, "sample-provider-v0.0.1"+testPluginExtension())
	if err := os.WriteFile(pluginPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		cfg:            &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: pluginsDir}},
		configFilePath: tempConfigPath(t), pluginStoreRegistryURL: "https://registry.example/registry.json",
		pluginStoreHTTPClient: pluginStoreResponseClient(
			map[string][]byte{"https://registry.example/registry.json": registryJSON(t)},
		),
	}
	response := runConfigListRequest(h.ListPluginStore, http.MethodGet, "/v0/management/plugin-store", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body pluginStoreListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 1 {
		t.Fatalf("plugins len=%d, want 1", len(body.Plugins))
	}
	entry := body.Plugins[0]
	if !entry.Installed || entry.InstalledVersion != "0.0.1" {
		t.Fatalf("entry=%#v, want installed 0.0.1", entry)
	}
	if !entry.UpdateAvailable {
		t.Fatal("update_available=false, want true for installed 0.0.1 and registry 0.1.0")
	}
}

func TestListPluginStoreUsesConfiguredStoreVersionWhenFilesCoexist(t *testing.T) {
	t.Parallel()
	pluginsDir := t.TempDir()
	archDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pinnedPath := filepath.Join(archDir, "sample-provider-v0.1.0"+testPluginExtension())
	newerPath := filepath.Join(archDir, "sample-provider-v0.2.0"+testPluginExtension())
	for _, path := range []string{pinnedPath, newerPath} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true, Dir: pluginsDir,
				Configs: map[string]config.PluginInstanceConfig{
					"sample-provider": pluginConfigFromYAML(
						t,
						"enabled: true\nstore:\n  version: 0.1.0\n  release-tag: v0.1.0\n",
					),
				},
			},
		},
		configFilePath: tempConfigPath(t), pluginStoreRegistryURL: "https://registry.example/registry.json",
		pluginStoreHTTPClient: pluginStoreResponseClient(
			map[string][]byte{"https://registry.example/registry.json": registryJSON(t)},
		),
	}
	response := runConfigListRequest(h.ListPluginStore, http.MethodGet, "/v0/management/plugin-store", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body pluginStoreListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 1 {
		t.Fatalf("plugins len=%d, want 1", len(body.Plugins))
	}
	entry := body.Plugins[0]
	if !entry.Installed || entry.InstalledVersion != "0.1.0" || entry.Path != pinnedPath {
		t.Fatalf("entry=%#v, want pinned version/path %s", entry, pinnedPath)
	}
	for _, path := range []string{pinnedPath, newerPath} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "x" {
			t.Fatalf("coexisting file=%q err=%v", data, err)
		}
	}
}

func TestInstallPluginFromStoreHonorsDirectQueryVersion(t *testing.T) {
	t.Parallel()
	pluginsDir := t.TempDir()
	archiveData := pluginStoreTestArchive(t, "sample-provider"+testPluginExtension(), "direct-history-data")
	checksum := sha256.Sum256(archiveData)
	topArtifactURL := "https://downloads.example/sample-provider-0.4.0.zip"
	versionArtifactURL := "https://downloads.example/sample-provider-0.3.0.zip"
	registry, err := json.Marshal(
		pluginstore.Registry{
			SchemaVersion: 2, Plugins: []pluginstore.Plugin{
				{
					ID: "sample-provider", Name: "Sample Provider", Description: "Adds sample provider support.",
					Author: "author-name", Version: "0.4.0", AuthRequired: true,
					Install: pluginstore.InstallPlan{
						Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{
							{
								GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, URL: topArtifactURL,
								SHA256: hex.EncodeToString(checksum[:]),
							},
						},
					},
					Versions: []pluginstore.Version{
						{
							Version: "0.3.0", Install: pluginstore.InstallPlan{
								Type: pluginstore.InstallTypeDirect,
								Artifacts: []pluginstore.Artifact{
									{
										GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, URL: versionArtifactURL,
										SHA256: hex.EncodeToString(checksum[:]),
									},
								},
							},
						},
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	responses := pluginStoreResponseClient(
		map[string][]byte{
			"https://registry.example/registry.json": registry, versionArtifactURL: archiveData,
		},
	)
	var downloads []string
	h := &Handler{
		cfg:            &config.Config{Plugins: config.PluginsConfig{Enabled: false, Dir: pluginsDir}},
		configFilePath: tempConfigPath(t), pluginStoreRegistryURL: "https://registry.example/registry.json",
		pluginStoreHTTPClient: fakePluginStoreHTTPClient(
			func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "downloads.example" {
					downloads = append(downloads, req.URL.String())
				}
				return responses.Do(req)
			},
		),
	}
	reloaded := false
	h.configCommitted = func(_ context.Context, commit ConfigCommit) error {
		_, reloaded = commit.Config.Plugins.Configs["sample-provider"]
		return nil
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Params = gin.Params{{Key: "id", Value: "sample-provider"}}
	ctx.Request = httptest.NewRequest(
		http.MethodPost, "/v0/management/plugin-store/sample-provider/install?version=0.3.0", nil,
	)
	h.InstallPluginFromStore(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !reloaded {
		t.Fatal("installed configuration was not delivered to its commit callback")
	}
	var body pluginInstallResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.InstallType != pluginstore.InstallTypeDirect || body.Version != "0.3.0" {
		t.Fatalf("response=%#v, want direct 0.3.0", body)
	}
	targetPath := filepath.Join(
		pluginsDir, runtime.GOOS, runtime.GOARCH, "sample-provider-v0.3.0"+testPluginExtension(),
	)
	if data, err := os.ReadFile(targetPath); err != nil || string(data) != "direct-history-data" {
		t.Fatalf("installed data=%q error=%v", data, err)
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	item := loaded.Plugins.Configs["sample-provider"]
	var manifest pluginstore.Manifest
	if node := yamlMappingValue(&item.Raw, "store"); node == nil {
		t.Fatal("persisted store manifest is missing")
	} else if err = node.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "0.3.0" || manifest.InstallType() != pluginstore.InstallTypeDirect ||
		manifest.SourceURL != "https://registry.example/registry.json" || len(manifest.Install.Artifacts) != 0 {
		t.Fatalf("manifest=%#v, want saved source and direct version 0.3.0", manifest)
	}
	if len(downloads) != 1 || downloads[0] != versionArtifactURL {
		t.Fatalf("artifact downloads=%v, want only requested historical URL %s", downloads, versionArtifactURL)
	}
}

func pluginStoreResponseClient(responses map[string][]byte) fakePluginStoreHTTPClient {
	return func(req *http.Request) (*http.Response, error) {
		data, ok := responses[req.URL.String()]
		if !ok {
			return nil, fmt.Errorf("unexpected synthetic plugin request: %s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)),
			Request: req,
		}, nil
	}
}

func registryJSON(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"schema_version":1,"plugins":[{"id":"sample-provider","name":"Sample ` +
		`Provider","description":"Adds sample provider support.","author":"auth` +
		`or-name","version":"0.1.0","repository":"https://github.com/author-nam` +
		`e/cliproxy-sample-provider-plugin","tags":["provider"]}]}`)
}
