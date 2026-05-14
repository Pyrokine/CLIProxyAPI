package api

import (
	"context"
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
