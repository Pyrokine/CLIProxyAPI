package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestGetVersionIncludesFeatureCapabilities(t *testing.T) {
	h := NewHandler(&config.Config{}, "", nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/version", nil)

	h.GetVersion(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("GetVersion status = %d", rec.Code)
	}
	var body struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal version response: %v", err)
	}
	if !body.Features["plugins"] {
		t.Fatalf("features.plugins = false, want true")
	}
}

func newConfigBasicTestHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return NewHandler(cfg, path, nil, WithEnvSecret(""))
}

func TestPutProxyURLAllowsPrivateAndRejectsInvalidScheme(t *testing.T) {
	cfg := &config.Config{}
	h := newConfigBasicTestHandler(t, cfg)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/config/proxy-url",
		bytes.NewBufferString(`{"value":"http://127.0.0.1:8080"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyURL(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("private proxy status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if cfg.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("proxy URL = %q", cfg.ProxyURL)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/config/proxy-url",
		bytes.NewBufferString(`{"value":"file:///tmp/socket"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyURL(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid proxy status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if cfg.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("proxy URL after invalid update = %q", cfg.ProxyURL)
	}
}
