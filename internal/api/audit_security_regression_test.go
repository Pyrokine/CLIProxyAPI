package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestAuditDirectLoopbackHasSeparateVideoIdentity(t *testing.T) {
	engine := gin.New()
	engine.Use(authMiddleware(sdkaccess.NewManager(), nil, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	var identity string
	engine.GET(
		"/test", func(c *gin.Context) {
			identity = coreusage.APIKeyIdentityFromContext(c.Request.Context())
			c.Status(http.StatusNoContent)
		},
	)
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || identity != handlers.DirectLoopbackIdentity {
		t.Fatalf("status=%d identity=%q", response.Code, identity)
	}
	request = httptest.NewRequest(http.MethodGet, "/test", nil)
	request.RemoteAddr = "203.0.113.1:12345"
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("remote keyless request status=%d", response.Code)
	}
}

func TestAuditCodexAlphaSearchEnforcesConcurrentQuota(t *testing.T) {
	server := newTestServer(t)
	cfg, err := proxyconfig.Clone(server.configSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKeyRules = map[string]proxyconfig.APIKeyRule{
		"test-key": {
			Limits: []proxyconfig.APIKeyPolicyLimit{
				{
					Window: "1h", Requests: 1,
				},
			},
		},
	}
	server.UpdateClients(cfg)
	persister := usage.NewPersister(t.TempDir(), proxyconfig.UsageRetention{})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("test quota storage is unavailable")
	}
	server.handlers.SetAPIKeyPolicyStore(persister)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	registerC02Credential(
		t, server, &auth.Auth{
			ID: "quota-codex", Provider: "codex", Status: auth.StatusActive,
			Metadata: map[string]any{"access_token": "synthetic-token"},
		}, "quota-model",
	)
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for index := range 8 {
		wg.Go(
			func() {
				path := []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"}[index%2]
				req := httptest.NewRequest(
					http.MethodPost, path, strings.NewReader(`{"model":"quota-model","query":"test"}`),
				)
				req.Header.Set("Authorization", "Bearer test-key")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, req)
				statuses <- response.Code
			},
		)
	}
	wg.Wait()
	close(statuses)
	succeeded := 0
	for status := range statuses {
		if status == http.StatusOK {
			succeeded++
		} else if status != http.StatusTooManyRequests {
			t.Errorf("status=%d, want 200 or 429", status)
		}
	}
	if succeeded != 1 || len(executor.selectedAuthIDs()) != 1 {
		t.Fatalf("successful=%d upstream calls=%d", succeeded, len(executor.selectedAuthIDs()))
	}
}

func TestAuditSuccessfulKeyDoesNotClearOtherCredentialFailures(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"valid-key": {}}}})
	limiter := access.NewAuthRateLimiter()
	t.Cleanup(limiter.Stop)
	engine := gin.New()
	engine.Use(authMiddleware(manager, limiter, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		req.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		return response.Code
	}
	for i := range 10 {
		if status := request(fmt.Sprintf("wrong-key-%d", i)); status != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want 401", i, status)
		}
		if i < 9 {
			if status := request("valid-key"); status != http.StatusNoContent {
				t.Fatalf("success %d status = %d, want 204", i, status)
			}
		}
	}
	if status := request("valid-key"); status != http.StatusTooManyRequests {
		t.Fatalf("status after ten IP failures = %d, want 429", status)
	}
}

func TestAuditCodexAlphaSearchEnforcesBlockedModels(t *testing.T) {
	for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		t.Run(
			path, func(t *testing.T) {
				server := newTestServer(t)
				cfg, err := proxyconfig.Clone(server.configSnapshot())
				if err != nil {
					t.Fatal(err)
				}
				cfg.APIKeyRules = map[string]proxyconfig.APIKeyRule{
					"test-key": {BlockedModels: []string{"restricted-model"}},
				}
				server.UpdateClients(cfg)
				executor := &c02SearchExecutor{}
				server.handlers.AuthManager.RegisterExecutor(executor)
				registerC02Credential(
					t, server, &auth.Auth{
						ID: "restricted-codex", Provider: "codex", Status: auth.StatusActive,
						Metadata: map[string]any{"access_token": "synthetic-token"},
					}, "restricted-model",
				)
				req := httptest.NewRequest(
					http.MethodPost, path, strings.NewReader(`{"model":"restricted-model","query":"test"}`),
				)
				req.Header.Set("Authorization", "Bearer test-key")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, req)
				if response.Code != http.StatusForbidden || !strings.Contains(
					response.Body.String(), "api_key_policy_forbidden",
				) {
					t.Fatalf("status=%d body=%s, want policy rejection", response.Code, response.Body.String())
				}
				if request, _ := executor.snapshot(); request != nil {
					t.Fatal("blocked request reached upstream")
				}
			},
		)
	}
}
