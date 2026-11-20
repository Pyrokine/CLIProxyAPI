package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func writeVersionedPluginFile(t *testing.T, root, id, version string) string {
	t.Helper()
	dir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+"-v"+version+pluginExtension(runtime.GOOS))
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func enabledPluginConfigWithStoreVersion(t *testing.T, version string) config.PluginInstanceConfig {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("store:\n  version: "+version+"\n"), &node); err != nil {
		t.Fatal(err)
	}
	return config.PluginInstanceConfig{Enabled: new(true), Raw: *node.Content[0]}
}

func TestRuntimeConfigOriginalStoreVersionInputs(t *testing.T) {
	for _, raw := range []string{
		"store:\n  version: 1.0.3\n  release-tag: v1.0.3\n",
		"store:\n  release-tag: v1.0.3\n",
	} {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(raw), &node); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Configs: map[string]config.PluginInstanceConfig{"alpha": {Enabled: new(true), Raw: *node.Content[0]}},
			},
		}
		got, err := runtimeConfigFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got.Items["alpha"].Version != "1.0.3" || desiredPluginVersions(got.Items)["alpha"] != "1.0.3" {
			t.Fatalf("store version input %q produced %+v", raw, got.Items["alpha"])
		}
	}
}

func TestSelectPluginFilesOriginalVersionInputs(t *testing.T) {
	root := t.TempDir()
	older := writeVersionedPluginFile(t, root, "alpha", "1.0.3")
	newer := writeVersionedPluginFile(t, root, "alpha", "1.0.4")
	for _, test := range []struct {
		name, desired, wantPath, wantVersion string
	}{
		{name: "pinned lower", desired: "1.0.3", wantPath: older, wantVersion: "1.0.3"},
		{name: "highest", wantPath: newer, wantVersion: "1.0.4"},
		{name: "missing", desired: "1.0.5"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				files, err := selectPluginFiles(root, map[string]string{"alpha": test.desired})
				if err != nil {
					t.Fatal(err)
				}
				if test.wantPath == "" {
					if len(files) != 0 {
						t.Fatalf("missing pinned version selected %+v", files)
					}
					return
				}
				if len(files) != 1 || files[0] != (pluginFile{
					ID: "alpha", Path: test.wantPath, Version: test.wantVersion,
				}) {
					t.Fatalf("selected %+v, want %s", files, test.wantPath)
				}
			},
		)
	}
}

func TestHostConfiguredVersionLifecycleAndHotReloadLogs(t *testing.T) {
	for _, targetVersion := range []string{"1.0.3", "1.0.5"} {
		t.Run(
			targetVersion, func(t *testing.T) {
				root := t.TempDir()
				initialPath := writeVersionedPluginFile(t, root, "alpha", "1.0.4")
				plugin := &testPlugin{
					registerResult: validTestPlugin("alpha"), reconfigureResult: validTestPlugin("alpha"),
				}
				loader := newTestSymbolLoader()
				loader.lookups["alpha"] = newTestSymbolLookup(plugin)
				host := newForTest(loader)
				t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
				cfg := &config.Config{
					Plugins: config.PluginsConfig{
						Enabled: true, Dir: root,
						Configs: map[string]config.PluginInstanceConfig{
							"alpha": enabledPluginConfigWithStoreVersion(
								t, "1.0.4",
							),
						},
					},
				}
				if err := host.ApplyConfig(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				if !host.pluginIdentityCurrent("alpha", initialPath, "1.0.4") {
					t.Fatal("initial version identity was not published")
				}
				cfg = &config.Config{
					Plugins: config.PluginsConfig{
						Enabled: true, Dir: root,
						Configs: map[string]config.PluginInstanceConfig{
							"alpha": enabledPluginConfigWithStoreVersion(
								t, targetVersion,
							),
						},
					},
				}
				if targetVersion == "1.0.5" {
					if err := host.ApplyConfig(t.Context(), cfg); err != nil {
						t.Fatal(err)
					}
					if !host.PluginRegistered("alpha") || !host.pluginIdentityCurrent("alpha", initialPath, "1.0.4") ||
						loader.openCalls != 2 || plugin.registerCalls != 2 || plugin.reconfigureCalls != 0 {
						t.Fatal("missing pinned version lost the prior version or bypassed isolated config publication")
					}
					if err := host.ApplyConfig(t.Context(), cfg); err != nil {
						t.Fatal(err)
					}
					if loader.openCalls != 2 || plugin.registerCalls != 2 || plugin.reconfigureCalls != 0 {
						t.Fatal("unchanged missing-version configuration reloaded the active plugin")
					}
				}
				targetPath := writeVersionedPluginFile(t, root, "alpha", targetVersion)
				capture, err := os.CreateTemp(t.TempDir(), "plugin-log-")
				if err != nil {
					t.Fatal(err)
				}
				logger := log.StandardLogger()
				originalOutput, originalFormatter, originalLevel := logger.Out, logger.Formatter, logger.Level
				log.SetOutput(capture)
				log.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})
				log.SetLevel(log.InfoLevel)
				defer func() {
					log.SetOutput(originalOutput)
					log.SetFormatter(originalFormatter)
					log.SetLevel(originalLevel)
					if err := capture.Close(); err != nil {
						t.Error(err)
					}
				}()
				if err := host.ApplyConfig(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				wantOpenCalls := 2
				if targetVersion == "1.0.5" {
					wantOpenCalls = 3
				}
				if !host.pluginIdentityCurrent("alpha", targetPath, targetVersion) ||
					host.pluginIdentityCurrent("alpha", initialPath, "1.0.4") || loader.openCalls != wantOpenCalls {
					t.Fatal("available pinned version did not replace the loaded version")
				}
				data, err := os.ReadFile(capture.Name())
				if err != nil {
					t.Fatal(err)
				}
				text := string(data)
				if strings.Count(text, `msg="pluginhost: plugin hot reloaded"`) != 1 {
					t.Fatalf("hot reload log count: %s", text)
				}
				for _, want := range []string{
					"plugin_id=alpha", "active_version=" + targetVersion,
					"retired_version=1.0.4", "active_path=" + targetPath, "retired_path=" + initialPath,
				} {
					if !strings.Contains(text, want) {
						t.Fatalf("hot reload log missing %q: %s", want, text)
					}
				}
			},
		)
	}
}

func TestPluginHotReloadLogFieldsIncludesActiveAndRetiredIdentity(t *testing.T) {
	fields := pluginHotReloadLogFields(
		"sample", "0.1.0", "/tmp/plugins/sample-v0.1.0.dll",
		"0.2.0", "/tmp/plugins/sample-v0.2.0.dll",
	)
	for key, want := range map[string]string{
		"plugin_id": "sample", "active_version": "0.1.0", "retired_version": "0.2.0",
		"active_path": "/tmp/plugins/sample-v0.1.0.dll", "retired_path": "/tmp/plugins/sample-v0.2.0.dll",
	} {
		if fields[key] != want {
			t.Fatalf("%s=%v, want %s", key, fields[key], want)
		}
	}
}
