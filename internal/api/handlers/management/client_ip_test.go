package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestResolveManagementClientAddressIgnoresUntrustedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	req.Header.Set("X-Real-IP", "198.51.100.11")

	got := resolveManagementClientAddress(req, &config.Config{})
	if got.ip != "127.0.0.1" || !got.local {
		t.Fatalf("resolveManagementClientAddress = %+v, want loopback peer", got)
	}
}

func TestResolveManagementClientAddressUsesRightmostUntrustedHop(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32", "10.0.0.0/8"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.10, 10.1.2.3")

	got := resolveManagementClientAddress(req, cfg)
	if got.ip != "198.51.100.10" || got.local {
		t.Fatalf("resolveManagementClientAddress = %+v, want external client", got)
	}
}

func TestResolveManagementClientAddressRejectsMalformedTrustedHeaderAsLocal(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Real-IP", "not-an-ip")

	got := resolveManagementClientAddress(req, cfg)
	if !strings.HasPrefix(got.ip, "invalid-forwarded:") || got.local {
		t.Fatalf("resolveManagementClientAddress = %+v, want isolated remote classification", got)
	}
}

func TestMiddlewareTrustedProxyRateLimitIsolatesClients(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	h := NewHandler(cfg, "", nil, WithEnvSecret("correct"))
	t.Cleanup(func() { _ = h.Close() })

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < managementMaxFailures; i++ {
		rec := httptest.NewRecorder()
		req := proxiedManagementRequest("198.51.100.10", "wrong")
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}

	blocked := httptest.NewRecorder()
	engine.ServeHTTP(blocked, proxiedManagementRequest("198.51.100.10", "correct"))
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked client status = %d, want 429", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("blocked client response is missing Retry-After")
	}

	other := httptest.NewRecorder()
	engine.ServeHTTP(other, proxiedManagementRequest("198.51.100.11", "correct"))
	if other.Code != http.StatusOK {
		t.Fatalf("other client status = %d, want 200: %s", other.Code, other.Body.String())
	}
}

func TestMiddlewareMalformedTrustedHeaderCannotUseLocalPassword(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))
	h.localPassword = "local-pass"
	t.Cleanup(func() { _ = h.Close() })

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Real-IP", "not-an-ip")
	req.Header.Set("X-Management-Key", "local-pass")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareMalformedTrustedHeaderDoesNotBanDirectLocalClient(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))
	h.localPassword = "local-pass"
	t.Cleanup(func() { _ = h.Close() })

	engine := gin.New()
	engine.Use(h.Middleware())
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < managementMaxFailures; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Real-IP", "not-an-ip")
		req.Header.Set("X-Management-Key", "wrong")
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("malformed attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}

	blocked := httptest.NewRecorder()
	blockedRequest := httptest.NewRequest(http.MethodGet, "/test", nil)
	blockedRequest.RemoteAddr = "127.0.0.1:12345"
	blockedRequest.Header.Set("X-Real-IP", "not-an-ip")
	blockedRequest.Header.Set("X-Management-Key", "local-pass")
	engine.ServeHTTP(blocked, blockedRequest)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("malformed client status = %d, want 429", blocked.Code)
	}

	local := httptest.NewRecorder()
	localRequest := httptest.NewRequest(http.MethodGet, "/test", nil)
	localRequest.RemoteAddr = "127.0.0.1:12345"
	localRequest.Header.Set("X-Management-Key", "local-pass")
	engine.ServeHTTP(local, localRequest)
	if local.Code != http.StatusOK {
		t.Fatalf("direct local client status = %d, want 200: %s", local.Code, local.Body.String())
	}
}

func TestGetConfigIncludesTrustedProxies(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	h := NewHandler(cfg, "", nil, WithEnvSecret(""))
	t.Cleanup(func() { _ = h.Close() })

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config", nil)
	h.GetConfig(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetConfig status = %d, want 200", rec.Code)
	}
	var body struct {
		RemoteManagement struct {
			TrustedProxies []string `json:"trusted-proxies"`
		} `json:"remote-management"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal config response: %v", err)
	}
	if len(body.RemoteManagement.TrustedProxies) != 2 || body.RemoteManagement.TrustedProxies[0] != "127.0.0.1/32" {
		t.Fatalf("trusted proxies = %v", body.RemoteManagement.TrustedProxies)
	}
}

func TestPutConfigYAMLRejectsInvalidTrustedProxy(t *testing.T) {
	h := newConfigBasicTestHandler(t, &config.Config{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/config.yaml",
		bytes.NewBufferString("remote-management:\n  trusted-proxies:\n    - not-an-ip\n"),
	)

	h.PutConfigYAML(c)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PutConfigYAML status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

func proxiedManagementRequest(clientIP, key string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Management-Key", key)
	return req
}
