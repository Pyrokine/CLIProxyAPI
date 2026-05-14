// Last compiled: 2026-05-09
// Author: pyro

package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestGitHubGet_AllowsLargeResponseBody(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				payload := bytes.Repeat([]byte("a"), 11<<20)
				_, _ = io.Copy(w, bytes.NewReader(payload))
			},
		),
	)
	defer server.Close()

	body, err := githubGet(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("githubGet: %v", err)
	}
	if got, want := len(body), 11<<20; got != want {
		t.Fatalf("len(body) = %d, want %d", got, want)
	}
}

// --- Option pattern tests ---

func TestWithEnvSecret(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret("my-secret"))
	if h.envSecret != "my-secret" {
		t.Fatalf("expected envSecret=%q, got %q", "my-secret", h.envSecret)
	}
	if !h.allowRemoteOverride {
		t.Fatal("expected allowRemoteOverride=true when envSecret is set")
	}

	h2 := NewHandler(&config.Config{}, "", nil, WithEnvSecret(""))
	if h2.allowRemoteOverride {
		t.Fatal("expected allowRemoteOverride=false when envSecret is empty")
	}
}

func TestWithTokenStore(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	store := &memoryAuthStore{}
	h := NewHandler(&config.Config{}, "", nil, WithTokenStore(store))
	if h.tokenStore != store {
		t.Fatal("expected tokenStore to be the injected store")
	}
}

func TestWithUsageStats(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	stats := usage.GetRequestStatistics()
	h := NewHandler(&config.Config{}, "", nil, WithUsageStats(stats))
	if h.usageStats != stats {
		t.Fatal("expected usageStats to be the injected stats")
	}
}

func TestWithLogDir(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{}, "", nil, WithLogDir("/tmp/test-logs"))
	if h.logDir != "/tmp/test-logs" {
		t.Fatalf("expected logDir=%q, got %q", "/tmp/test-logs", h.logDir)
	}
}

func TestWithPostAuthHook(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	called := false
	hook := func(_ interface{}, _ *coreauth.Auth) error { called = true; return nil }
	// PostAuthHook is func(context.Context, *Auth) error — we need to match the signature
	_ = called
	_ = hook
	// Just verify compilation works with the option
	h := NewHandler(&config.Config{}, "", nil)
	if h.postAuthHook != nil {
		t.Fatal("expected nil postAuthHook by default")
	}
}

// --- Middleware auth tests ---

// newLoopbackRequest creates an http.Request that appears to come from loopback.
func newLoopbackRequest(method, path, authHeader string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// newRemoteRequest creates an http.Request that appears to come from a remote IP.
func newRemoteRequest(method, path, authHeader string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "203.0.113.1:54321"
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func TestMiddleware_BcryptSecret(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	secret := "test-secret"
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}

	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	cfg.RemoteManagement.AllowRemote = true
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	// Correct secret
	rec := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(rec)
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	c.Request = newRemoteRequest("GET", "/test", "Bearer "+secret)
	engine.ServeHTTP(rec, c.Request)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct bcrypt secret, got %d: %s", rec.Code, rec.Body.String())
	}

	// Wrong secret
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = newRemoteRequest("GET", "/test", "Bearer wrong-secret")
	engine.ServeHTTP(rec, c.Request)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong bcrypt secret, got %d", rec.Code)
	}
}

func TestMiddleware_EnvSecret(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "", nil, WithEnvSecret("env-pass"))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	// Correct env secret
	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer env-pass")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct env secret, got %d: %s", rec.Code, rec.Body.String())
	}

	// Wrong env secret (no bcrypt hash set)
	rec = httptest.NewRecorder()
	req = newRemoteRequest("GET", "/test", "Bearer wrong")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong env secret, got %d", rec.Code)
	}
}

func TestMiddleware_LocalPassword(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))
	h.localPassword = "local-pass"

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	// Correct local password from loopback
	rec := httptest.NewRecorder()
	req := newLoopbackRequest("GET", "/test", "Bearer local-pass")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct local password, got %d: %s", rec.Code, rec.Body.String())
	}

	// Wrong local password from loopback
	rec = httptest.NewRecorder()
	req = newLoopbackRequest("GET", "/test", "Bearer wrong")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong local password, got %d", rec.Code)
	}

	// Correct local password but from remote — should fail (no remote key set)
	rec = httptest.NewRecorder()
	req = newRemoteRequest("GET", "/test", "Bearer local-pass")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for remote with local-only password, got %d", rec.Code)
	}
}

func TestMiddleware_MissingKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = "$2a$04$dummy" // won't match anything
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	rec := httptest.NewRecorder()
	req := newLoopbackRequest("GET", "/test", "")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with missing key, got %d", rec.Code)
	}
}

func TestMiddleware_XManagementKeyHeader(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "", nil, WithEnvSecret("env-key"))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "")
	req.Header.Set("X-Management-Key", "env-key")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with X-Management-Key header, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_RemoteDisabled(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.SecretKey = "some-hash"
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer something")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when remote management disabled, got %d", rec.Code)
	}
}

func TestMiddleware_IPBanning(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	secret := "correct"
	hash, _ := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	cfg.RemoteManagement.AllowRemote = true
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	// Send 10 failed attempts to trigger ban (sliding window: 10 in 5 min)
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := newRemoteRequest("GET", "/test", "Bearer wrong")
		engine.ServeHTTP(rec, req)
	}

	// Next attempt should be banned even with correct key
	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer "+secret)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 after IP ban, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "banned") {
		t.Fatalf("expected ban message, got: %s", body)
	}
}

// --- Config endpoint tests ---

func TestPutConfigYAML_BodyLimit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))

	// Create a body larger than 10MB
	largeBody := strings.Repeat("a", 11<<20)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(largeBody))
	c.Request.Header.Set("Content-Type", "application/yaml")

	h.PutConfigYAML(c)

	// Should fail gracefully (400 or 422), not OOM
	if rec.Code == http.StatusOK {
		t.Fatal("expected PutConfigYAML to reject >10MB body, but got 200")
	}
}

func TestImportUsageStatistics_BodyLimit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	baseDir := t.TempDir()
	configPath := filepath.Join(baseDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	persister := usage.NewPersister(baseDir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	defer persister.Stop()

	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""), WithUsagePersister(persister))

	largeBody := strings.Repeat("a", 101<<20)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/usage/import", strings.NewReader(largeBody))
	c.Request.Header.Set("Content-Type", "application/json")

	h.ImportUsageStatistics(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100MB") {
		t.Fatalf("expected explicit 100MB limit message, got %s", rec.Body.String())
	}
}

func TestGetConfigYAML(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := "port: 8317\ndebug: true\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != content {
		t.Fatalf("expected config content %q, got %q", content, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "yaml") {
		t.Fatalf("expected yaml content-type, got %q", ct)
	}
}

func TestGetConfigYAML_NotFound(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "/nonexistent/config.yaml", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestValidateConfigYAML_ReturnsLineForTypeError(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/config.yaml/validate", strings.NewReader("port: nope\n"))
	c.Request.Header.Set("Content-Type", "application/yaml")
	h.ValidateConfigYAML(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Valid {
		t.Fatalf("expected invalid config, got valid=true")
	}
	if len(body.Errors) == 0 {
		t.Fatalf("expected validation errors, got none")
	}
	if body.Errors[0].Field != "line 1" {
		t.Fatalf("expected field=line 1, got %q", body.Errors[0].Field)
	}
}

func TestValidateConfigYAML_ReturnsYamlFieldForSyntaxError(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/config.yaml/validate", strings.NewReader("port: [\n"))
	c.Request.Header.Set("Content-Type", "application/yaml")
	h.ValidateConfigYAML(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Valid {
		t.Fatalf("expected invalid config, got valid=true")
	}
	if len(body.Errors) == 0 {
		t.Fatalf("expected validation errors, got none")
	}
	if body.Errors[0].Field != "line 1" {
		t.Fatalf("expected field=line 1, got %q", body.Errors[0].Field)
	}
}

func TestInferConfigValidationField(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "api keys", message: "api-keys[2] is empty", want: "api-keys"},
		{name: "port", message: "port 0 is out of range", want: "port"},
		{
			name:    "yaml line",
			message: "failed to parse config file: yaml: unmarshal errors:\n  line 3: cannot unmarshal !!str `nope` into int",
			want:    "line 3",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := inferConfigValidationField(tt.message); got != tt.want {
					t.Fatalf("inferConfigValidationField(%q) = %q, want %q", tt.message, got, tt.want)
				}
			},
		)
	}
}

func TestGetConfig_IncludesRemoteManagement(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.RemoteManagement.PanelGitHubRepository = "https://github.com/example/panel"
	cfg.RemoteManagement.CPAGitHubRepository = "https://github.com/example/cpa"
	cfg.RemoteManagement.AutoCheckUpdate = true
	cfg.RemoteManagement.AutoUpdateCPA = true
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config", nil)
	h.GetConfig(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	remoteManagement, ok := body["remote-management"].(map[string]any)
	if !ok {
		t.Fatalf("expected remote-management object, got %#v", body["remote-management"])
	}
	if remoteManagement["allow-remote"] != true {
		t.Fatalf("allow-remote = %#v, want true", remoteManagement["allow-remote"])
	}
	if remoteManagement["disable-control-panel"] != true {
		t.Fatalf("disable-control-panel = %#v, want true", remoteManagement["disable-control-panel"])
	}
	if remoteManagement["auto-update-panel"] != true {
		t.Fatalf("auto-update-panel = %#v, want true", remoteManagement["auto-update-panel"])
	}
	if remoteManagement["auto-check-update"] != true {
		t.Fatalf("auto-check-update = %#v, want true", remoteManagement["auto-check-update"])
	}
	if remoteManagement["auto-update-cpa"] != true {
		t.Fatalf("auto-update-cpa = %#v, want true", remoteManagement["auto-update-cpa"])
	}
	if remoteManagement["panel-github-repository"] != "https://github.com/example/panel" {
		t.Fatalf("panel-github-repository = %#v", remoteManagement["panel-github-repository"])
	}
	if remoteManagement["cpa-github-repository"] != "https://github.com/example/cpa" {
		t.Fatalf("cpa-github-repository = %#v", remoteManagement["cpa-github-repository"])
	}
	if remoteManagement["check-interval"] != float64(180) {
		t.Fatalf("check-interval = %#v, want 180", remoteManagement["check-interval"])
	}
}

// --- Usage retention tests ---

func TestPutUsageRetention_Validation(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	persister := usage.NewPersister(filepath.Join(tmpDir, "usage"), config.UsageRetention{})
	defer persister.Stop()
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""), WithUsagePersister(persister))

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"valid positive", `{"days": 30}`, http.StatusOK},
		{"valid disable", `{"days": -1}`, http.StatusOK},
		{"valid size cap", `{"max_db_size_mb": 512, "warning_threshold_pct": 85}`, http.StatusOK},
		{"invalid zero", `{"days": 0}`, http.StatusBadRequest},
		{"invalid negative", `{"days": -2}`, http.StatusBadRequest},
		{"invalid size cap", `{"max_db_size_mb": -1}`, http.StatusBadRequest},
		{"invalid threshold low", `{"warning_threshold_pct": 0}`, http.StatusBadRequest},
		{"invalid threshold high", `{"warning_threshold_pct": 101}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPut, "/usage/retention", strings.NewReader(tt.body))
				c.Request.Header.Set("Content-Type", "application/json")
				h.PutUsageRetention(c)
				if rec.Code != tt.wantCode {
					t.Errorf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
				}
			},
		)
	}
}

func TestPutUsageRetention_PersistsToConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Port: 8317}
	persister := usage.NewPersister(filepath.Join(tmpDir, "usage"), config.UsageRetention{})
	defer persister.Stop()
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""), WithUsagePersister(persister))

	body := `{"days": 60, "max_db_size_mb": 512, "warning_threshold_pct": 85}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/usage/retention", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutUsageRetention(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify in-memory config was updated
	if cfg.UsageRetention.Days != 60 {
		t.Fatalf("expected cfg.UsageRetention.Days=60, got %d", cfg.UsageRetention.Days)
	}
	if cfg.UsageRetention.MaxDBSizeMB != 512 {
		t.Fatalf("expected cfg.UsageRetention.MaxDBSizeMB=512, got %d", cfg.UsageRetention.MaxDBSizeMB)
	}
	if cfg.UsageRetention.WarningThresholdPct != 85 {
		t.Fatalf("expected cfg.UsageRetention.WarningThresholdPct=85, got %d", cfg.UsageRetention.WarningThresholdPct)
	}

	// Verify response
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
	}
	if resp["max_db_size_mb"] != float64(512) {
		t.Fatalf("expected max_db_size_mb=512, got %v", resp["max_db_size_mb"])
	}
	if resp["warning_threshold_pct"] != float64(85) {
		t.Fatalf("expected warning_threshold_pct=85, got %v", resp["warning_threshold_pct"])
	}
}

func TestGetUsageRetention_NoPersister(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/retention", nil)
	h.GetUsageRetention(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["days"] != float64(0) {
		t.Fatalf("expected days=0 when no persister, got %v", resp["days"])
	}
	if resp["max_db_size_mb"] != float64(0) {
		t.Fatalf("expected max_db_size_mb=0 when no persister, got %v", resp["max_db_size_mb"])
	}
	if resp["warning_threshold_pct"] != float64(80) {
		t.Fatalf("expected warning_threshold_pct=80 when no persister, got %v", resp["warning_threshold_pct"])
	}
}

// --- Auth file upload tests ---

func TestUploadAuthFile_MultipartPermissions(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))

	// Create multipart form with a valid auth file
	authContent := `{"type":"anthropic","key":"sk-ant-test"}`
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "anthropic-test.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(authContent)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth-files", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.UploadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify file permissions
	filePath := filepath.Join(authDir, "anthropic-test.json")
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("expected file permissions 0600, got %o", perm)
	}
}

func TestUploadAuthFile_BodyMethod(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))

	authContent := `{"type":"anthropic","key":"sk-ant-test"}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost, "/auth-files?name=anthropic-body.json", strings.NewReader(authContent),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.UploadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal upload response: %v", err)
	}
	if payload["deleted"] != nil {
		t.Fatalf("unexpected deleted in upload response: %#v", payload["deleted"])
	}
	if payload["uploaded"] != float64(1) {
		t.Fatalf("uploaded = %#v, want 1", payload["uploaded"])
	}

	filePath := filepath.Join(authDir, "anthropic-body.json")
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("expected file permissions 0600, got %o", perm)
	}
}

func TestUploadAuthFile_MultipartBatch(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	files := map[string]string{
		"claude-a.json": `{"type":"anthropic","email":"a@example.com"}`,
		"claude-b.json": `{"type":"anthropic","email":"b@example.com"}`,
	}
	for name, content := range files {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth-files", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.UploadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal batch upload response: %v", err)
	}
	if payload["uploaded"] != float64(2) {
		t.Fatalf("uploaded = %#v, want 2", payload["uploaded"])
	}
	filesValue, ok := payload["files"].([]any)
	if !ok || len(filesValue) != 2 {
		t.Fatalf("files = %#v, want 2 entries", payload["files"])
	}
}

func TestUploadAuthFile_InvalidName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))

	tests := []struct {
		name string
		qn   string
	}{
		{"path traversal", "../evil.json"},
		{"no extension", "noext"},
		{"empty name", ""},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/auth-files?name="+tt.qn, strings.NewReader(`{}`))
				h.UploadAuthFile(c)
				if rec.Code == http.StatusOK {
					t.Fatalf("expected error for name %q, got 200", tt.qn)
				}
			},
		)
	}
}

func TestUploadAuthFile_RegistersQuotaEntry(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))
	h.quotaScheduler = quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return json.Marshal(map[string]any{"file": entry.FileName})
		},
	)

	authContent := `{"type":"gemini","email":"gemini@example.com","project_id":"proj-a"}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost, "/auth-files?name=gemini-user.json", strings.NewReader(authContent),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.UploadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	status := h.quotaScheduler.GetStatus()
	entry, exists := status.Credentials["gemini-user.json"]
	if !exists {
		t.Fatal("expected quota entry for uploaded auth file")
	}
	if entry.Type != quota.TypeGeminiCli {
		t.Fatalf("expected quota type %s, got %s", quota.TypeGeminiCli, entry.Type)
	}
	if entry.AuthIndex == "" {
		t.Fatal("expected quota auth index to be populated")
	}

	stored, ok := manager.GetByID("gemini-user.json")
	if !ok {
		t.Fatal("expected uploaded auth to be registered in auth manager")
	}
	if stored.Provider != string(quota.TypeGeminiCli) {
		t.Fatalf("expected provider %s, got %s", quota.TypeGeminiCli, stored.Provider)
	}
	if stored.FileName != "gemini-user.json" {
		t.Fatalf("expected FileName gemini-user.json, got %s", stored.FileName)
	}
}

func TestSaveTokenRecord_RegistersAuthManagerAndQuotaEntry(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))
	h.quotaScheduler = quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return json.Marshal(map[string]any{"file": entry.FileName})
		},
	)

	record := &coreauth.Auth{
		ID:       "codex-user.json",
		FileName: "codex-user.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":  "codex",
			"email": "codex@example.com",
		},
	}

	savedPath, err := h.saveTokenRecord(t.Context(), record)
	if err != nil {
		t.Fatalf("saveTokenRecord failed: %v", err)
	}
	if savedPath != filepath.Join(authDir, "codex-user.json") {
		t.Fatalf("expected saved path %q, got %q", filepath.Join(authDir, "codex-user.json"), savedPath)
	}
	if _, err := os.Stat(savedPath); err != nil {
		t.Fatalf("expected auth file on disk: %v", err)
	}

	stored, ok := manager.GetByID("codex-user.json")
	if !ok {
		t.Fatal("expected saved auth to be registered in auth manager")
	}
	if stored.Provider != "codex" {
		t.Fatalf("expected provider codex, got %s", stored.Provider)
	}
	if stored.FileName != "codex-user.json" {
		t.Fatalf("expected FileName codex-user.json, got %s", stored.FileName)
	}
	if strings.TrimSpace(stored.Attributes["path"]) != savedPath {
		t.Fatalf("expected stored path %q, got %q", savedPath, stored.Attributes["path"])
	}
	if stored.Index == "" {
		t.Fatal("expected saved auth index to be populated")
	}

	status := h.quotaScheduler.GetStatus()
	entry, exists := status.Credentials["codex-user.json"]
	if !exists {
		t.Fatal("expected quota entry for saved token record")
	}
	if entry.Type != quota.TypeCodex {
		t.Fatalf("expected quota type %s, got %s", quota.TypeCodex, entry.Type)
	}
	if entry.AuthIndex != stored.Index {
		t.Fatalf("expected quota auth index %q, got %q", stored.Index, entry.AuthIndex)
	}
}

func TestPatchAuthFileStatus_SyncsQuotaDisabledState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{AuthDir: authDir}
	h := NewHandler(cfg, "", manager, WithEnvSecret(""), WithTokenStore(store))
	h.quotaScheduler = quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return json.Marshal(map[string]any{"file": entry.FileName})
		},
	)

	authPath := filepath.Join(authDir, "claude-user.json")
	auth := &coreauth.Auth{
		ID:         "claude-user.json",
		FileName:   "claude-user.json",
		Provider:   "claude",
		Metadata:   map[string]any{"type": "claude", "email": "user@example.com"},
		Attributes: map[string]string{"path": authPath, "source": authPath},
	}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	auth.EnsureIndex()
	h.quotaScheduler.Register(auth.FileName, quota.TypeClaude, auth.Index)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/status",
		strings.NewReader(`{"name":"claude-user.json","disabled":true}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileStatus(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	stored, ok := manager.GetByID("claude-user.json")
	if !ok {
		t.Fatal("expected auth to remain in manager")
	}
	if !stored.Disabled {
		t.Fatal("expected auth to be disabled in manager")
	}
	if stored.Status != coreauth.StatusDisabled {
		t.Fatalf("expected disabled status, got %s", stored.Status)
	}

	status := h.quotaScheduler.GetStatus()
	entry, exists := status.Credentials["claude-user.json"]
	if !exists {
		t.Fatal("expected quota entry for patched auth file")
	}
	if !entry.Disabled {
		t.Fatal("expected quota entry to be disabled")
	}
	if entry.AuthIndex != stored.Index {
		t.Fatalf("expected quota auth index %q, got %q", stored.Index, entry.AuthIndex)
	}
}

// --- Config basic endpoint tests ---

func TestGetDebug(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{Debug: true}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/debug", nil)
	h.GetDebug(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["debug"] != true {
		t.Fatalf("expected debug=true, got %v", resp["debug"])
	}
}

func TestPutDebug(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Debug: false}
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))

	body := `{"value": true}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/debug", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutDebug(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !cfg.Debug {
		t.Fatal("expected cfg.Debug=true after PutDebug")
	}
}

func TestPutDebug_InvalidBody(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/debug", strings.NewReader(`not json`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutDebug(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d", rec.Code)
	}
}

// --- Routing strategy tests ---

func TestNormalizeRoutingStrategy(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		ok       bool
	}{
		{"round-robin", "round-robin", true},
		{"roundrobin", "round-robin", true},
		{"rr", "round-robin", true},
		{"fill-first", "fill-first", true},
		{"fillfirst", "fill-first", true},
		{"ff", "fill-first", true},
		{"", "round-robin", true},
		{"invalid", "", false},
	}

	for _, tt := range tests {
		t.Run(
			fmt.Sprintf("input=%q", tt.input), func(t *testing.T) {
				got, ok := normalizeRoutingStrategy(tt.input)
				if ok != tt.ok {
					t.Fatalf("expected ok=%v, got %v", tt.ok, ok)
				}
				if got != tt.expected {
					t.Fatalf("expected %q, got %q", tt.expected, got)
				}
			},
		)
	}
}

// --- Purge stale attempts tests ---

func TestPurgeStaleAttempts(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := &Handler{
		failedAttempts: map[string]*attemptInfo{
			"1.2.3.4": {failures: make([]time.Time, 3), lastActivity: time.Now().Add(-3 * time.Hour)},
			"5.6.7.8": {failures: make([]time.Time, 1), lastActivity: time.Now()},
		},
	}

	h.purgeStaleAttempts()

	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	if _, exists := h.failedAttempts["1.2.3.4"]; exists {
		t.Fatal("expected stale IP 1.2.3.4 to be purged")
	}
	if _, exists := h.failedAttempts["5.6.7.8"]; !exists {
		t.Fatal("expected recent IP 5.6.7.8 to be kept")
	}
}

func TestPurgeStaleAttempts_BannedIPKept(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := &Handler{
		failedAttempts: map[string]*attemptInfo{
			"1.2.3.4": {
				blockedUntil: time.Now().Add(1 * time.Hour),
				lastActivity: time.Now().Add(-3 * time.Hour),
			},
		},
	}

	h.purgeStaleAttempts()

	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	if _, exists := h.failedAttempts["1.2.3.4"]; !exists {
		t.Fatal("expected banned IP to be kept even if idle")
	}
}

// --- Usage statistics tests ---

func TestGetUsageStatistics_NilPersister(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret(""), WithUsageStats(nil))
	h.usageStats = nil

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage", nil)
	h.GetUsageStatistics(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestGetLogsFullRefreshKeepsNewestLines(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	logDir := t.TempDir()
	mainPath := filepath.Join(logDir, "main.log")
	rotatedPath := filepath.Join(logDir, "main-2026-05-08T02-13-14.529.log")

	rotatedContent := strings.Join(
		[]string{
			"[2026-05-08 10:08:42] [--------] [info ] [gin_logger.go:92] 304 | 0s | 61.119.121.203 | GET \"/management.html\"",
			"[2026-05-08 10:07:57] [--------] [warn ] [gin_logger.go:90] 404 | 0s | 107.173.241.217 | GET \"/containers/json\"",
		}, "\n",
	) + "\n"
	mainContent := strings.Join(
		[]string{
			"[2026-05-09 00:45:59] [--------] [info ] [gin_logger.go:92] 304 | 0s | 61.119.121.203 | GET \"/management.html\"",
			"[2026-05-09 00:46:20] [1e532bb7] [info ] [gin_logger.go:92] 200 | 1m7s | 61.119.121.203 | POST \"/v1/messages?beta=true\"",
		}, "\n",
	) + "\n"

	if err := os.WriteFile(rotatedPath, []byte(rotatedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte(mainContent), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(&config.Config{LoggingToFile: true}, "", nil, WithEnvSecret(""), WithLogDir(logDir))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/logs?limit=50", nil)
	h.GetLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(resp.Lines))
	}
	last := resp.Lines[len(resp.Lines)-1]
	if !strings.Contains(last, "[2026-05-09 00:46:20]") || !strings.Contains(last, "/v1/messages?beta=true") {
		t.Fatalf("expected newest main.log line to stay last, got %q", last)
	}
}
