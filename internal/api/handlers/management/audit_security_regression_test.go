package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/gin-gonic/gin"
)

func TestManagementBuildHeadersRequireAuthentication(t *testing.T) {
	h := NewHandler(
		&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil, withEnvSecret("synthetic-admin-key"),
	)
	defer func() { _ = h.Close() }()
	engine := gin.New()
	engine.GET("/test", h.middleware(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, authenticated := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		if authenticated {
			req.Header.Set("X-Management-Key", "synthetic-admin-key")
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		if authenticated {
			if recorder.Code != http.StatusNoContent || recorder.Header().Get(pluginhost.SupportPluginHeader) == "" {
				t.Fatalf("authenticated response=%d headers=%v", recorder.Code, recorder.Header())
			}
		} else {
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d", recorder.Code)
			}
			for _, header := range []string{
				"X-CPA-VERSION", "X-CPA-COMMIT", "X-CPA-BUILD-DATE", pluginhost.SupportPluginHeader,
			} {
				if _, present := recorder.Header()[http.CanonicalHeaderKey(header)]; present {
					t.Errorf("unauthenticated response exposed %s", header)
				}
			}
		}
	}
}

func TestDownloadAuthFileRejectsSymlinkEscapeAndFollowsConfiguredDirectory(t *testing.T) {
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"marker":"outside-boundary"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(firstDir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(secondDir, "valid.json"),
		[]byte(`{"marker":"configured-directory","private_key":"secret-value"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: firstDir}
	h := NewHandler(cfg, filepath.Join(t.TempDir(), "config.yaml"), nil, withEnvSecret(""))
	defer func() { _ = h.Close() }()
	engine := gin.New()
	engine.GET("/download", h.DownloadAuthFile)
	request := func(name string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/download?name="+name, nil))
		return recorder
	}
	escaped := request("linked.json")
	if escaped.Code == http.StatusOK || strings.Contains(escaped.Body.String(), "outside-boundary") {
		t.Fatalf("symlink escaped root: %d %s", escaped.Code, escaped.Body.String())
	}
	next := *cfg
	next.AuthDir = secondDir
	h.SetConfig(&next)
	valid := request("valid.json")
	if valid.Code != http.StatusOK || !strings.Contains(
		valid.Body.String(), "configured-directory",
	) || strings.Contains(valid.Body.String(), "secret-value") {
		t.Fatalf("configured directory download=%d %s", valid.Code, valid.Body.String())
	}
}
