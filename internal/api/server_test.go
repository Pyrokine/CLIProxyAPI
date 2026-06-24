package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath)
}

func TestRegisteredManagementRouteKeysIncludeFixedPluginRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)
	keys := server.registeredManagementRouteKeys()
	for _, key := range []string{
		"GET /v0/management/plugins",
		"PATCH /v0/management/plugins/:id/enabled",
		"PUT /v0/management/plugins/:id/config",
		"PATCH /v0/management/plugins/:id/config",
	} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("registeredManagementRouteKeys() missing %s", key)
		}
	}
}

func TestPluginResourcePrefixRequiresManagementAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Management-Key", "test-management-password")
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("authenticated missing route status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("authenticated missing route set cookies: %#v", cookies)
	}
}

func TestPluginResourceSessionCookieSkipsManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.Store(true)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	if !server.issuePluginResourceSession(c) {
		t.Fatal("issuePluginResourceSession() = false")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != pluginResourceSessionCookie {
		t.Fatalf("cookies = %#v, want one plugin resource session cookie", cookies)
	}
	if cookies[0].Path != "/v0/resource/plugins/" || !cookies[0].HttpOnly {
		t.Fatalf("cookie attributes = %#v, want resource path and HttpOnly", cookies[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.AddCookie(cookies[0])
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cookie-authenticated missing route status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginResourceExpiredSessionCookieRequiresManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.Store(true)

	server.pluginResourceSessionMu.Lock()
	server.pluginResourceSessionTokens["expired"] = pluginResourceSession{
		IdleExpiresAt:     time.Now().Add(-time.Minute),
		AbsoluteExpiresAt: time.Now().Add(pluginResourceSessionAbsoluteTTL),
	}
	server.pluginResourceSessionMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.AddCookie(&http.Cookie{Name: pluginResourceSessionCookie, Value: "expired"})
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired cookie status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginResourceAbsoluteExpiredSessionCookieRequiresManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.Store(true)

	server.pluginResourceSessionMu.Lock()
	server.pluginResourceSessionTokens["absolute-expired"] = pluginResourceSession{
		IdleExpiresAt:     time.Now().Add(pluginResourceSessionTTL),
		AbsoluteExpiresAt: time.Now().Add(-time.Minute),
	}
	server.pluginResourceSessionMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.AddCookie(&http.Cookie{Name: pluginResourceSessionCookie, Value: "absolute-expired"})
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("absolute expired cookie status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginResourceSessionRenewalIsCappedByAbsoluteExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	now := time.Now()
	absoluteExpiresAt := now.Add(time.Minute)
	server.pluginResourceSessionMu.Lock()
	server.pluginResourceSessionTokens["near-absolute"] = pluginResourceSession{
		IdleExpiresAt:     now.Add(pluginResourceSessionTTL),
		AbsoluteExpiresAt: absoluteExpiresAt,
	}
	server.pluginResourceSessionMu.Unlock()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	c.Request.AddCookie(&http.Cookie{Name: pluginResourceSessionCookie, Value: "near-absolute"})

	if !server.pluginResourceSessionAuthenticated(c) {
		t.Fatal("pluginResourceSessionAuthenticated() = false, want true")
	}
	server.pluginResourceSessionMu.Lock()
	session := server.pluginResourceSessionTokens["near-absolute"]
	server.pluginResourceSessionMu.Unlock()
	if session.IdleExpiresAt.After(absoluteExpiresAt) {
		t.Fatalf("idle expiry = %s, want not after absolute expiry %s", session.IdleExpiresAt, absoluteExpiresAt)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != pluginResourceSessionCookie {
		t.Fatalf("renewal cookies = %#v, want session cookie", cookies)
	}
	if cookies[0].MaxAge > int(time.Minute.Seconds()) {
		t.Fatalf("renewed cookie MaxAge = %d, want capped by absolute expiry", cookies[0].MaxAge)
	}
}

func TestAmpProviderModelRoutes(t *testing.T) {
	testCases := []struct {
		name         string
		path         string
		wantStatus   int
		wantContains string
	}{
		{
			name:         "openai root models",
			path:         "/api/provider/openai/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "groq root models",
			path:         "/api/provider/groq/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "openai models",
			path:         "/api/provider/openai/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "anthropic models",
			path:         "/api/provider/anthropic/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"data"`,
		},
		{
			name:         "google models v1",
			path:         "/api/provider/google/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
		{
			name:         "google models v1beta",
			path:         "/api/provider/google/v1beta/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
	}

	for _, tc := range testCases {
		t.Run(
			tc.name, func(t *testing.T) {
				server := newTestServer(t)

				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				req.Header.Set("Authorization", "Bearer test-key")

				rr := httptest.NewRecorder()
				server.engine.ServeHTTP(rr, req)

				if rr.Code != tc.wantStatus {
					t.Fatalf(
						"unexpected status code for %s: got %d want %d; body=%s", tc.path, rr.Code, tc.wantStatus,
						rr.Body.String(),
					)
				}
				if body := rr.Body.String(); !strings.Contains(body, tc.wantContains) {
					t.Fatalf("response body for %s missing %q: %s", tc.path, tc.wantContains, body)
				}
			},
		)
	}
}

func TestPluginResourceCORSAllowsCredentialsForOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(corsMiddleware(nil))
	engine.GET("/v0/resource/plugins/demo/status", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.GET("/v0/management/config", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodOptions, "/v0/resource/plugins/demo/status", nil)
	req.Header.Set("Origin", "https://panel.example.com")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://panel.example.com" {
		t.Fatalf("plugin resource ACAO = %q, want request origin", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("plugin resource ACAC = %q, want true", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "authorization" {
		t.Fatalf("plugin resource ACAH = %q, want requested header", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.Header.Set("Origin", "https://panel.example.com")
	rr = httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("management ACAO = %q, want *", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("management ACAC = %q, want empty", got)
	}
}

func TestManagementBodyLimitMiddlewareUsesImportLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(managementBodyLimitMiddleware())
	engine.POST(
		"/v0/management/usage/import", func(c *gin.Context) {
			_, errRead := io.ReadAll(c.Request.Body)
			if errRead != nil {
				c.String(http.StatusRequestEntityTooLarge, errRead.Error())
				return
			}
			c.Status(http.StatusNoContent)
		},
	)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v0/management/usage/import",
		strings.NewReader(strings.Repeat("a", (10<<20)+1)),
	)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestNoStoreMiddleware_SetsCacheControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(noStoreMiddleware())
	engine.GET(
		"/test", func(c *gin.Context) {
			c.String(http.StatusOK, "ok")
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fall back to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(
		filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644,
	); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}

// --- authMiddleware tests ---

func setupAuthMiddlewareEngine(manager *sdkaccess.Manager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(authMiddleware(manager, nil, false))
	engine.GET(
		"/test", func(c *gin.Context) {
			apiKey, _ := c.Get("apiKey")
			c.JSON(http.StatusOK, gin.H{"apiKey": apiKey})
		},
	)
	return engine
}

func TestAuthMiddleware_NilManager_LoopbackAllowed(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for loopback with nil manager, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthMiddleware_NilManager_NonLoopbackRejected(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-loopback with nil manager, got %d", rr.Code)
	}
}

func TestAuthMiddleware_NilManager_IPv6LoopbackAllowed(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "[::1]:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for IPv6 loopback with nil manager, got %d", rr.Code)
	}
}

func setupRequireTLSEngine(cfg *proxyconfig.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(requireTLSForAuth(cfg))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
}

func TestRequireTLSForAuthRejectsSpoofedLoopbackHost(t *testing.T) {
	engine := setupRequireTLSEngine(&proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true}})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMisdirectedRequest)
	}
}

func TestRequireTLSForAuthAllowsLoopbackRemoteAddr(t *testing.T) {
	engine := setupRequireTLSEngine(&proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true}})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Host = "example.com"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
}

// stubProvider implements sdkaccess.Provider for testing.
type stubProvider struct {
	keys map[string]struct{}
}

func (s *stubProvider) Identifier() string { return "stub" }

func (s *stubProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	key := strings.TrimPrefix(authHeader, "Bearer ")
	if _, ok := s.keys[key]; ok {
		return &sdkaccess.Result{
			Provider:  "stub",
			Principal: key,
		}, nil
	}
	return nil, sdkaccess.NewInvalidCredentialError()
}

func TestAuthMiddleware_ValidAPIKey(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, nil, false))
	engine.GET(
		"/test", func(c *gin.Context) {
			apiKey, _ := c.Get("apiKey")
			c.JSON(http.StatusOK, gin.H{"apiKey": apiKey})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	req.RemoteAddr = "10.0.0.1:9999"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "valid-key") {
		t.Fatalf("expected apiKey in context, body: %s", rr.Body.String())
	}
}

func TestAuthMiddleware_InvalidAPIKey(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, nil, false))
	engine.GET(
		"/test", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer bad-key")
	req.RemoteAddr = "10.0.0.1:9999"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthMiddleware_RateLimited(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)

	rl := access.NewAuthRateLimiter()
	defer rl.Stop()

	// Trigger lockout for the IP.
	for i := 0; i < 10; i++ {
		rl.RecordFailure("10.0.0.99")
	}

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rl, false))
	engine.GET(
		"/test", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	req.RemoteAddr = "10.0.0.99:9999"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rr.Code, rr.Body.String())
	}
}
