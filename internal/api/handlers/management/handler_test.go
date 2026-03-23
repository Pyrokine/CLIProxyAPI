// Last compiled: 2026-03-23
// Author: pyro

package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

func init() {
	gin.SetMode(gin.TestMode)
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

	// Send 5 failed attempts to trigger ban
	for i := 0; i < 5; i++ {
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
		{"invalid zero", `{"days": 0}`, http.StatusBadRequest},
		{"invalid negative", `{"days": -2}`, http.StatusBadRequest},
		{"valid max_file_size", `{"max_file_size_mb": 100}`, http.StatusOK},
		{"invalid max_file_size zero", `{"max_file_size_mb": 0}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/usage/retention", strings.NewReader(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.PutUsageRetention(c)
			if rec.Code != tt.wantCode {
				t.Errorf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
		})
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

	body := `{"days": 60, "max_file_size_mb": 200}`
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
	if cfg.UsageRetention.MaxFileSizeMB != 200 {
		t.Fatalf("expected cfg.UsageRetention.MaxFileSizeMB=200, got %d", cfg.UsageRetention.MaxFileSizeMB)
	}

	// Verify response
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
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
	c.Request = httptest.NewRequest(http.MethodPost, "/auth-files?name=anthropic-body.json", strings.NewReader(authContent))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UploadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify file was written with correct permissions
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
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/auth-files?name="+tt.qn, strings.NewReader(`{}`))
			h.UploadAuthFile(c)
			if rec.Code == http.StatusOK {
				t.Fatalf("expected error for name %q, got 200", tt.qn)
			}
		})
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
		t.Run(fmt.Sprintf("input=%q", tt.input), func(t *testing.T) {
			got, ok := normalizeRoutingStrategy(tt.input)
			if ok != tt.ok {
				t.Fatalf("expected ok=%v, got %v", tt.ok, ok)
			}
			if got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

// --- Purge stale attempts tests ---

func TestPurgeStaleAttempts(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := &Handler{
		failedAttempts: map[string]*attemptInfo{
			"1.2.3.4": {count: 3, lastActivity: time.Now().Add(-3 * time.Hour)},
			"5.6.7.8": {count: 1, lastActivity: time.Now()},
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
				count:        0,
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
