package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/gin-gonic/gin"
)

func TestValidatePluginStoreInstallSourceUsesCaseInsensitivePluginID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	configs := map[string]config.PluginInstanceConfig{
		"Alpha": pluginConfigFromYAML(
			t,
			"enabled: true\nstore:\n  id: alpha\n  source-id: official\n  source-url: https://official.example/registry.json\n",
		),
	}
	sources := []pluginstore.Source{
		{ID: "official", URL: "https://official.example/registry.json"},
		{ID: "community", URL: "https://community.example/registry.json"},
	}

	if validatePluginStoreInstallSource(ctx, configs, sources, "alpha", "community") {
		t.Fatal("case-variant plugin id bypassed source ownership")
	}
	if recorder.Code != http.StatusConflict || !strings.Contains(
		recorder.Body.String(), "plugin_store_source_conflict",
	) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPluginConfigMutationAndDeletionUseCaseInsensitivePluginID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("{}\n"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile(config) error = %v", errWrite)
	}
	handler := NewHandler(
		&config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     filepath.Join(root, "plugins"),
				Configs: map[string]config.PluginInstanceConfig{
					"Alpha": pluginConfigFromYAML(t, "enabled: true\nmode: safe\n"),
				},
			},
		}, configPath, nil,
	)
	t.Cleanup(func() { _ = handler.Close() })

	patchRecorder := httptest.NewRecorder()
	patchCtx, _ := gin.CreateTestContext(patchRecorder)
	patchCtx.Params = []gin.Param{{Key: "id", Value: "ALPHA"}}
	patchCtx.Request = httptest.NewRequest(http.MethodPatch, "/plugins/ALPHA", strings.NewReader(`{"enabled":false}`))
	patchCtx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchPluginEnabled(patchCtx)
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchRecorder.Code, patchRecorder.Body.String())
	}
	configs := handler.configSnapshot().Plugins.Configs
	item, exists := configs["alpha"]
	if !exists || item.Enabled == nil || *item.Enabled {
		t.Fatalf("configs after patch = %#v, want disabled alpha", configs)
	}
	if _, exists := configs["Alpha"]; exists {
		t.Fatalf("mixed-case config remains after patch: %#v", configs)
	}

	deleteRecorder := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRecorder)
	deleteCtx.Params = []gin.Param{{Key: "id", Value: "Alpha"}}
	deleteCtx.Request = httptest.NewRequest(http.MethodDelete, "/plugin-store/Alpha", nil)
	handler.DeletePlugin(deleteCtx)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if configs := handler.configSnapshot().Plugins.Configs; len(configs) != 0 {
		t.Fatalf("configs after delete = %#v, want empty", configs)
	}
}
