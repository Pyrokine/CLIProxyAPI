package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestStartServiceBackgroundRejectsUnsupportedUserPluginsDir(t *testing.T) {
	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    8317,
		AuthDir: t.TempDir(),
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     "~other/plugins",
		},
	}
	cancel, done := StartServiceBackgroundWithPluginHost(
		cfg,
		filepath.Join(t.TempDir(), "config.yaml"),
		"",
		nil,
	)
	defer cancel()
	errBuild := <-done
	if errBuild == nil || !strings.Contains(errBuild.Error(), "unsupported user home path") {
		t.Fatalf("startup error = %v, want unsupported user home path", errBuild)
	}
}
