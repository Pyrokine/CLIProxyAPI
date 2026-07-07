package homeplugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

type fakePluginRuntime struct {
	busy         bool
	unloadResult bool
	unloadCalls  int
}

func (r *fakePluginRuntime) PluginBusy(string) bool {
	return r.busy
}

func (r *fakePluginRuntime) UnloadPlugin(string) bool {
	r.unloadCalls++
	if r.unloadResult {
		r.busy = false
	}
	return r.unloadResult
}

type fakePluginLoadInspector struct {
	registered map[string]bool
}

func (i fakePluginLoadInspector) PluginRegistered(id string) bool {
	return i.registered[id]
}

func TestStoreManifestFromPluginConfigUsesConfigID(t *testing.T) {
	t.Parallel()

	item := pluginConfigFromYAML(
		t, `
enabled: true
store:
  schema-version: 2
  version: 0.2.0
  source-url: https://example.com/plugins/sample.so
  install:
    type: direct
`,
	)

	manifest, ok, err := storeManifestFromPluginConfig("sample-provider", item)
	if err != nil {
		t.Fatalf("storeManifestFromPluginConfig() error = %v", err)
	}
	if !ok {
		t.Fatalf("storeManifestFromPluginConfig() ok = false, want true")
	}
	if manifest.ID != "sample-provider" {
		t.Fatalf("manifest.ID = %q, want sample-provider", manifest.ID)
	}
	if manifest.Version != "0.2.0" {
		t.Fatalf("manifest.Version = %q, want 0.2.0", manifest.Version)
	}
}

func TestMarkLoadResultsReportsUnloadedPlugin(t *testing.T) {
	t.Parallel()

	report := SyncReport{
		Plugins: []PluginInstallStatus{
			{ID: "loaded-plugin", InstallStatus: pluginInstallStatusInstalled},
			{ID: "missing-plugin", InstallStatus: pluginInstallStatusInstalled},
		},
	}

	err := MarkLoadResults(&report, fakePluginLoadInspector{registered: map[string]bool{"loaded-plugin": true}})
	if err == nil {
		t.Fatalf("MarkLoadResults() error = nil, want load error")
	}
	if report.OK {
		t.Fatalf("report.OK = true, want false")
	}
	if report.Phase != pluginTaskPhaseLoad {
		t.Fatalf("report.Phase = %q, want %q", report.Phase, pluginTaskPhaseLoad)
	}
	if report.Plugins[0].LoadStatus != pluginLoadStatusLoaded {
		t.Fatalf("loaded plugin LoadStatus = %q, want %q", report.Plugins[0].LoadStatus, pluginLoadStatusLoaded)
	}
	if report.Plugins[1].LoadStatus != pluginLoadStatusFailed {
		t.Fatalf("missing plugin LoadStatus = %q, want %q", report.Plugins[1].LoadStatus, pluginLoadStatusFailed)
	}
	if !strings.Contains(report.Plugins[1].Error, "installed but not loaded") {
		t.Fatalf("missing plugin Error = %q, want installed but not loaded", report.Plugins[1].Error)
	}
}

func TestDeleteWithReportUnloadsBusyPluginBeforeDeletingArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pluginPath := writePluginFile(t, root, "sample", "0.2.0")
	runtime := &fakePluginRuntime{busy: true, unloadResult: true}
	cfg := &config.Config{Plugins: config.PluginsConfig{Dir: root}}

	report := DeleteWithReport(context.Background(), cfg, runtime, 42, "sample")

	if !report.OK {
		t.Fatalf("report.OK = false, want true: %s", report.Error)
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) error = %v, want not exist", pluginPath, err)
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("len(report.Plugins) = %d, want 1", len(report.Plugins))
	}
	status := report.Plugins[0]
	if status.InstallStatus != pluginInstallStatusDeleted {
		t.Fatalf("InstallStatus = %q, want %q", status.InstallStatus, pluginInstallStatusDeleted)
	}
	if status.Path != pluginPath {
		t.Fatalf("Path = %q, want %q", status.Path, pluginPath)
	}
}

func TestDeleteWithReportKeepsBusyPluginWhenUnloadFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pluginPath := writePluginFile(t, root, "sample", "0.2.0")
	runtime := &fakePluginRuntime{busy: true, unloadResult: false}
	cfg := &config.Config{Plugins: config.PluginsConfig{Dir: root}}

	report := DeleteWithReport(context.Background(), cfg, runtime, 7, "sample")

	if report.OK {
		t.Fatalf("report.OK = true, want false")
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("Stat(%s) error = %v, want kept", pluginPath, err)
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("len(report.Plugins) = %d, want 1", len(report.Plugins))
	}
	status := report.Plugins[0]
	if status.InstallStatus != pluginInstallStatusFailed {
		t.Fatalf("InstallStatus = %q, want %q", status.InstallStatus, pluginInstallStatusFailed)
	}
	if !strings.Contains(status.Error, "loaded plugin") {
		t.Fatalf("Error = %q, want loaded plugin", status.Error)
	}
}

func TestPreparePluginOverwriteUnloadsBusyPlugin(t *testing.T) {
	t.Parallel()

	runtime := &fakePluginRuntime{busy: true, unloadResult: true}

	if err := preparePluginOverwrite("sample", runtime); err != nil {
		t.Fatalf("preparePluginOverwrite() error = %v", err)
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if runtime.busy {
		t.Fatal("runtime.busy = true, want false")
	}
}

func TestPreparePluginOverwriteRejectsStillBusyPlugin(t *testing.T) {
	t.Parallel()

	runtime := &fakePluginRuntime{busy: true, unloadResult: false}

	err := preparePluginOverwrite("sample", runtime)
	if err == nil {
		t.Fatal("preparePluginOverwrite() error = nil, want error")
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("runtime.unloadCalls = %d, want 1", runtime.unloadCalls)
	}
	if !strings.Contains(err.Error(), "loaded plugin") {
		t.Fatalf("preparePluginOverwrite() error = %v, want loaded plugin", err)
	}
}

func pluginConfigFromYAML(t *testing.T, raw string) config.PluginInstanceConfig {
	t.Helper()
	var item config.PluginInstanceConfig
	if err := yaml.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	return item
}

func writePluginFile(t *testing.T, root string, id string, version string) string {
	t.Helper()
	dir := filepath.Join(root, CurrentPlatform().GOOS, CurrentPlatform().GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", dir, err)
	}
	path := filepath.Join(dir, id+"-v"+version+pluginExtension(CurrentPlatform().GOOS))
	if err := os.WriteFile(path, []byte("plugin"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}
