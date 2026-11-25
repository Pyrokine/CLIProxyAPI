package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/gin-gonic/gin"
)

func TestAuthMiddlewareAuthenticatesBeforeCredentialCooldown(t *testing.T) {
	const key = "test-authorized-key"
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{key: {}}}})
	limiter := access.NewAuthRateLimiter()
	t.Cleanup(limiter.Stop)
	hash := access.HashAccountKey(key)
	for range 16 {
		limiter.RecordAccountFailure(hash)
	}
	if !limiter.IsAccountLimited(hash) {
		t.Fatal("expected active credential cooldown")
	}

	router := gin.New()
	router.Use(authMiddleware(manager, limiter, func() *config.Config { return &config.Config{} }))
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("verified key returned status %d", response.Code)
	}
	if limiter.IsAccountLimited(hash) {
		t.Fatal("verified identity retained failure state")
	}
}
