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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type blockingReadCloser struct {
	reader  io.Reader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *blockingReadCloser) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	return r.reader.Read(p)
}

func (*blockingReadCloser) Close() error {
	return nil
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret("test-secret"))
	engine := gin.New()
	engine.GET(
		"/v0/management/config", h.Middleware(), func(c *gin.Context) {
			c.Status(http.StatusOK)
		},
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Management-Key", "test-secret")
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get(pluginhost.SupportPluginHeader); got != pluginhost.SupportPluginHeaderValue() {
		t.Fatalf("%s = %q, want %q", pluginhost.SupportPluginHeader, got, pluginhost.SupportPluginHeaderValue())
	}
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

func TestFetchAllReleases_ForceBypassesCache(t *testing.T) {
	cachedReleases.entries = map[string]releasesCacheEntry{
		"Pyrokine/Cli-Proxy-API-Management-Center": {
			data: []gitHubRelease{{TagName: "cached"}}, fetchedAt: time.Now(),
		},
	}
	defer func() {
		cachedReleases.entries = make(map[string]releasesCacheEntry)
	}()

	proxy := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `[{"tag_name":"fresh"}]`)
			},
		),
	)
	defer proxy.Close()

	h := NewHandler(&config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxy.URL}}, "", nil)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/releases?force=1", nil)
	releases, err := h.fetchAllReleases(ctx, "Pyrokine/Cli-Proxy-API-Management-Center", true, proxy.URL)
	if err == nil {
		t.Fatalf("expected proxy URL to fail because it is not a valid proxy transport endpoint")
	}
	if releases != nil {
		t.Fatalf("releases = %#v, want nil on fetch error", releases)
	}
}

func TestRestartCurrentProcess_RequiresSystemd(t *testing.T) {
	if err := os.Unsetenv("INVOCATION_ID"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	if err := restartCurrentProcess(); err == nil {
		t.Fatal("expected restartCurrentProcess to reject non-systemd environment")
	}
}

// --- Option pattern tests ---

func TestWithEnvSecret(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret(" my-secret "))
	if h.envSecret != "my-secret" {
		t.Fatalf("expected envSecret=%q, got %q", "my-secret", h.envSecret)
	}

	h2 := NewHandler(&config.Config{}, "", nil, WithEnvSecret(""))
	if h2.envSecret != "" {
		t.Fatalf("expected empty envSecret, got %q", h2.envSecret)
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
	cfg.RemoteManagement.AllowRemote = true
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

func TestMiddleware_EnvSecretDoesNotEnableRemote(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "", nil, WithEnvSecret("env-pass"))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer env-pass")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when remote management is disabled, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_UntrustedXRealIPIsIgnored(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))
	h.localPassword = "local-pass"

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	rec := httptest.NewRecorder()
	req := newLoopbackRequest("GET", "/test", "Bearer local-pass")
	req.Header.Set("X-Real-IP", "not-an-ip")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when untrusted X-Real-IP is ignored, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_InvalidTrustedXRealIPIsRemote(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))
	h.localPassword = "local-pass"

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	rec := httptest.NewRecorder()
	req := newLoopbackRequest("GET", "/test", "Bearer local-pass")
	req.Header.Set("X-Real-IP", "not-an-ip")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for malformed trusted X-Real-IP, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_UntrustedXRealIPDoesNotBypassRateLimit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = true
	h := NewHandler(cfg, "", nil, WithEnvSecret("env-pass"))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := newLoopbackRequest("GET", "/test", "Bearer wrong")
		req.Header.Set("X-Real-IP", fmt.Sprintf("20.0.0.%d", i+1))
		engine.ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	req := newLoopbackRequest("GET", "/test", "Bearer env-pass")
	req.Header.Set("X-Real-IP", "20.0.0.99")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after spoofed X-Real-IP attempts, got %d: %s", rec.Code, rec.Body.String())
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
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for remote with local-only password, got %d", rec.Code)
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
	cfg.RemoteManagement.AllowRemote = true
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
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when remote management disabled, got %d", rec.Code)
	}
}

func TestMiddleware_RemoteDisabledConsumesRateLimit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.SecretKey = "some-hash"
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := newRemoteRequest("GET", "/test", "Bearer something")
		engine.ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer something")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after repeated remote-disabled probes, got %d", rec.Code)
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

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := newRemoteRequest("GET", "/test", "Bearer wrong")
		engine.ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer "+secret)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after IP ban, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, managementRateLimitMessage) {
		t.Fatalf("expected rate-limit message, got: %s", body)
	}
}

func TestRateLimitStatusIncludesBanCountAndUnbanHistory(t *testing.T) {
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

	triggerBan := func() {
		for i := 0; i < 10; i++ {
			rec := httptest.NewRecorder()
			req := newRemoteRequest("GET", "/test", "Bearer wrong")
			engine.ServeHTTP(rec, req)
		}
	}
	readStatus := func() struct {
		BannedIPs []struct {
			IP       string `json:"ip"`
			BanCount int    `json:"ban_count"`
		} `json:"banned_ips"`
		UnbanHistory []struct {
			IP          string    `json:"ip"`
			UnbannedAt  time.Time `json:"unbanned_at"`
			BannedUntil time.Time `json:"banned_until"`
			BanCount    int       `json:"ban_count"`
		} `json:"unban_history"`
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/management/rate-limit/status", nil)
		h.GetRateLimitStatus(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRateLimitStatus status = %d", rec.Code)
		}
		var body struct {
			BannedIPs []struct {
				IP       string `json:"ip"`
				BanCount int    `json:"ban_count"`
			} `json:"banned_ips"`
			UnbanHistory []struct {
				IP          string    `json:"ip"`
				UnbannedAt  time.Time `json:"unbanned_at"`
				BannedUntil time.Time `json:"banned_until"`
				BanCount    int       `json:"ban_count"`
			} `json:"unban_history"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal rate limit status: %v", err)
		}
		return body
	}

	triggerBan()
	status := readStatus()
	if len(status.BannedIPs) != 1 {
		t.Fatalf("banned IPs = %d, want 1", len(status.BannedIPs))
	}
	if status.BannedIPs[0].IP != "203.0.113.1" || status.BannedIPs[0].BanCount != 1 {
		t.Fatalf("banned entry = %#v, want IP 203.0.113.1 and ban_count 1", status.BannedIPs[0])
	}
	if len(status.UnbanHistory) != 0 {
		t.Fatalf("unban history = %d, want 0 before manual unban", len(status.UnbanHistory))
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodDelete,
		"/management/rate-limit/ban",
		strings.NewReader(`{"ip":"203.0.113.1"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.DeleteRateLimitBan(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("DeleteRateLimitBan status = %d", rec.Code)
	}

	status = readStatus()
	if len(status.BannedIPs) != 0 {
		t.Fatalf("banned IPs = %d, want 0 after manual unban", len(status.BannedIPs))
	}
	if len(status.UnbanHistory) != 1 {
		t.Fatalf("unban history = %d, want 1", len(status.UnbanHistory))
	}
	history := status.UnbanHistory[0]
	if history.IP != "203.0.113.1" || history.BanCount != 1 || history.UnbannedAt.IsZero() || history.BannedUntil.IsZero() {
		t.Fatalf("unban history entry = %#v", history)
	}

	triggerBan()
	status = readStatus()
	if len(status.BannedIPs) != 1 || status.BannedIPs[0].BanCount != 2 {
		t.Fatalf("banned IPs after second ban = %#v, want ban_count 2", status.BannedIPs)
	}
}

func TestRateLimitStatusIncludesAndConsumesFailedLoginNotices(t *testing.T) {
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

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := newRemoteRequest("GET", "/test", "Bearer wrong")
		engine.ServeHTTP(rec, req)
	}
	rec := httptest.NewRecorder()
	req := newRemoteRequest("GET", "/test", "Bearer "+secret)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected successful auth, got %d", rec.Code)
	}

	readNotices := func(consume bool) []struct {
		IP                   string    `json:"ip"`
		Count                int       `json:"count"`
		LastFailureAt        time.Time `json:"last_failure_at"`
		LastSuccessfulAuthAt time.Time `json:"last_successful_auth_at"`
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		path := "/management/rate-limit/status"
		if consume {
			path += "?consume_failed_login_notices=true"
		}
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		h.GetRateLimitStatus(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRateLimitStatus status = %d", rec.Code)
		}
		var body struct {
			FailedLoginNotices []struct {
				IP                   string    `json:"ip"`
				Count                int       `json:"count"`
				LastFailureAt        time.Time `json:"last_failure_at"`
				LastSuccessfulAuthAt time.Time `json:"last_successful_auth_at"`
			} `json:"failed_login_notices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal rate limit status: %v", err)
		}
		return body.FailedLoginNotices
	}

	notices := readNotices(true)
	if len(notices) != 1 {
		t.Fatalf("failed login notices = %d, want 1", len(notices))
	}
	if notices[0].IP != "203.0.113.1" || notices[0].Count != 3 {
		t.Fatalf("failed login notice = %#v, want IP 203.0.113.1 count 3", notices[0])
	}
	if notices[0].LastFailureAt.IsZero() || notices[0].LastSuccessfulAuthAt.IsZero() {
		t.Fatalf("failed login notice timestamps missing: %#v", notices[0])
	}
	if got := readNotices(false); len(got) != 0 {
		t.Fatalf("failed login notices after consume = %#v, want empty", got)
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

	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	prefix := "port: 8317\n#"
	body := prefix + strings.Repeat("a", int(maxConfigYAMLBytes)-len(prefix)) + "x"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/yaml")

	h.PutConfigYAML(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.Error != "body_too_large" {
		t.Fatalf("error = %q, want body_too_large", response.Error)
	}
}

func TestValidateConfigYAML_BodyLimit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
	prefix := "port: 8317\n#"
	body := prefix + strings.Repeat("a", int(maxConfigYAMLBytes)-len(prefix)) + "x"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/config.yaml/validate", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/yaml")

	h.ValidateConfigYAML(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"body_too_large"`) {
		t.Fatalf("response missing body_too_large: %s", rec.Body.String())
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

func TestImportUsageStatisticsDistinguishesInputAndStorageErrors(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	newHandler := func(t *testing.T) (*Handler, *usage.Persister) {
		t.Helper()
		baseDir := t.TempDir()
		configPath := filepath.Join(baseDir, "config.yaml")
		if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		persister := usage.NewPersister(baseDir, config.UsageRetention{Days: -1})
		persister.Start(context.Background())
		handler := NewHandler(
			&config.Config{},
			configPath,
			nil,
			WithEnvSecret(""),
			WithUsagePersister(persister),
		)
		return handler, persister
	}

	t.Run("invalid payload", func(t *testing.T) {
		handler, persister := newHandler(t)
		defer persister.Stop()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/usage/import", strings.NewReader("{"))
		handler.ImportUsageStatistics(c)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		handler, persister := newHandler(t)
		persister.Stop()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(
			http.MethodPost,
			"/usage/import",
			strings.NewReader(`{"date":"2026-04-27","details":[{"timestamp":"2026-04-27T10:00:00Z","model":"m"}]}`),
		)
		handler.ImportUsageStatistics(c)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", recorder.Code, recorder.Body.String())
		}
	})
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
	if remoteManagement["disable-auto-update-panel"] != false {
		t.Fatalf("disable-auto-update-panel = %#v, want false", remoteManagement["disable-auto-update-panel"])
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
		{"trailing json value", `{"days": 30}{"days": -1}`, http.StatusBadRequest},
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

	committed := h.configSnapshot()
	if committed == nil {
		t.Fatal("expected committed config")
	}
	if committed.UsageRetention.Days != 60 {
		t.Fatalf("expected UsageRetention.Days=60, got %d", committed.UsageRetention.Days)
	}
	if committed.UsageRetention.MaxDBSizeMB != 512 {
		t.Fatalf("expected UsageRetention.MaxDBSizeMB=512, got %d", committed.UsageRetention.MaxDBSizeMB)
	}
	if committed.UsageRetention.WarningThresholdPct != 85 {
		t.Fatalf("expected UsageRetention.WarningThresholdPct=85, got %d", committed.UsageRetention.WarningThresholdPct)
	}
	if cfg.UsageRetention != (config.UsageRetention{}) {
		t.Fatalf("original config changed: %+v", cfg.UsageRetention)
	}
	if got := persister.Retention(); got != committed.UsageRetention {
		t.Fatalf("persister.Retention() = %+v, want %+v", got, committed.UsageRetention)
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

func TestPutUsageRetention_SaveFailureKeepsRuntimeConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("usage-retention: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	previousRetention := config.UsageRetention{Days: 30, MaxDBSizeMB: 256, WarningThresholdPct: 75}
	cfg := &config.Config{Port: 8317, UsageRetention: previousRetention}
	persister := usage.NewPersister(filepath.Join(tmpDir, "usage"), previousRetention)
	defer persister.Stop()
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""), WithUsagePersister(persister))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/usage/retention",
		strings.NewReader(`{"days":60,"max_db_size_mb":512,"warning_threshold_pct":85}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutUsageRetention(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if cfg.UsageRetention != previousRetention {
		t.Fatalf("cfg.UsageRetention = %+v, want %+v", cfg.UsageRetention, previousRetention)
	}
	if got := persister.Retention(); got != previousRetention {
		t.Fatalf("persister.Retention() = %+v, want %+v", got, previousRetention)
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

func TestUploadAuthFileInvalidReplacementPreservesExistingCredential(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandler(&config.Config{AuthDir: authDir}, "", manager, WithEnvSecret(""), WithTokenStore(store))
	path := filepath.Join(authDir, "existing.json")
	original := []byte(`{"type":"codex","access_token":"old"}`)
	if errWrite := os.WriteFile(path, original, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/auth-files?name=existing.json", strings.NewReader(`{"type":`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.UploadAuthFile(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(current) != string(original) {
		t.Fatalf("existing credential changed after invalid upload: %q", current)
	}
}

func TestUploadAuthFileUsesCapturedAuthDir(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDirA := t.TempDir()
	authDirB := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandler(
		&config.Config{AuthDir: authDirA}, "", manager, WithEnvSecret(""), WithTokenStore(store),
	)

	started := make(chan struct{})
	release := make(chan struct{})
	body := &blockingReadCloser{
		reader:  strings.NewReader(`{"type":"codex","email":"generation-a@example.com"}`),
		started: started,
		release: release,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth-files?name=generation-a.json", body)
	c.Request.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.UploadAuthFile(c)
	}()
	<-started
	h.SetConfig(&config.Config{AuthDir: authDirB})
	close(release)
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	pathA := filepath.Join(authDirA, "generation-a.json")
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("generation A auth file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDirB, "generation-a.json")); !os.IsNotExist(err) {
		t.Fatalf("auth file unexpectedly written to generation B: %v", err)
	}
	registered, ok := manager.GetByID("generation-a.json")
	if !ok {
		t.Fatal("generation A auth record was not registered")
	}
	if got := strings.TrimSpace(registered.Attributes["path"]); got != pathA {
		t.Fatalf("registered auth path = %q, want %q", got, pathA)
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
	entry, exists := status.Credentials[quota.EntryKey(quota.TypeGeminiCli, "gemini-user.json")]
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

	savedPath, err := h.saveTokenRecord(t.Context(), authDir, record)
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
	entry, exists := status.Credentials[quota.EntryKey(quota.TypeCodex, "codex-user.json")]
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

func TestSaveTokenRecordUsesAuthFlowConfigGeneration(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDirA := t.TempDir()
	authDirB := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDirA)
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandler(
		&config.Config{AuthDir: authDirA}, "", manager, WithEnvSecret(""), WithTokenStore(store),
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		t.Fatalf("authFlowConfig failed with status %d", rec.Code)
	}

	h.SetConfig(&config.Config{AuthDir: authDirB})
	store.SetBaseDir(authDirB)
	record := &coreauth.Auth{
		ID:       "generation-a.json",
		FileName: "generation-a.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	savedPath, err := h.saveTokenRecord(t.Context(), flowCfg.AuthDir, record)
	if err != nil {
		t.Fatalf("saveTokenRecord failed: %v", err)
	}
	if want := filepath.Join(authDirA, "generation-a.json"); savedPath != want {
		t.Fatalf("saved path = %q, want %q", savedPath, want)
	}
	if _, err = os.Stat(savedPath); err != nil {
		t.Fatalf("generation A auth file missing: %v", err)
	}
	if _, err = os.Stat(filepath.Join(authDirB, "generation-a.json")); !os.IsNotExist(err) {
		t.Fatalf("generation A record unexpectedly written to generation B: %v", err)
	}
	registered, ok := manager.GetByID("generation-a.json")
	if !ok {
		t.Fatal("generation A auth record was not registered with its generation-relative ID")
	}
	if got := strings.TrimSpace(registered.Attributes["path"]); got != savedPath {
		t.Fatalf("registered auth path = %q, want %q", got, savedPath)
	}
}

func TestSaveTokenRecordProvidesScopedPathToLegacyStore(t *testing.T) {
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	h := NewHandler(
		&config.Config{AuthDir: authDir},
		"",
		nil,
		WithEnvSecret(""),
		WithTokenStore(store),
	)
	record := &coreauth.Auth{
		ID:       "legacy-store.json",
		FileName: "legacy-store.json",
		Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
	}
	if _, err := h.saveTokenRecord(t.Context(), authDir, record); err != nil {
		t.Fatalf("saveTokenRecord failed: %v", err)
	}
	stored := store.items[record.ID]
	if stored == nil {
		t.Fatal("legacy store did not receive auth record")
	}
	want := filepath.Join(authDir, "legacy-store.json")
	if got := strings.TrimSpace(stored.Attributes["path"]); got != want {
		t.Fatalf("stored path = %q, want %q", got, want)
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
	entry, exists := status.Credentials[quota.EntryKey(quota.TypeClaude, "claude-user.json")]
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
	if snapshot := h.configSnapshot(); !snapshot.Debug {
		t.Fatal("expected committed Debug=true after PutDebug")
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

func TestDownloadAuthFileRedactsSecrets(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(authDir, "secret.json"),
		[]byte(`{"access_token":"access-secret-value","refresh_token":"refresh-secret-value"}`),
		0o600,
	); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files/download?name=secret.json", nil)
	h.DownloadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "access-secret-value") || strings.Contains(body, "refresh-secret-value") {
		t.Fatalf("download kept raw auth secret: %s", body)
	}
}

func TestRedactedManagementPayloadMasksSensitiveMapKeys(t *testing.T) {
	payload := map[string]any{
		"usage": map[string]any{
			"apis": map[string]any{
				"sk-secret-value": map[string]any{
					"models": map[string]any{
						"gpt-5": map[string]any{
							"details": []any{
								map[string]any{"api_key": "sk-secret-value"},
							},
						},
					},
				},
			},
		},
	}

	redacted, err := redactedManagementPayload(payload)
	if err != nil {
		t.Fatalf("redactedManagementPayload() error = %v", err)
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("marshal redacted payload: %v", err)
	}
	if strings.Contains(string(data), "sk-secret-value") {
		t.Fatalf("redacted payload kept raw API key: %s", data)
	}
}

func TestWriteRedactedJSONPreservesLargeIntegers(t *testing.T) {
	const requests int64 = 9007199254740993
	const rawAPIKey = "sk-large-integer-secret-value"

	engine := gin.New()
	engine.GET("/usage", func(c *gin.Context) {
		writeRedactedJSON(c, http.StatusOK, gin.H{
			"requests": requests,
			"api_key":  rawAPIKey,
		})
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/usage", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"requests":9007199254740993`) {
		t.Fatalf("response changed large integer: %s", body)
	}
	if strings.Contains(body, rawAPIKey) || !strings.Contains(body, util.HideAPIKey(rawAPIKey)) {
		t.Fatalf("response API key redaction mismatch: %s", body)
	}
}

func TestGetConfigRedactsSensitiveValues(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawAPIKey := "sk-config-secret-value"
	rawProviderKey := "sk-gemini-secret-value"
	rawAuthorization := "Bearer sk-header-secret-value"
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeys:       []string{rawAPIKey},
			APIKeyAliases: map[string]string{rawAPIKey: "main"},
		},
		GeminiKey: []config.GeminiKey{
			{APIKey: rawProviderKey, Headers: map[string]string{"Authorization": rawAuthorization}},
		},
	}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config", nil)
	h.GetConfig(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{rawAPIKey, rawProviderKey, rawAuthorization, "sk-header-secret-value"} {
		if strings.Contains(body, secret) {
			t.Fatalf("GetConfig kept raw secret %q in %s", secret, body)
		}
	}
	for _, masked := range []string{util.HideAPIKey(rawAPIKey), util.HideAPIKey(rawProviderKey), "Bearer " + util.HideAPIKey("sk-header-secret-value")} {
		if !strings.Contains(body, masked) {
			t.Fatalf("GetConfig missing masked value %q in %s", masked, body)
		}
	}
}

func TestConfigYAMLRedactsAndRestoresSensitivePlaceholders(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawAPIKey := "sk-yaml-secret-value"
	rawAliasKey := "sk-yaml-alias-value"
	cfg := &config.Config{
		Port: 8317,
		SDKConfig: config.SDKConfig{
			APIKeys:       []string{rawAPIKey},
			APIKeyAliases: map[string]string{rawAliasKey: "alias"},
		},
	}
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := strings.Join(
		[]string{
			"port: 8317",
			"api-keys:",
			"  - " + rawAPIKey,
			"api-key-aliases:",
			"  " + rawAliasKey + ": alias",
			"",
		}, "\n",
	)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	redacted := rec.Body.String()
	if strings.Contains(redacted, rawAPIKey) || strings.Contains(redacted, rawAliasKey) {
		t.Fatalf("GetConfigYAML kept raw secret: %s", redacted)
	}
	if count := strings.Count(redacted, configYAMLPlaceholderPrefix); count != 2 {
		t.Fatalf("GetConfigYAML placeholder count = %d, want 2: %s", count, redacted)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(redacted))
	c.Request.Header.Set("Content-Type", "application/yaml")
	h.PutConfigYAML(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	stored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	storedText := string(stored)
	if !strings.Contains(storedText, rawAPIKey) || !strings.Contains(storedText, rawAliasKey) {
		t.Fatalf("PutConfigYAML failed to restore raw secrets: %s", storedText)
	}
	if strings.Contains(storedText, configYAMLPlaceholderPrefix) {
		t.Fatalf("PutConfigYAML persisted opaque placeholders: %s", storedText)
	}
}

func TestConfigYAMLRedactionUsesSensitiveScalarContext(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawSecret := ":"
	masked := util.HideAPIKey(rawSecret)
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeys:  []string{rawSecret},
			ProxyURL: masked,
		},
	}
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := strings.Join([]string{
		"# keep this comment",
		"port: 8317",
		"proxy-url: '" + masked + "'",
		"api-keys:",
		"  - ':'",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	redacted := rec.Body.String()
	if !strings.Contains(redacted, "# keep this comment") {
		t.Fatalf("GetConfigYAML discarded comment: %s", redacted)
	}
	if strings.Index(redacted, "port:") > strings.Index(redacted, "api-keys:") {
		t.Fatalf("GetConfigYAML changed mapping order: %s", redacted)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal([]byte(redacted), &decoded); err != nil {
		t.Fatalf("decode redacted YAML: %v", err)
	}
	if decoded["proxy-url"] != masked {
		t.Fatalf("proxy-url = %#v, want literal %q", decoded["proxy-url"], masked)
	}
	keys, ok := decoded["api-keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("api-keys = %#v, want one redacted secret", decoded["api-keys"])
	}
	placeholder, ok := keys[0].(string)
	if !ok || !isConfigYAMLPlaceholder(placeholder) {
		t.Fatalf("api-keys = %#v, want opaque placeholder", decoded["api-keys"])
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(redacted))
	c.Request.Header.Set("Content-Type", "application/yaml")
	h.PutConfigYAML(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	stored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var storedConfig config.Config
	if err = yaml.Unmarshal(stored, &storedConfig); err != nil {
		t.Fatalf("decode stored YAML: %v", err)
	}
	if len(storedConfig.APIKeys) != 1 || storedConfig.APIKeys[0] != rawSecret {
		t.Fatalf("stored APIKeys = %#v, want raw secret", storedConfig.APIKeys)
	}
	if storedConfig.ProxyURL != masked {
		t.Fatalf("stored ProxyURL = %q, want literal placeholder", storedConfig.ProxyURL)
	}
}

func TestConfigYAMLRedactionUsesCurrentFileInsteadOfHandlerConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	fileSecret := "sk-current-file-secret-value"
	cfgSecret := "sk-stale-handler-secret-value"
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\napi-keys:\n  - "+fileSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(
		&config.Config{Port: 8317, SDKConfig: config.SDKConfig{APIKeys: []string{cfgSecret}}},
		configPath,
		nil,
		WithEnvSecret(""),
	)
	defer func() { _ = h.Close() }()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, fileSecret) || !strings.Contains(body, configYAMLPlaceholderPrefix) {
		t.Fatalf("GetConfigYAML source-driven redaction mismatch: %s", body)
	}
}

func TestPutConfigYAMLPublishesAPIKeyAliasSnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawKey := "sk-yaml-alias-snapshot-value"
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := "port: 8317\napi-key-aliases:\n  " + rawKey + ": before\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(
		&config.Config{Port: 8317, SDKConfig: config.SDKConfig{APIKeyAliases: map[string]string{rawKey: "before"}}},
		configPath,
		nil,
		WithEnvSecret(""),
	)
	defer func() { _ = h.Close() }()

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, want %d body=%s", getRecorder.Code, http.StatusOK, getRecorder.Body.String())
	}
	submitted := strings.Replace(getRecorder.Body.String(), "before", "after", 1)

	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(submitted))
	putContext.Request.Header.Set("Content-Type", "application/yaml")
	h.PutConfigYAML(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d, want %d body=%s", putRecorder.Code, http.StatusOK, putRecorder.Body.String())
	}
	if got := h.apiKeyAliasesSnapshot()[rawKey]; got != "after" {
		t.Fatalf("API key alias snapshot = %q, want after", got)
	}
}

func TestAPIKeyAliasPersistenceFailureRestoresConfigAndSnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawKey := "sk-alias-persist-failure-value"

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		call   func(*Handler, *gin.Context)
	}{
		{
			name:   "put",
			method: http.MethodPut,
			path:   "/api-key-aliases",
			body:   `{"sk-alias-persist-failure-value":"changed"}`,
			call:   (*Handler).PutAPIKeyAliases,
		},
		{
			name:   "patch",
			method: http.MethodPatch,
			path:   "/api-key-aliases",
			body:   `{"key":"sk-alias-persist-failure-value","alias":"changed"}`,
			call:   (*Handler).PatchAPIKeyAlias,
		},
		{
			name:   "delete",
			method: http.MethodDelete,
			path:   "/api-key-aliases",
			body:   `{"key":"sk-alias-persist-failure-value"}`,
			call:   (*Handler).DeleteAPIKeyAlias,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")
			if err := os.WriteFile(configPath, []byte("port: 8317\napi-key-aliases:\n  "+rawKey+": before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Port: 8317, SDKConfig: config.SDKConfig{APIKeyAliases: map[string]string{rawKey: "before"}}}
			h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))
			defer func() { _ = h.Close() }()
			if err := os.Remove(configPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(configPath, 0o700); err != nil {
				t.Fatal(err)
			}

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			c.Request.Header.Set("Content-Type", "application/json")
			test.call(h, c)

			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
			}
			if got := h.cfg.APIKeyAliases[rawKey]; got != "before" {
				t.Fatalf("config alias = %q, want before", got)
			}
			if got := h.apiKeyAliasesSnapshot()[rawKey]; got != "before" {
				t.Fatalf("snapshot alias = %q, want before", got)
			}
		})
	}
}

func TestPutConfigYAMLWithoutCurrentFileOnlyRejectsValidPlaceholders(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	t.Run("prefix in comment", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
		defer func() { _ = h.Close() }()
		body := "port: 8317\n# " + configYAMLPlaceholderPrefix + "not-a-placeholder\n"
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/yaml")
		h.PutConfigYAML(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutConfigYAML status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	})

	t.Run("valid placeholder scalar", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		h := NewHandler(&config.Config{}, configPath, nil, WithEnvSecret(""))
		defer func() { _ = h.Close() }()
		placeholder := configYAMLPlaceholderPrefix + strings.Repeat("a", 32) + "__"
		body := "port: 8317\napi-keys:\n  - " + placeholder + "\n"
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/yaml")
		h.PutConfigYAML(c)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PutConfigYAML status = %d, want %d body=%s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
		}
	})
}

func TestConfigYAMLRejectsMultipleDocuments(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"sk-yaml-multi-doc-secret"}}}
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	multipleDocuments := "api-keys:\n  - sk-yaml-multi-doc-secret\n---\nport: 8317\n"
	if err := os.WriteFile(configPath, []byte(multipleDocuments), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, configPath, nil, WithEnvSecret(""))

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(c)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("GetConfigYAML status = %d, want %d body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(multipleDocuments))
	c.Request.Header.Set("Content-Type", "application/yaml")
	h.PutConfigYAML(c)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PutConfigYAML status = %d, want %d body=%s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
}

func TestAPIKeysMaskedPlaceholdersRoundTrip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawAPIKey := "sk-api-keys-secret-value"
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{rawAPIKey}}}
	h := newConfigBasicTestHandler(t, cfg)
	masked := util.HideAPIKey(rawAPIKey)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api-keys", nil)
	h.GetAPIKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetAPIKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), rawAPIKey) || !strings.Contains(rec.Body.String(), masked) {
		t.Fatalf("GetAPIKeys redaction mismatch: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-keys", strings.NewReader(`["`+masked+`"]`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutAPIKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutAPIKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if snapshot := h.configSnapshot(); len(snapshot.APIKeys) != 1 || snapshot.APIKeys[0] != rawAPIKey {
		t.Fatalf("PutAPIKeys APIKeys = %#v, want raw key", snapshot.APIKeys)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api-keys", strings.NewReader(`{"index":0,"value":"`+masked+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAPIKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PatchAPIKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if snapshot := h.configSnapshot(); len(snapshot.APIKeys) != 1 || snapshot.APIKeys[0] != rawAPIKey {
		t.Fatalf("PatchAPIKeys APIKeys = %#v, want raw key", snapshot.APIKeys)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api-keys?value="+masked, nil)
	h.DeleteAPIKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("DeleteAPIKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if snapshot := h.configSnapshot(); len(snapshot.APIKeys) != 0 {
		t.Fatalf("DeleteAPIKeys APIKeys = %#v, want empty", snapshot.APIKeys)
	}
}

func TestAPIKeyAliasesMaskedKeysRoundTrip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "sk-alias-aone-zzzz"
	rawB := "sk-alias-btwo-zzzz"
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeyAliases: map[string]string{rawA: "a", rawB: "b"}}}
	h := newConfigBasicTestHandler(t, cfg)
	maskedA := util.HideAPIKey(rawA) + "#1"
	maskedB := util.HideAPIKey(rawB) + "#2"

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api-key-aliases", nil)
	h.GetAPIKeyAliases(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetAPIKeyAliases status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, rawA) || strings.Contains(body, rawB) || !strings.Contains(body, maskedA) || !strings.Contains(body, maskedB) {
		t.Fatalf("GetAPIKeyAliases redaction mismatch: %s", body)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-key-aliases", strings.NewReader(`{"`+maskedA+`":"a","`+maskedB+`":"b"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutAPIKeyAliases(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutAPIKeyAliases status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot := h.configSnapshot()
	if snapshot.APIKeyAliases[rawA] != "a" || snapshot.APIKeyAliases[rawB] != "b" {
		t.Fatalf("PutAPIKeyAliases aliases = %#v", snapshot.APIKeyAliases)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api-key-aliases", strings.NewReader(`{"key":"`+maskedA+`","alias":"updated"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAPIKeyAlias(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PatchAPIKeyAlias status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot = h.configSnapshot()
	if snapshot.APIKeyAliases[rawA] != "updated" || snapshot.APIKeyAliases[rawB] != "b" {
		t.Fatalf("PatchAPIKeyAlias aliases = %#v", snapshot.APIKeyAliases)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api-key-aliases", strings.NewReader(`{"key":"`+maskedB+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.DeleteAPIKeyAlias(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("DeleteAPIKeyAlias status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot = h.configSnapshot()
	if _, ok := snapshot.APIKeyAliases[rawB]; ok {
		t.Fatalf("DeleteAPIKeyAlias kept %q in %#v", rawB, snapshot.APIKeyAliases)
	}
}

func TestAPIKeyAliasesRejectAmbiguousMaskedKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "sk-alias-aone-zzzz"
	rawB := "sk-alias-btwo-zzzz"
	masked := util.HideAPIKey(rawA)
	tests := []struct {
		name   string
		method string
		body   string
		call   func(*Handler, *gin.Context)
	}{
		{
			name:   "put",
			method: http.MethodPut,
			body:   `{"` + masked + `":"changed"}`,
			call:   (*Handler).PutAPIKeyAliases,
		},
		{
			name:   "patch",
			method: http.MethodPatch,
			body:   `{"key":"` + masked + `","alias":"changed"}`,
			call:   (*Handler).PatchAPIKeyAlias,
		},
		{
			name:   "delete",
			method: http.MethodDelete,
			body:   `{"key":"` + masked + `"}`,
			call:   (*Handler).DeleteAPIKeyAlias,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeyAliases: map[string]string{rawA: "a", rawB: "b"}}}
			h := newConfigBasicTestHandler(t, cfg)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(tt.method, "/api-key-aliases", strings.NewReader(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			tt.call(h, c)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ambiguous API key placeholder") {
				t.Fatalf("body = %s, want ambiguous placeholder error", rec.Body.String())
			}
			if len(cfg.APIKeyAliases) != 2 || cfg.APIKeyAliases[rawA] != "a" || cfg.APIKeyAliases[rawB] != "b" {
				t.Fatalf("aliases changed after rejected request: %#v", cfg.APIKeyAliases)
			}
			snapshot := h.apiKeyAliasesSnapshot()
			if len(snapshot) != 2 || snapshot[rawA] != "a" || snapshot[rawB] != "b" {
				t.Fatalf("alias snapshot changed after rejected request: %#v", snapshot)
			}
		})
	}
}

func TestPutAPIKeyAliasesRejectsDuplicateAfterPlaceholderResolution(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "sk-alias-aone-zzzz"
	rawB := "sk-alias-btwo-zzzz"
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeyAliases: map[string]string{rawA: "a", rawB: "b"}}}
	h := newConfigBasicTestHandler(t, cfg)
	maskedA := util.HideAPIKey(rawA) + "#1"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api-key-aliases",
		strings.NewReader(`{"`+rawA+`":"raw","`+maskedA+`":"masked"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutAPIKeyAliases(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "duplicate API key after placeholder resolution") {
		t.Fatalf("body = %s, want duplicate placeholder error", rec.Body.String())
	}
	if len(cfg.APIKeyAliases) != 2 || cfg.APIKeyAliases[rawA] != "a" || cfg.APIKeyAliases[rawB] != "b" {
		t.Fatalf("aliases changed after rejected request: %#v", cfg.APIKeyAliases)
	}
}

func TestPutAPIKeyAliasesAcceptsLiteralKeyContainingEllipsis(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	literalKey := "sk-literal...alias-key"
	cfg := &config.Config{}
	h := newConfigBasicTestHandler(t, cfg)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api-key-aliases",
		strings.NewReader(`{"`+literalKey+`":"literal"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutAPIKeyAliases(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot := h.configSnapshot()
	if len(snapshot.APIKeyAliases) != 1 || snapshot.APIKeyAliases[literalKey] != "literal" {
		t.Fatalf("aliases = %#v, want literal key", snapshot.APIKeyAliases)
	}
}

func TestProviderMaskedAPIKeyRoundTrip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawKey := "sk-gemini-roundtrip-value"
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{APIKey: rawKey, BaseURL: "https://example.com/gemini"}}}
	h := newConfigBasicTestHandler(t, cfg)
	masked := util.HideAPIKey(rawKey)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/gemini-api-key", nil)
	h.GetGeminiKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetGeminiKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), rawKey) || !strings.Contains(rec.Body.String(), masked) {
		t.Fatalf("GetGeminiKeys redaction mismatch: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(`[{"api-key":"`+masked+`","base-url":"https://example.com/gemini"}]`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutGeminiKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutGeminiKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot := h.configSnapshot()
	if len(snapshot.GeminiKey) != 1 || snapshot.GeminiKey[0].APIKey != rawKey {
		t.Fatalf("PutGeminiKeys GeminiKey = %#v, want raw key", snapshot.GeminiKey)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/gemini-api-key", strings.NewReader(`{"index":0,"value":{"api-key":"`+masked+`","base-url":"https://example.org/gemini"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchGeminiKey(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PatchGeminiKey status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot = h.configSnapshot()
	if len(snapshot.GeminiKey) != 1 || snapshot.GeminiKey[0].APIKey != rawKey || snapshot.GeminiKey[0].BaseURL != "https://example.org/gemini" {
		t.Fatalf("PatchGeminiKey GeminiKey = %#v", snapshot.GeminiKey)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/gemini-api-key", strings.NewReader(`{"api-key":"`+masked+`","base-url":"https://example.org/gemini"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.DeleteGeminiKey(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("DeleteGeminiKey status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot = h.configSnapshot()
	if len(snapshot.GeminiKey) != 0 {
		t.Fatalf("DeleteGeminiKey GeminiKey = %#v, want empty", snapshot.GeminiKey)
	}
}

func TestProviderMaskedSecretsFollowStableEntryIdentity(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "abcd-first-wxyz"
	rawB := "abcd-second-wxyz"
	masked := util.HideAPIKey(rawA)
	authA := "Bearer auth-first-secret"
	authB := "Bearer auth-second-secret"
	maskedAuthA := util.MaskSensitiveHeaderValue("Authorization", authA)
	maskedAuthB := util.MaskSensitiveHeaderValue("Authorization", authB)
	cfg := &config.Config{GeminiKey: []config.GeminiKey{
		{APIKey: rawA, BaseURL: "https://a.example", Headers: map[string]string{"Authorization": authA}},
		{APIKey: rawB, BaseURL: "https://b.example", Headers: map[string]string{"Authorization": authB}},
	}}
	h := newConfigBasicTestHandler(t, cfg)
	body := fmt.Sprintf(
		`[{"api-key":%q,"base-url":"https://b.example","headers":{"Authorization":%q}},{"api-key":%q,"base-url":"https://a.example","headers":{"Authorization":%q}}]`,
		masked,
		maskedAuthB,
		masked,
		maskedAuthA,
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutGeminiKeys(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutGeminiKeys status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot := h.configSnapshot()
	if len(snapshot.GeminiKey) != 2 {
		t.Fatalf("GeminiKey length = %d, want 2", len(snapshot.GeminiKey))
	}
	if snapshot.GeminiKey[0].APIKey != rawB || snapshot.GeminiKey[0].Headers["Authorization"] != authB {
		t.Fatalf("first reordered provider = %#v, want provider B secrets", snapshot.GeminiKey[0])
	}
	if snapshot.GeminiKey[1].APIKey != rawA || snapshot.GeminiKey[1].Headers["Authorization"] != authA {
		t.Fatalf("second reordered provider = %#v, want provider A secrets", snapshot.GeminiKey[1])
	}
}

func TestProviderRestrictedHeaderChangesAreRejected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawKey := "sk-gemini-restricted-header"
	authorization := "Bearer existing-secret"
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{
		APIKey:  rawKey,
		BaseURL: "https://example.com/gemini",
		Headers: map[string]string{"Authorization": authorization},
	}}}
	h := newConfigBasicTestHandler(t, cfg)
	maskedKey := util.HideAPIKey(rawKey)
	maskedAuthorization := util.MaskSensitiveHeaderValue("Authorization", authorization)

	tests := []struct {
		name string
		body string
	}{
		{
			name: "change existing restricted header",
			body: fmt.Sprintf(
				`[{"api-key":%q,"base-url":"https://example.com/gemini","headers":{"Authorization":"Bearer changed-secret"}}]`,
				maskedKey,
			),
		},
		{
			name: "add restricted header to new provider",
			body: `[{"api-key":"sk-new-provider-key","base-url":"https://new.example/gemini","headers":{"Authorization":"Bearer existing-secret"}}]`,
		},
		{
			name: "reuse one existing provider twice",
			body: fmt.Sprintf(
				`[{"api-key":%q,"base-url":"https://example.com/gemini","headers":{"Authorization":%q}},{"api-key":"sk-duplicate-provider-key","base-url":"https://example.com/gemini","headers":{"Authorization":%q}}]`,
				maskedKey,
				maskedAuthorization,
				maskedAuthorization,
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(test.body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.PutGeminiKeys(c)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("PutGeminiKeys status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if len(cfg.GeminiKey) != 1 || cfg.GeminiKey[0].APIKey != rawKey || cfg.GeminiKey[0].Headers["Authorization"] != authorization {
				t.Fatalf("rejected request changed GeminiKey = %#v", cfg.GeminiKey)
			}
		})
	}
}

func TestProviderAmbiguousMaskedSecretsAreRejected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "abcd-first-wxyz"
	rawB := "abcd-second-wxyz"
	masked := util.HideAPIKey(rawA)
	cfg := &config.Config{GeminiKey: []config.GeminiKey{
		{APIKey: rawA, BaseURL: "https://same.example/gemini"},
		{APIKey: rawB, BaseURL: "https://same.example/gemini"},
	}}
	h := newConfigBasicTestHandler(t, cfg)
	body := fmt.Sprintf(`[{"api-key":%q,"base-url":"https://same.example/gemini"}]`, masked)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutGeminiKeys(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PutGeminiKeys status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if len(cfg.GeminiKey) != 2 || cfg.GeminiKey[0].APIKey != rawA || cfg.GeminiKey[1].APIKey != rawB {
		t.Fatalf("ambiguous request changed GeminiKey = %#v", cfg.GeminiKey)
	}
}

func TestProviderCollidingMaskedAPIKeysRoundTrip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "abcd-first-wxyz"
	rawB := "abcd-second-wxyz"
	masked := util.HideAPIKey(rawA)
	if masked != util.HideAPIKey(rawB) {
		t.Fatal("test API keys do not share a display mask")
	}

	t.Run("provider entries", func(t *testing.T) {
		headerSecret := "abcd-header-wxyz"
		if util.HideAPIKey(headerSecret) != masked {
			t.Fatal("test header secret does not share the API key display mask")
		}
		cfg := &config.Config{GeminiKey: []config.GeminiKey{
			{
				APIKey:  rawA,
				BaseURL: "https://same.example/gemini",
				Headers: map[string]string{"client-secret": headerSecret},
			},
			{APIKey: rawB, BaseURL: "https://same.example/gemini"},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/gemini-api-key", nil)
		h.GetGeminiKeys(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetGeminiKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			GeminiKeys []config.GeminiKey `json:"gemini-api-key"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetGeminiKeys response: %v", err)
		}
		if len(response.GeminiKeys) != 2 || response.GeminiKeys[0].APIKey != masked+"#1" || response.GeminiKeys[1].APIKey != masked+"#2" {
			t.Fatalf("GetGeminiKeys API key masks = %#v", response.GeminiKeys)
		}
		body, err := json.Marshal(response.GeminiKeys)
		if err != nil {
			t.Fatalf("encode PutGeminiKeys body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/gemini-api-key", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutGeminiKeys(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutGeminiKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.GeminiKey) != 2 || snapshot.GeminiKey[0].APIKey != rawA || snapshot.GeminiKey[1].APIKey != rawB {
			t.Fatalf("PutGeminiKeys API keys = %#v", snapshot.GeminiKey)
		}
		if snapshot.GeminiKey[0].Headers["client-secret"] != headerSecret {
			t.Fatalf("PutGeminiKeys client-secret = %q, want original", snapshot.GeminiKey[0].Headers["client-secret"])
		}
	})

	t.Run("nested API key entries", func(t *testing.T) {
		cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "openrouter",
			BaseURL: "https://openrouter.ai/api/v1",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: rawA, ProxyURL: "http://127.0.0.1:8080"},
				{APIKey: rawB, ProxyURL: "http://127.0.0.1:8080"},
			},
		}}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
		h.GetOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetOpenAICompat response: %v", err)
		}
		if len(response.Entries) != 1 || len(response.Entries[0].APIKeyEntries) != 2 ||
			response.Entries[0].APIKeyEntries[0].APIKey != masked+"#1" ||
			response.Entries[0].APIKeyEntries[1].APIKey != masked+"#2" {
			t.Fatalf("GetOpenAICompat API key masks = %#v", response.Entries)
		}
		body, err := json.Marshal(response.Entries)
		if err != nil {
			t.Fatalf("encode PutOpenAICompat body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.OpenAICompatibility) != 1 || len(snapshot.OpenAICompatibility[0].APIKeyEntries) != 2 ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != rawA ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[1].APIKey != rawB {
			t.Fatalf("PutOpenAICompat API keys = %#v", snapshot.OpenAICompatibility)
		}
	})

	t.Run("nested API key entries stay provider scoped", func(t *testing.T) {
		cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "first",
				BaseURL: "https://first.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: rawA},
				},
			},
			{
				Name:    "second",
				BaseURL: "https://second.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: rawA, ProxyURL: "http://127.0.0.1:8080"},
					{APIKey: rawB, ProxyURL: "http://127.0.0.1:8080"},
				},
			},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
		h.GetOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetOpenAICompat response: %v", err)
		}
		if len(response.Entries) != 2 || len(response.Entries[1].APIKeyEntries) != 2 ||
			response.Entries[0].APIKeyEntries[0].APIKey != masked ||
			response.Entries[1].APIKeyEntries[0].APIKey != masked+"#1" ||
			response.Entries[1].APIKeyEntries[1].APIKey != masked+"#2" {
			t.Fatalf("GetOpenAICompat scoped API key masks = %#v", response.Entries)
		}
		body, err := json.Marshal(response.Entries)
		if err != nil {
			t.Fatalf("encode PutOpenAICompat body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.OpenAICompatibility) != 2 ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != rawA ||
			snapshot.OpenAICompatibility[1].APIKeyEntries[0].APIKey != rawA ||
			snapshot.OpenAICompatibility[1].APIKeyEntries[1].APIKey != rawB {
			t.Fatalf("PutOpenAICompat scoped API keys = %#v", snapshot.OpenAICompatibility)
		}
	})

	t.Run("renamed provider keeps local API key ordinal scope", func(t *testing.T) {
		firstA := "abcd-first-one-wxyz"
		firstB := "abcd-first-two-wxyz"
		secondA := "abcd-second-one-wxyz"
		secondB := "abcd-second-two-wxyz"
		for _, key := range []string{firstA, firstB, secondA, secondB} {
			if util.HideAPIKey(key) != masked {
				t.Fatalf("test API key %q does not share display mask %q", key, masked)
			}
		}
		cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "first",
				BaseURL: "https://first.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: firstA},
					{APIKey: firstB},
				},
			},
			{
				Name:    "second",
				BaseURL: "https://second.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: secondA},
					{APIKey: secondB},
				},
			},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
		h.GetOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetOpenAICompat response: %v", err)
		}
		if len(response.Entries) != 2 {
			t.Fatalf("GetOpenAICompat entries = %#v", response.Entries)
		}
		response.Entries[1].Name = "renamed-second"
		body, err := json.Marshal(response.Entries)
		if err != nil {
			t.Fatalf("encode PutOpenAICompat body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.OpenAICompatibility) != 2 ||
			snapshot.OpenAICompatibility[1].Name != "renamed-second" ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != firstA ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[1].APIKey != firstB ||
			snapshot.OpenAICompatibility[1].APIKeyEntries[0].APIKey != secondA ||
			snapshot.OpenAICompatibility[1].APIKeyEntries[1].APIKey != secondB {
			t.Fatalf("PutOpenAICompat renamed provider = %#v", snapshot.OpenAICompatibility)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
		h.GetOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("second GetOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		response = struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}{}
		if err = json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode second GetOpenAICompat response: %v", err)
		}
		response.Entries[1].Name = "renamed-and-moved-second"
		response.Entries[1].BaseURL = "https://moved-second.example/v1"
		body, err = json.Marshal(response.Entries)
		if err != nil {
			t.Fatalf("encode ambiguous PutOpenAICompat body: %v", err)
		}
		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutOpenAICompat(c)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("ambiguous PutOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
		afterRejectedUpdate := h.configSnapshot()
		if len(afterRejectedUpdate.OpenAICompatibility) != 2 ||
			afterRejectedUpdate.OpenAICompatibility[1].Name != "renamed-second" ||
			afterRejectedUpdate.OpenAICompatibility[1].BaseURL != "https://second.example/v1" ||
			afterRejectedUpdate.OpenAICompatibility[1].APIKeyEntries[0].APIKey != secondA ||
			afterRejectedUpdate.OpenAICompatibility[1].APIKeyEntries[1].APIKey != secondB {
			t.Fatalf("rejected provider update changed config = %#v", afterRejectedUpdate.OpenAICompatibility)
		}
	})

	t.Run("masked keys cannot move to another provider identity", func(t *testing.T) {
		firstKey := "abcd-first-one-wxyz"
		secondKey := "abcd-second-one-wxyz"
		if util.HideAPIKey(firstKey) != util.HideAPIKey(secondKey) {
			t.Fatal("test API keys do not share a display mask")
		}
		cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "first",
				BaseURL:       "https://first.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: firstKey}},
			},
			{
				Name:          "second",
				BaseURL:       "https://second.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: secondKey}},
			},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
		h.GetOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetOpenAICompat response: %v", err)
		}
		moved := response.Entries[1]
		moved.Name = response.Entries[0].Name
		moved.BaseURL = "https://moved.example/v1"
		body, err := json.Marshal([]config.OpenAICompatibility{moved})
		if err != nil {
			t.Fatalf("encode PutOpenAICompat body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutOpenAICompat(c)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("PutOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.OpenAICompatibility) != 2 ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != firstKey ||
			snapshot.OpenAICompatibility[1].APIKeyEntries[0].APIKey != secondKey {
			t.Fatalf("rejected provider move changed config = %#v", snapshot.OpenAICompatibility)
		}
	})

	t.Run("used colliding source remains ambiguous", func(t *testing.T) {
		firstKey := "wxyz-first-one-abcd"
		secondKey := "abcd-second-one-wxyz"
		thirdKey := "abcd-third-one-wxyz"
		if util.HideAPIKey(secondKey) != util.HideAPIKey(thirdKey) {
			t.Fatal("test API keys do not share a display mask")
		}
		cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "first",
				BaseURL:       "https://first.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: firstKey}},
			},
			{
				Name:          "second",
				BaseURL:       "https://second.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: secondKey}},
			},
			{
				Name:          "third",
				BaseURL:       "https://third.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: thirdKey}},
			},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
		h.GetOpenAICompat(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetOpenAICompat response: %v", err)
		}
		keptThird := response.Entries[2]
		movedThird := response.Entries[2]
		movedThird.Name = response.Entries[1].Name
		movedThird.BaseURL = "https://moved.example/v1"
		body, err := json.Marshal([]config.OpenAICompatibility{keptThird, movedThird})
		if err != nil {
			t.Fatalf("encode PutOpenAICompat body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutOpenAICompat(c)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("PutOpenAICompat status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.OpenAICompatibility) != 3 ||
			snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != firstKey ||
			snapshot.OpenAICompatibility[1].APIKeyEntries[0].APIKey != secondKey ||
			snapshot.OpenAICompatibility[2].APIKeyEntries[0].APIKey != thirdKey {
			t.Fatalf("rejected provider reuse changed config = %#v", snapshot.OpenAICompatibility)
		}
	})

	t.Run("same-length put cannot copy used ordinal credential", func(t *testing.T) {
		firstKey := "abcd-first-one-wxyz"
		secondKey := "abcd-second-one-wxyz"
		if util.HideAPIKey(firstKey) != util.HideAPIKey(secondKey) {
			t.Fatal("test API keys do not share a display mask")
		}
		cfg := &config.Config{ClaudeKey: []config.ClaudeKey{
			{APIKey: firstKey, BaseURL: "https://first.example/v1"},
			{APIKey: secondKey, BaseURL: "https://second.example/v1"},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/claude-api-key", nil)
		h.GetClaudeKeys(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetClaudeKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.ClaudeKey `json:"claude-api-key"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetClaudeKeys response: %v", err)
		}
		if len(response.Entries) != 2 || !strings.HasSuffix(response.Entries[1].APIKey, "#2") {
			t.Fatalf("GetClaudeKeys entries = %#v", response.Entries)
		}
		copiedFirst := response.Entries[0]
		copiedFirst.APIKey = response.Entries[1].APIKey
		body, err := json.Marshal([]config.ClaudeKey{response.Entries[1], copiedFirst})
		if err != nil {
			t.Fatalf("encode PutClaudeKeys body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/claude-api-key", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutClaudeKeys(c)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("PutClaudeKeys status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.ClaudeKey) != 2 || snapshot.ClaudeKey[0].APIKey != firstKey || snapshot.ClaudeKey[1].APIKey != secondKey {
			t.Fatalf("rejected credential copy changed config = %#v", snapshot.ClaudeKey)
		}
	})

	t.Run("ordinal delete resolves before secondary match", func(t *testing.T) {
		firstKey := "abcd-first-one-wxyz"
		secondKey := "abcd-second-one-wxyz"
		if util.HideAPIKey(firstKey) != util.HideAPIKey(secondKey) {
			t.Fatal("test API keys do not share a display mask")
		}
		cfg := &config.Config{ClaudeKey: []config.ClaudeKey{
			{APIKey: firstKey, BaseURL: "https://first.example/v1"},
			{APIKey: secondKey, BaseURL: "https://second.example/v1"},
		}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/claude-api-key", nil)
		h.GetClaudeKeys(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetClaudeKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			Entries []config.ClaudeKey `json:"claude-api-key"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetClaudeKeys response: %v", err)
		}
		if len(response.Entries) != 2 || !strings.HasSuffix(response.Entries[1].APIKey, "#2") {
			t.Fatalf("GetClaudeKeys entries = %#v", response.Entries)
		}
		body, err := json.Marshal(map[string]string{
			"api-key":  response.Entries[1].APIKey,
			"base-url": response.Entries[1].BaseURL,
		})
		if err != nil {
			t.Fatalf("encode delete body: %v", err)
		}

		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodDelete, "/claude-api-key", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.DeleteClaudeKey(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("DeleteClaudeKey status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		snapshot := h.configSnapshot()
		if len(snapshot.ClaudeKey) != 1 || snapshot.ClaudeKey[0].APIKey != firstKey {
			t.Fatalf("DeleteClaudeKey config = %#v, want first credential only", snapshot.ClaudeKey)
		}
	})

	t.Run("natural numeric suffix", func(t *testing.T) {
		rawKey := "abcd-secret-#123"
		maskedKey := util.HideAPIKey(rawKey)
		if !strings.HasSuffix(maskedKey, "#123") {
			t.Fatalf("test mask = %q, want numeric suffix", maskedKey)
		}
		cfg := &config.Config{GeminiKey: []config.GeminiKey{{
			APIKey:  rawKey,
			BaseURL: "https://numeric.example/gemini",
		}}}
		h := newConfigBasicTestHandler(t, cfg)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/gemini-api-key", nil)
		h.GetGeminiKeys(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetGeminiKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response struct {
			GeminiKeys []config.GeminiKey `json:"gemini-api-key"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetGeminiKeys response: %v", err)
		}
		body, err := json.Marshal(response.GeminiKeys)
		if err != nil {
			t.Fatalf("encode PutGeminiKeys body: %v", err)
		}
		recorder = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPut, "/gemini-api-key", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.PutGeminiKeys(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutGeminiKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		if got := h.configSnapshot().GeminiKey[0].APIKey; got != rawKey {
			t.Fatalf("PutGeminiKeys API key = %q, want %q", got, rawKey)
		}
	})
}

func TestOpenAICompatibilityNestedAPIKeyPlaceholdersRoundTrip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawKey := "sk-openai-nested-value"
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "openrouter",
		BaseURL: "https://openrouter.ai/api/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{
			{APIKey: rawKey, ProxyURL: "http://127.0.0.1:8080"},
		},
	}}}
	h := newConfigBasicTestHandler(t, cfg)
	masked := util.HideAPIKey(rawKey)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/openai-compatibility", nil)
	h.GetOpenAICompat(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetOpenAICompat status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), rawKey) || !strings.Contains(rec.Body.String(), masked) {
		t.Fatalf("GetOpenAICompat redaction mismatch: %s", rec.Body.String())
	}

	body := `[{"name":"openrouter","base-url":"https://openrouter.ai/api/v1","api-key-entries":[{"api-key":"` + masked + `","proxy-url":"http://127.0.0.1:8080"}]}]`
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/openai-compatibility", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutOpenAICompat(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutOpenAICompat status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot := h.configSnapshot()
	if len(snapshot.OpenAICompatibility) != 1 || len(snapshot.OpenAICompatibility[0].APIKeyEntries) != 1 || snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != rawKey {
		t.Fatalf("PutOpenAICompat OpenAICompatibility = %#v", snapshot.OpenAICompatibility)
	}

	patchBody := `{"index":0,"value":{"name":"openrouter","base-url":"https://openrouter.ai/api/v1","api-key-entries":[{"api-key":"` + masked + `","proxy-url":"http://127.0.0.1:8181"}]}}`
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/openai-compatibility", strings.NewReader(patchBody))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchOpenAICompat(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PatchOpenAICompat status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	snapshot = h.configSnapshot()
	if snapshot.OpenAICompatibility[0].APIKeyEntries[0].APIKey != rawKey || snapshot.OpenAICompatibility[0].APIKeyEntries[0].ProxyURL != "http://127.0.0.1:8181" {
		t.Fatalf("PatchOpenAICompat OpenAICompatibility = %#v", snapshot.OpenAICompatibility)
	}
}

func TestAmbiguousMaskedAPIKeyPlaceholderIsRejected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "sk-ambiguous-one-zzzz"
	rawB := "sk-ambiguous-two-zzzz"
	masked := util.HideAPIKey(rawA)
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{rawA, rawB}}}
	h := newConfigBasicTestHandler(t, cfg)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api-keys", strings.NewReader(`{"old":"`+rawA+`","new":"`+masked+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAPIKeys(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PatchAPIKeys ambiguous status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if cfg.APIKeys[0] != rawA || cfg.APIKeys[1] != rawB {
		t.Fatalf("ambiguous placeholder changed APIKeys = %#v", cfg.APIKeys)
	}
}

func TestAPIKeyCollisionPlaceholdersAreOrdinalAndUnambiguous(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "sk-ambiguous-one-zzzz"
	rawB := "sk-ambiguous-two-zzzz"
	masked := util.HideAPIKey(rawA)
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{rawA, rawB}}}
	h := newConfigBasicTestHandler(t, cfg)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api-keys", nil)
	h.GetAPIKeys(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GetAPIKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		APIKeys []string `json:"api-keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GetAPIKeys response: %v", err)
	}
	if len(response.APIKeys) != 2 || response.APIKeys[0] != masked+"#1" || response.APIKeys[1] != masked+"#2" {
		t.Fatalf("GetAPIKeys placeholders = %#v", response.APIKeys)
	}

	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-keys", strings.NewReader(`[`+fmt.Sprintf("%q", response.APIKeys[1])+`]`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutAPIKeys(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PutAPIKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if snapshot := h.configSnapshot(); len(snapshot.APIKeys) != 1 || snapshot.APIKeys[0] != rawB {
		t.Fatalf("PutAPIKeys APIKeys = %#v, want second raw key", snapshot.APIKeys)
	}
}

func TestAmbiguousAPIKeyListMutationsAreRejected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "sk-ambiguous-one-zzzz"
	rawB := "sk-ambiguous-two-zzzz"
	masked := util.HideAPIKey(rawA)

	for _, test := range []struct {
		name   string
		method string
		target string
		body   string
		call   func(*Handler, *gin.Context)
	}{
		{
			name:   "put",
			method: http.MethodPut,
			target: "/api-keys",
			body:   `[` + fmt.Sprintf("%q", masked) + `]`,
			call:   (*Handler).PutAPIKeys,
		},
		{
			name:   "patch",
			method: http.MethodPatch,
			target: "/api-keys",
			body:   `{"old":` + fmt.Sprintf("%q", masked) + `,"new":"sk-brand-new-value"}`,
			call:   (*Handler).PatchAPIKeys,
		},
		{
			name:   "delete",
			method: http.MethodDelete,
			target: "/api-keys?value=" + url.QueryEscape(masked),
			call:   (*Handler).DeleteAPIKeys,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{rawA, rawB}}}
			h := newConfigBasicTestHandler(t, cfg)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			if test.body != "" {
				c.Request.Header.Set("Content-Type", "application/json")
			}
			test.call(h, c)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if len(cfg.APIKeys) != 2 || cfg.APIKeys[0] != rawA || cfg.APIKeys[1] != rawB {
				t.Fatalf("rejected mutation changed APIKeys = %#v", cfg.APIKeys)
			}
		})
	}
}

func TestAPIKeyContainingEllipsisIsAcceptedAsLiteral(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	const literal = "abcd...wxyz"
	cfg := &config.Config{}
	h := newConfigBasicTestHandler(t, cfg)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-keys", strings.NewReader(`[`+fmt.Sprintf("%q", literal)+`]`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutAPIKeys(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PutAPIKeys status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if snapshot := h.configSnapshot(); len(snapshot.APIKeys) != 1 || snapshot.APIKeys[0] != literal {
		t.Fatalf("PutAPIKeys APIKeys = %#v, want literal ellipsis key", snapshot.APIKeys)
	}
}

func TestGetUsageStatisticsTruncatesOversizedLegacySnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	base := time.Now().UTC().Add(-time.Hour)
	for index := range 3 {
		persister.Record(usage.FlatDetail{
			Timestamp: base.Add(time.Duration(index) * time.Minute),
			Model:     "legacy-limit-model",
			Completed: true,
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		totalRequests, _, err := persister.QueryRequestCountsContext(context.Background())
		if err == nil && totalRequests == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records were not persisted before deadline: total=%d error=%v", totalRequests, err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret(""), WithUsagePersister(persister))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage", nil)
	h.getUsageStatistics(c, 2)

	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"GetUsageStatistics status = %d, want %d body=%s",
			recorder.Code,
			http.StatusOK,
			recorder.Body.String(),
		)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"details_truncated":true`) ||
		!strings.Contains(body, "compatibility memory budget") ||
		!strings.Contains(body, "/usage/summary") ||
		!strings.Contains(body, `"total_requests":3`) ||
		!strings.Contains(body, `"apis":{}`) {
		t.Fatalf("GetUsageStatistics body = %s, want bounded aggregate with warning", body)
	}

	aggregateRecorder := httptest.NewRecorder()
	aggregateContext, _ := gin.CreateTestContext(aggregateRecorder)
	aggregateContext.Request = httptest.NewRequest(http.MethodGet, "/usage?details=false", nil)
	h.getUsageStatistics(aggregateContext, 2)
	if aggregateRecorder.Code != http.StatusOK {
		t.Fatalf(
			"GetUsageStatistics aggregate status = %d, want %d body=%s",
			aggregateRecorder.Code,
			http.StatusOK,
			aggregateRecorder.Body.String(),
		)
	}
	aggregateBody := aggregateRecorder.Body.String()
	if !strings.Contains(aggregateBody, `"total_requests":3`) ||
		strings.Contains(aggregateBody, `"details"`) ||
		!strings.Contains(aggregateBody, `"apis":{}`) {
		t.Fatalf("GetUsageStatistics aggregate body = %s, want bounded totals without details", aggregateBody)
	}
}

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

func TestGetUsageSummaryAggregatesMaskedAPIKeyCollisions(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	rawA := "abcd-first-wxyz"
	rawB := "abcd-second-wxyz"
	masked := util.HideAPIKey(rawA)
	credentialRawA := "sk-credential-first-wxyz"
	credentialRawB := "sk-credential-other-wxyz"
	credentialMasked := util.HideAPIKey(credentialRawA)
	if rawA == rawB || masked != util.HideAPIKey(rawB) ||
		credentialRawA == credentialRawB || credentialMasked != util.HideAPIKey(credentialRawB) {
		t.Fatalf("invalid collision fixtures: %q/%q, %q/%q", rawA, rawB, credentialRawA, credentialRawB)
	}

	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.SetPriceFunc(func(string) (float64, float64, float64, bool) {
		return 1000, 2000, 1000, true
	})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	bucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	first := usage.FlatDetail{
		Timestamp: bucket.Add(5 * time.Minute),
		Model:     "collision-model",
		APIKey:    rawA,
		Source:    credentialRawA,
		Provider:  "test",
		LatencyMs: 100,
		Completed: true,
	}
	first.Tokens.InputTokens = 1
	first.Tokens.CachedTokens = 3
	first.Tokens.TotalTokens = 4
	second := usage.FlatDetail{
		Timestamp: bucket.Add(10 * time.Minute),
		Model:     "collision-model",
		APIKey:    rawB,
		Source:    credentialRawB,
		Provider:  "test",
		LatencyMs: 300,
		Completed: true,
		Failed:    true,
	}
	second.Tokens.OutputTokens = 2
	second.Tokens.ReasoningTokens = 5
	second.Tokens.TotalTokens = 7
	third := usage.FlatDetail{
		Timestamp: bucket.Add(time.Hour + 5*time.Minute),
		Model:     "collision-model",
		APIKey:    rawA,
		Source:    usage.TagAPIKeyCredentialSource(credentialMasked, true),
		Provider:  "test",
		LatencyMs: 200,
		Completed: true,
	}
	third.Tokens.InputTokens = 2
	third.Tokens.TotalTokens = 2
	persister.Record(first)
	persister.Record(second)
	persister.Record(third)

	h := NewHandler(
		&config.Config{},
		"",
		nil,
		WithEnvSecret(""),
		WithUsagePersister(persister),
	)
	basePath := fmt.Sprintf(
		"/usage/summary?from=%s&to=%s",
		bucket.Format(time.RFC3339),
		bucket.Add(time.Hour+59*time.Minute).Format(time.RFC3339),
	)
	requestSummary := func(path string) (summaryResponse, string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		h.GetUsageSummary(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetUsageSummary status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response summaryResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode usage summary: %v body=%s", err, recorder.Body.String())
		}
		return response, recorder.Body.String()
	}

	var response summaryResponse
	var responseBody string
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, responseBody = requestSummary(basePath)
		if response.Totals.Requests == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records were not persisted before deadline: %s", responseBody)
		}
		time.Sleep(25 * time.Millisecond)
	}

	assertAggregated := func(label string, got summaryResponse) {
		t.Helper()
		if got.Totals.Requests != 3 || got.Totals.Success != 2 || got.Totals.Failure != 1 {
			t.Fatalf("%s totals = %+v, want requests/success/failure 3/2/1", label, got.Totals)
		}
		if len(got.ByAPIKey) != 1 {
			t.Fatalf("%s by_api_key has %d entries, want 1: %#v", label, len(got.ByAPIKey), got.ByAPIKey)
		}
		stats := got.ByAPIKey[masked]
		if stats == nil {
			t.Fatalf("%s missing masked API key %q: %#v", label, masked, got.ByAPIKey)
		}
		if stats.Requests != 3 || stats.Success != 2 || stats.Failure != 1 {
			t.Fatalf("%s API key counts = %+v, want requests/success/failure 3/2/1", label, stats)
		}
		wantTokens := (summaryTokens{Input: 3, Output: 2, Cached: 3, Reasoning: 5, Total: 13})
		if stats.Tokens != wantTokens {
			t.Fatalf("%s API key tokens = %+v, want %+v", label, stats.Tokens, wantTokens)
		}
		if stats.Cost != 0.01 {
			t.Fatalf("%s API key cost = %v, want 0.01", label, stats.Cost)
		}
		if stats.TotalLatencyMs == nil || *stats.TotalLatencyMs != 600 ||
			stats.AverageLatencyMs == nil || *stats.AverageLatencyMs != 200 ||
			stats.LatencySampleCount != 3 {
			t.Fatalf("%s API key latency = %+v, want total/average/samples 600/200/3", label, stats)
		}

		credentialKey := "test:" + credentialMasked + " [api_key]"
		if len(got.ByCredential) != 1 {
			t.Fatalf("%s by_credential has %d entries, want 1: %#v", label, len(got.ByCredential), got.ByCredential)
		}
		credentialStats := got.ByCredential[credentialKey]
		if credentialStats == nil {
			t.Fatalf("%s missing masked credential %q: %#v", label, credentialKey, got.ByCredential)
		}
		if credentialStats.Success != 2 || credentialStats.Failure != 1 ||
			credentialStats.Provider != "test" || credentialStats.Source != credentialMasked {
			t.Fatalf("%s credential stats = %+v, want success/failure/provider/source 2/1/test/%s", label, credentialStats, credentialMasked)
		}
		credentialSeries := got.TimeSeriesByCredential[credentialKey]
		if len(credentialSeries) != 2 || credentialSeries[0].Requests != 2 || credentialSeries[1].Requests != 1 {
			t.Fatalf("%s credential time series = %#v, want request counts 2/1", label, credentialSeries)
		}

		series := got.TimeSeriesByAPIKey[masked]
		if len(series) != 2 {
			t.Fatalf("%s masked time series has %d points, want 2: %#v", label, len(series), series)
		}
		firstPoint := series[0]
		firstTokens := summaryTokens{Input: 1, Output: 2, Cached: 3, Reasoning: 5, Total: 11}
		if firstPoint.Time != bucket.Format(time.RFC3339) ||
			firstPoint.Requests != 2 || firstPoint.Success != 1 || firstPoint.Failure != 1 ||
			firstPoint.Tokens != firstTokens || firstPoint.Cost != 0.01 || !firstPoint.HasCost {
			t.Fatalf("%s first masked time point = %+v", label, firstPoint)
		}
		secondPoint := series[1]
		secondTokens := summaryTokens{Input: 2, Total: 2}
		if secondPoint.Time != bucket.Add(time.Hour).Format(time.RFC3339) ||
			secondPoint.Requests != 1 || secondPoint.Success != 1 || secondPoint.Failure != 0 ||
			secondPoint.Tokens != secondTokens || secondPoint.Cost != 0 || !secondPoint.HasCost {
			t.Fatalf("%s second masked time point = %+v", label, secondPoint)
		}
	}

	assertNoRawSecrets := func(label, body string) {
		t.Helper()
		for _, raw := range []string{rawA, rawB, credentialRawA, credentialRawB} {
			if strings.Contains(body, raw) {
				t.Fatalf("%s kept raw secret %q: %s", label, raw, body)
			}
		}
	}
	assertNoRawSecrets("usage summary", responseBody)
	assertAggregated("unfiltered", response)

	filtered, filteredBody := requestSummary(basePath + "&api_key=" + masked)
	assertNoRawSecrets("API-key filtered usage summary", filteredBody)
	assertAggregated("masked API-key filter", filtered)

	credentialFiltered, credentialFilteredBody := requestSummary(
		basePath + "&credential=test:" + credentialMasked,
	)
	assertNoRawSecrets("credential filtered usage summary", credentialFilteredBody)
	assertAggregated("provider-qualified masked credential filter", credentialFiltered)

	rawCredentialFiltered, rawCredentialFilteredBody := requestSummary(
		basePath + "&credential=" + credentialRawA,
	)
	assertNoRawSecrets("raw credential filtered usage summary", rawCredentialFilteredBody)
	if rawCredentialFiltered.Totals.Requests != 2 {
		t.Fatalf("raw credential filter requests = %d, want historical and tagged rows", rawCredentialFiltered.Totals.Requests)
	}

	eventsRecorder := httptest.NewRecorder()
	eventsContext, _ := gin.CreateTestContext(eventsRecorder)
	eventsContext.Request = httptest.NewRequest(
		http.MethodGet,
		strings.Replace(basePath, "/usage/summary?", "/usage/events?", 1),
		nil,
	)
	h.GetUsageEvents(eventsContext)
	if eventsRecorder.Code != http.StatusOK {
		t.Fatalf("GetUsageEvents status = %d, want %d body=%s", eventsRecorder.Code, http.StatusOK, eventsRecorder.Body.String())
	}
	assertNoRawSecrets("usage events", eventsRecorder.Body.String())

	usageRecorder := httptest.NewRecorder()
	usageContext, _ := gin.CreateTestContext(usageRecorder)
	usageContext.Request = httptest.NewRequest(http.MethodGet, "/usage", nil)
	h.GetUsageStatistics(usageContext)
	if usageRecorder.Code != http.StatusOK {
		t.Fatalf("GetUsageStatistics status = %d, want %d body=%s", usageRecorder.Code, http.StatusOK, usageRecorder.Body.String())
	}
	assertNoRawSecrets("legacy usage snapshot", usageRecorder.Body.String())
}

func TestGetUsageSummarySeparatesCredentialSourceKinds(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	bucket := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	display := "abcd...wxyz"
	persister.Record(usage.FlatDetail{
		Timestamp: bucket.Add(5 * time.Minute),
		Model:     "kind-collision-model",
		Source:    usage.TagIdentityCredentialSource(display),
		Provider:  "test",
		Completed: true,
	})
	persister.Record(usage.FlatDetail{
		Timestamp: bucket.Add(10 * time.Minute),
		Model:     "kind-collision-model",
		Source:    usage.TagAPIKeyCredentialSource(display, true),
		Provider:  "test",
		Completed: true,
	})

	h := NewHandler(&config.Config{}, "", nil, WithEnvSecret(""), WithUsagePersister(persister))
	basePath := fmt.Sprintf(
		"/usage/summary?from=%s&to=%s",
		bucket.Format(time.RFC3339),
		bucket.Add(59*time.Minute).Format(time.RFC3339),
	)
	requestSummary := func(path string) summaryResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		h.GetUsageSummary(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetUsageSummary status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response summaryResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode usage summary: %v body=%s", err, recorder.Body.String())
		}
		return response
	}

	var response summaryResponse
	deadline := time.Now().Add(5 * time.Second)
	for {
		response = requestSummary(basePath)
		if response.Totals.Requests == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records were not persisted before deadline: %#v", response)
		}
		time.Sleep(25 * time.Millisecond)
	}

	identityKey := "test:" + display + " [identity]"
	apiKeyKey := "test:" + display + " [api_key]"
	for _, key := range []string{identityKey, apiKeyKey} {
		wantKind := "api_key"
		if key == identityKey {
			wantKind = "identity"
		}
		stats := response.ByCredential[key]
		if stats == nil || stats.Success != 1 || stats.Failure != 0 || stats.Source != display ||
			stats.SourceKind != wantKind || stats.FilterKey == "" {
			t.Fatalf("credential %q = %#v, want one successful %s request with a typed filter key", key, stats, wantKind)
		}
		filtered := requestSummary(basePath + "&credential=" + url.QueryEscape(stats.FilterKey))
		if filtered.Totals.Requests != 1 || len(filtered.ByCredential) != 1 || filtered.ByCredential[key] == nil {
			t.Fatalf("credential filter %q = totals %d entries %#v, want stable key with one request", stats.FilterKey, filtered.Totals.Requests, filtered.ByCredential)
		}
	}
}

func TestGetUsageSummaryAPIKeyFilterBoundaries(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	collidingRawA := "ab,c-first-zzzz"
	collidingRawB := "ab,c-second-zzzz"
	keys := []string{
		"abcd...wxyz",
		"abcd-secret-wxyz",
		"(none)",
		"",
		collidingRawA,
		collidingRawB,
	}
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	bucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	for _, key := range keys {
		detail := usage.FlatDetail{
			Timestamp: bucket,
			Model:     "filter-boundary-model",
			APIKey:    key,
			Provider:  "test",
			Completed: true,
		}
		detail.Tokens.TotalTokens = 1
		persister.Record(detail)
	}

	h := NewHandler(
		&config.Config{
			SDKConfig: config.SDKConfig{
				APIKeyAliases: map[string]string{
					collidingRawA: "comma-first",
					collidingRawB: "comma-second",
				},
			},
		},
		"",
		nil,
		WithEnvSecret(""),
		WithUsagePersister(persister),
	)
	basePath := fmt.Sprintf(
		"/usage/summary?from=%s&to=%s",
		bucket.Add(-time.Hour).Format(time.RFC3339),
		bucket.Add(time.Hour).Format(time.RFC3339),
	)
	withAPIKeyFilters := func(path string, filters ...string) string {
		for _, filter := range filters {
			if filter != "" {
				path += "&api_key=" + filter
			}
		}
		return path
	}
	requestTotals := func(filters ...string) int64 {
		t.Helper()
		path := withAPIKeyFilters(basePath, filters...)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		h.GetUsageSummary(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetUsageSummary(%q) status = %d, want %d body=%s", filters, recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response summaryResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetUsageSummary(%q): %v body=%s", filters, err, recorder.Body.String())
		}
		return response.Totals.Requests
	}

	deadline := time.Now().Add(5 * time.Second)
	for requestTotals("") != int64(len(keys)) {
		if time.Now().After(deadline) {
			t.Fatalf("usage records were not persisted before deadline")
		}
		time.Sleep(25 * time.Millisecond)
	}

	tests := []struct {
		name   string
		filter string
		want   int64
	}{
		{name: "exact raw and masked display", filter: "abcd...wxyz", want: 2},
		{name: "literal none and null sentinel", filter: "(none)", want: 2},
		{name: "masked display containing comma", filter: util.HideAPIKey("ab,c-first-zzzz"), want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestTotals(test.filter); got != test.want {
				t.Fatalf("GetUsageSummary(%q) totals = %d, want %d", test.filter, got, test.want)
			}
		})
	}

	repeated := []string{util.HideAPIKey("ab,c-first-zzzz"), "abcd...wxyz"}
	if got := requestTotals(repeated...); got != 4 {
		t.Fatalf("GetUsageSummary(repeated API keys) totals = %d, want 4", got)
	}

	eventsPath := strings.Replace(basePath, "/usage/summary?", "/usage/events?", 1)
	eventsRecorder := httptest.NewRecorder()
	eventsContext, _ := gin.CreateTestContext(eventsRecorder)
	eventsContext.Request = httptest.NewRequest(
		http.MethodGet,
		withAPIKeyFilters(eventsPath, repeated...),
		nil,
	)
	h.GetUsageEvents(eventsContext)
	if eventsRecorder.Code != http.StatusOK {
		t.Fatalf(
			"GetUsageEvents(repeated API keys) status = %d, want %d body=%s",
			eventsRecorder.Code,
			http.StatusOK,
			eventsRecorder.Body.String(),
		)
	}
	var events eventsResponse
	if err := json.Unmarshal(eventsRecorder.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode GetUsageEvents(repeated API keys): %v body=%s", err, eventsRecorder.Body.String())
	}
	if events.Total != 4 || len(events.Events) != 4 {
		t.Fatalf(
			"GetUsageEvents(repeated API keys) total/events = %d/%d, want 4/4",
			events.Total,
			len(events.Events),
		)
	}
	if strings.Contains(eventsRecorder.Body.String(), collidingRawA) ||
		strings.Contains(eventsRecorder.Body.String(), collidingRawB) {
		t.Fatalf("GetUsageEvents returned raw colliding API key: %s", eventsRecorder.Body.String())
	}
	aliases := make(map[string]string)
	for _, event := range events.Events {
		if event.APIKeyAlias != "" {
			aliases[event.APIKeyAlias] = event.APIKey
		}
	}
	wantMaskedCollision := util.HideAPIKey(collidingRawA)
	if aliases["comma-first"] != wantMaskedCollision || aliases["comma-second"] != wantMaskedCollision {
		t.Fatalf("GetUsageEvents collision aliases = %#v, want both mapped to %q", aliases, wantMaskedCollision)
	}

	exportPath := strings.Replace(basePath, "/usage/summary?", "/usage/export?", 1)
	exportRecorder := httptest.NewRecorder()
	exportContext, _ := gin.CreateTestContext(exportRecorder)
	exportContext.Request = httptest.NewRequest(
		http.MethodGet,
		withAPIKeyFilters(exportPath, repeated...),
		nil,
	)
	const backupKey = "test-usage-backup-key"
	exportContext.Request.Header.Set(usageBackupKeyHeader, backupKey)
	h.ExportUsageStatistics(exportContext)
	if exportRecorder.Code != http.StatusOK {
		t.Fatalf(
			"ExportUsageStatistics(repeated API keys) status = %d, want %d body=%s",
			exportRecorder.Code,
			http.StatusOK,
			exportRecorder.Body.String(),
		)
	}
	if strings.Contains(exportRecorder.Body.String(), collidingRawA) ||
		strings.Contains(exportRecorder.Body.String(), collidingRawB) {
		t.Fatalf("ExportUsageStatistics returned raw API key: %s", exportRecorder.Body.String())
	}
	exported, err := usage.ParseUsageBackup(exportRecorder.Body.Bytes(), backupKey)
	if err != nil {
		t.Fatalf(
			"decode ExportUsageStatistics(repeated API keys): %v body=%s",
			err,
			exportRecorder.Body.String(),
		)
	}
	if len(exported) != 4 {
		t.Fatalf("ExportUsageStatistics(repeated API keys) events = %d, want 4", len(exported))
	}
}

func TestUsageSnapshotHandlersReturnDatabaseErrors(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	persister.Stop()

	h := NewHandler(
		&config.Config{},
		"",
		nil,
		WithEnvSecret(""),
		WithUsagePersister(persister),
	)
	tests := []struct {
		name      string
		path      string
		wantError string
		invoke    func(*Handler, *gin.Context)
	}{
		{
			name:      "statistics",
			path:      "/usage",
			wantError: "failed to query usage statistics",
			invoke:    (*Handler).GetUsageStatistics,
		},
		{
			name:      "export",
			path:      "/usage/export",
			wantError: "failed to query usage export",
			invoke:    (*Handler).ExportUsageStatistics,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, test.path, nil)
			test.invoke(h, c)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.wantError) {
				t.Fatalf("body = %s, want error %q", rec.Body.String(), test.wantError)
			}
		})
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
