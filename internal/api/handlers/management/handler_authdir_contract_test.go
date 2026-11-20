package management

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestStandaloneHandlerAuthDirContract(t *testing.T) {
	for _, input := range []string{"~/tokens", "tokens", "."} {
		t.Run(
			input, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Setenv("WRITABLE_PATH", "")
				t.Setenv("writable_path", "")
				t.Chdir(root)
				dir := filepath.Join(root, "tokens")
				if input == "." {
					dir = root
				}
				store := sdkAuth.NewFileTokenStore()
				manager := coreauth.NewManager(store, nil, nil)
				cfg := &config.Config{AuthDir: input}
				h := NewHandlerWithoutConfigFilePath(cfg, manager, withTokenStore(store))
				t.Cleanup(func() { _ = h.Close() })
				if got := h.authDirSnapshot(); got != dir {
					t.Fatalf("handler directory = %q, want %q", got, dir)
				}
				if cfg.AuthDir != input {
					t.Fatalf("constructor changed caller config: %q", cfg.AuthDir)
				}
				const name = "synthetic.json"
				const credential = `{"type":"codex","access_token":"synthetic-token","account_id":"synthetic-account"}`
				invoke := func(method, target, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
					t.Helper()
					response := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(response)
					ctx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
					ctx.Request.Header.Set("Content-Type", "application/json")
					handler(ctx)
					return response
				}
				if response := invoke(
					http.MethodPost, "/auth-files?name="+name, credential, h.UploadAuthFile,
				); response.Code != http.StatusOK {
					t.Fatalf("upload = %d: %s", response.Code, response.Body.String())
				}
				path := filepath.Join(dir, name)
				if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "synthetic-token") {
					t.Fatalf("uploaded credential = %s, error = %v", data, err)
				}
				if auth, ok := manager.GetByID(name); !ok || auth.ID != name ||
					auth.Attributes[coreauth.AttributePath] != path {
					t.Fatalf("runtime credential identity = %#v, present = %t", auth, ok)
				}
				if response := invoke(
					http.MethodGet, "/auth-files/download?name="+name, "", h.DownloadAuthFile,
				); response.Code != http.StatusOK || !strings.Contains(
					response.Body.String(), "synthetic-account",
				) || strings.Contains(response.Body.String(), "synthetic-token") {
					t.Fatalf("download = %d: %s", response.Code, response.Body.String())
				}
				h.SetAuthManager(nil)
				if response := invoke(
					http.MethodGet, "/auth-files", "", h.ListAuthFiles,
				); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), name) {
					t.Fatalf("disk list = %d: %s", response.Code, response.Body.String())
				}
				h.SetAuthManager(manager)
				if response := invoke(
					http.MethodDelete, "/auth-files?name="+name, "", h.DeleteAuthFile,
				); response.Code != http.StatusOK {
					t.Fatalf("delete = %d: %s", response.Code, response.Body.String())
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("deleted credential remains: %v", err)
				}
				if _, ok := manager.GetByID(name); ok {
					t.Fatal("deleted credential remains registered")
				}
				if _, err := os.Stat(filepath.Join(root, "~")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("literal tilde directory exists: %v", err)
				}
			},
		)
	}
}

func TestHandlerAuthDirResolutionFailureAndRecovery(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	t.Chdir(t.TempDir())
	var logs bytes.Buffer
	logger := log.StandardLogger()
	originalOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(originalOutput) })
	cfg := &config.Config{AuthDir: "~/tokens"}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	t.Cleanup(func() { _ = h.Close() })
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)
	h.ListAuthFiles(ctx)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(
		logs.String(), "resolve auth dir",
	) || cfg.AuthDir != "~/tokens" {
		t.Fatalf(
			"directory failure was hidden: status = %d, logs = %s, caller directory = %q", response.Code, logs.String(),
			cfg.AuthDir,
		)
	}
	t.Setenv("HOME", t.TempDir())
	h.SetConfig(cfg)
	if dir := h.authDirSnapshot(); !filepath.IsAbs(dir) || strings.Contains(dir, "~") {
		t.Fatalf("replacement config did not restore the directory: %q", dir)
	}
}
