package homeplugins

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPrepareSyncWithReportResolvesTildePluginDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     "~/.cli-proxy-api/plugins",
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}

	prepared, errPrepare := PrepareSyncWithReport(context.Background(), cfg)
	if errPrepare != nil {
		t.Fatalf("PrepareSyncWithReport() error = %v", errPrepare)
	}
	defer func() { _ = prepared.Close() }()
	want := filepath.Join(homeDir, ".cli-proxy-api", "plugins")
	if prepared.root != want {
		t.Fatalf("prepared.root = %q, want %q", prepared.root, want)
	}
}

func TestDeleteWithReportRejectsUnsupportedUserPluginsDir(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	literalRoot := filepath.Join(workspace, "~other", "plugins", runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(literalRoot, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(literalRoot, "sample"+homePluginExtension(runtime.GOOS))
	if errWrite := os.WriteFile(target, []byte("plugin"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	cfg := &config.Config{Plugins: config.PluginsConfig{Dir: "~other/plugins"}}

	report := DeleteWithReport(context.Background(), cfg, nil, 41, "sample")

	if report.OK || len(report.Plugins) != 1 || report.Plugins[0].InstallStatus != pluginInstallStatusFailed {
		t.Fatalf("report = %+v, want failed delete", report)
	}
	if !strings.Contains(report.Plugins[0].Error, "unsupported user home path") {
		t.Fatalf("error = %q, want unsupported user home path", report.Plugins[0].Error)
	}
	if _, errStat := os.Stat(target); errStat != nil {
		t.Fatalf("literal plugin target was touched: %v", errStat)
	}
}
