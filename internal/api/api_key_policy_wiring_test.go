package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func TestAuthMiddlewareAttachesIrreversibleAPIKeyIdentity(t *testing.T) {
	apiKey := t.Name()
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{apiKey: {}}},
		},
	)
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(
		authMiddleware(
			manager, nil, func() *proxyconfig.Config {
				return &proxyconfig.Config{}
			},
		),
	)
	var identity string
	engine.GET(
		"/test", func(c *gin.Context) {
			identity = coreusage.APIKeyIdentityFromContext(c.Request.Context())
			c.Status(http.StatusNoContent)
		},
	)

	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.RemoteAddr = "10.0.0.1:9999"
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if want := coreusage.HashAPIKeyIdentity(apiKey); identity != want {
		t.Fatalf("API key identity = %q, want %q", identity, want)
	}
}
