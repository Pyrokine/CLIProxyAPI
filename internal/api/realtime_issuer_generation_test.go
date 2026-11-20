package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	codexlive "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/live"
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/gin-gonic/gin"
)

func TestRealtimeAuthenticationGenerationProtectsNewIssuerFromLateCreation(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"old-issuer": {}}}})
	oldGeneration := manager.Generation()
	handler := codexlive.NewHandler(nil, nil)
	t.Cleanup(func() { _ = handler.Close() })
	authenticated := make(chan struct{})
	router := gin.New()
	router.POST(
		"/v1/realtime/client_secrets", realtimeStandardAuthMiddleware(
			manager, nil, func() *proxyconfig.Config {
				return &proxyconfig.Config{}
			},
		), func(c *gin.Context) {
			if principal, _ := c.Get("apiKey"); principal == "old-issuer" {
				if captured, _ := c.Get(codexlive.ClientSecretIssuerGenerationContextKey); captured != oldGeneration {
					t.Error("actual authentication middleware did not capture the issuer generation")
				}
				close(authenticated)
			}
			c.Next()
		}, handler.CreateClientSecret,
	)
	reader, writer := io.Pipe()
	defer func(closeResource func() error) { _ = closeResource() }(reader.Close)
	defer func(closeResource func() error) { _ = closeResource() }(writer.Close)
	oldRequest := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", reader)
	oldRequest.Header.Set("Authorization", "Bearer old-issuer")
	oldRequest.Header.Set("Content-Type", "application/json")
	oldResponse := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(oldResponse, oldRequest)
		close(done)
	}()
	select {
	case <-authenticated:
	case <-time.After(3 * time.Second):
		t.Fatal("old issuer did not pass actual generation-aware authentication")
	}
	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"new-issuer": {}}}})
	if manager.Generation() <= oldGeneration {
		t.Fatal("provider publication did not advance the access generation")
	}
	freshRequest := httptest.NewRequest(
		http.MethodPost, "/v1/realtime/client_secrets", strings.NewReader(`{"session":{"model":"gpt-realtime"}}`),
	)
	freshRequest.Header.Set("Authorization", "Bearer new-issuer")
	freshRequest.Header.Set("Content-Type", "application/json")
	freshResponse := httptest.NewRecorder()
	router.ServeHTTP(freshResponse, freshRequest)
	var issued struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(
		freshResponse.Body.Bytes(), &issued,
	); err != nil || freshResponse.Code != http.StatusOK || issued.Value == "" {
		t.Fatalf("new issuer creation status=%d error=%v", freshResponse.Code, err)
	}
	if _, err := writer.Write([]byte(`{"session":{"model":"gpt-realtime"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("late old-generation request did not finish")
	}
	if oldResponse.Code != http.StatusUnauthorized {
		t.Fatalf("late old-generation creation status=%d", oldResponse.Code)
	}
	use := httptest.NewRequest(http.MethodPost, "/v1/realtime", nil)
	use.Header.Set("Authorization", "Bearer "+issued.Value)
	authorization, matched, err := handler.AuthenticateClientSecret(use)
	if err != nil || !matched || authorization.IssuerGeneration != manager.Generation() ||
		authorization.IssuerPrincipal != "new-issuer" ||
		authorization.IssuerProvider != "stub" {
		t.Fatalf(
			"new issuer credential changed: matched=%t generation=%d error=%v", matched, authorization.IssuerGeneration,
			err,
		)
	}
}
