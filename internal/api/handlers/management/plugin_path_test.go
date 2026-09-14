package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestListPluginStoreRejectsUnsupportedUserPluginsDirBeforeFetchingRegistry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{Plugins: config.PluginsConfig{Dir: "~other/plugins"}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugin-store", nil)

	h.ListPluginStore(ctx)

	assertPluginDirectoryInvalid(t, recorder)
}

func TestListPluginsRejectsUnsupportedUserPluginsDir(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{Plugins: config.PluginsConfig{Dir: "~other/plugins"}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugins", nil)

	h.ListPlugins(ctx)

	assertPluginDirectoryInvalid(t, recorder)
}

func TestDeletePluginRejectsUnsupportedUserPluginsDirWithoutTouchingLiteralPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	workspace := t.TempDir()
	t.Chdir(workspace)
	literalRoot := filepath.Join(workspace, "~other", "plugins", runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(literalRoot, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(literalRoot, "sample"+testPluginExtension())
	if errWrite := os.WriteFile(target, []byte("plugin"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	h := &Handler{cfg: &config.Config{Plugins: config.PluginsConfig{Dir: "~other/plugins"}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "sample"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample", nil)

	h.DeletePlugin(ctx)

	assertPluginDirectoryInvalid(t, recorder)
	if _, errStat := os.Stat(target); errStat != nil {
		t.Fatalf("literal plugin target was touched: %v", errStat)
	}
}

func assertPluginDirectoryInvalid(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	var body map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("Unmarshal() error = %v", errDecode)
	}
	if body["error"] != "plugin_directory_invalid" {
		t.Fatalf("error = %#v, want plugin_directory_invalid", body["error"])
	}
}
