package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

func TestPluginHostPublishedContextualUnloader(t *testing.T) {
	var host interface {
		UnloadPluginContext(context.Context, string) bool
	} = NewPluginHost()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if host.UnloadPluginContext(ctx, "missing") {
		t.Fatal("canceled missing plugin reported an unload")
	}
	if (*PluginHost)(nil).UnloadPluginContext(t.Context(), "missing") {
		t.Fatal("nil plugin host reported an unload")
	}
}

func TestServiceHomePluginDeleteTaskPreservesCanceledArtifacts(t *testing.T) {
	root := t.TempDir()
	extension := ".so"
	switch runtime.GOOS {
	case "windows":
		extension = ".dll"
	case "darwin":
		extension = ".dylib"
	}
	path := filepath.Join(root, "sample-v1.0.0"+extension)
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Service{pluginHost: NewPluginHost().internal()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report := service.processHomePluginDeleteTask(
		ctx,
		&config.Config{Plugins: config.PluginsConfig{Dir: root}},
		home.PluginTask{ID: 44, Operation: "delete", PluginID: "sample"},
	)
	if report.OK || report.TaskID != 44 || !strings.Contains(report.Error, context.Canceled.Error()) {
		t.Fatalf("canceled Home task report=%+v", report)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("canceled Home task removed the plugin: %v", err)
	}
}
