package live

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/gin-gonic/gin"
)

func TestStaleClientSecretIssuancePreservesNewGenerationAcrossIssuers(t *testing.T) {
	handler := NewHandler(nil, nil)
	t.Cleanup(func() { _ = handler.Close() })
	var generation atomic.Uint64
	generation.Store(1)
	authenticated := make(chan struct{})
	router := gin.New()
	router.POST(
		"/client-secrets", func(c *gin.Context) {
			c.Set("apiKey", c.GetHeader("X-Test-Issuer"))
			c.Set("accessProvider", "synthetic-provider")
			c.Set(ClientSecretIssuerGenerationContextKey, generation.Load())
			if c.GetHeader("X-Test-Issuer") == "old-issuer" {
				close(authenticated)
			}
			c.Next()
		}, handler.CreateClientSecret,
	)
	reader, writer := io.Pipe()
	defer func(closeResource func() error) { _ = closeResource() }(reader.Close)
	defer func(closeResource func() error) { _ = closeResource() }(writer.Close)
	oldRequest := httptest.NewRequest(http.MethodPost, "/client-secrets", reader)
	oldRequest.Header.Set("X-Test-Issuer", "old-issuer")
	oldResponse := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { router.ServeHTTP(oldResponse, oldRequest); close(done) }()
	select {
	case <-authenticated:
	case <-time.After(time.Second):
		t.Fatal("old request was not authenticated")
	}
	generation.Store(2)
	newRequest := httptest.NewRequest(
		http.MethodPost, "/client-secrets", strings.NewReader(`{"session":{"model":"gpt-realtime"}}`),
	)
	newRequest.Header.Set("X-Test-Issuer", "new-issuer")
	newResponse := httptest.NewRecorder()
	router.ServeHTTP(newResponse, newRequest)
	if newResponse.Code != 200 {
		t.Fatalf("new generation status=%d body=%s", newResponse.Code, newResponse.Body.String())
	}
	var issued clientSecretCreateResponse
	if err := json.Unmarshal(newResponse.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(`{"session":{"model":"gpt-realtime"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old request did not complete")
	}
	if oldResponse.Code != http.StatusUnauthorized {
		t.Fatalf("stale generation status=%d, want401", oldResponse.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/calls", nil)
	request.Header.Set("Authorization", "Bearer "+issued.Value)
	authorization, matched, err := handler.AuthenticateClientSecret(request)
	if err != nil || !matched || authorization.IssuerGeneration != 2 || authorization.IssuerPrincipal != "new-issuer" {
		t.Fatalf(
			"new credential was removed: matched=%t generation=%d err=%v", matched, authorization.IssuerGeneration, err,
		)
	}
}

func TestClientSecretGenerationWatermarkSurvivesEntryExpiry(t *testing.T) {
	store := newClientSecretStore()
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	if _, _, _, err := store.create(json.RawMessage(`{}`), time.Second, "new", "provider", 2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, _, _, err := store.create(json.RawMessage(`{}`), time.Minute, "old", "provider", 1); err == nil {
		t.Fatal("an expired entry reset the accepted policy generation")
	}
	if _, _, _, err := store.create(json.RawMessage(`{}`), time.Minute, "standalone", "provider", 0); err != nil {
		t.Fatalf("standalone use without generation stopped working: %v", err)
	}
	if _, _, _, err := store.create(json.RawMessage(`{}`), time.Minute, "current", "provider", 2); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictedClientSecretBootstrapUsesItsFixedModel(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body string }{
		{"sdp", "application/sdp", "v=0\r\n"},
		{"plain", "text/plain", "v=0\r\n"},
		{"json", "application/json", `{"sdp":"v=0\r\n"}`},
		{"multipart", "multipart/form-data; boundary=restricted-model", multipartBody("restricted-model", "v=0", "")},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
				cfg.APIKeyRules = map[string]config.APIKeyRule{
					"restricted-issuer": {
						CanonicalModels: []string{"future-live-model"},
						Providers:       []string{"codex"},
					},
				}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				registry := executionregistry.New()
				dispatcher := &homeDispatcher{}
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				executor := &captureExecutor{responseBody: io.NopCloser(strings.NewReader("v=0\r\n"))}
				manager.RegisterExecutor(executor)
				handler := NewHandler(manager, func() *config.Config { return cfg })
				t.Cleanup(func() { _ = handler.Close(); _ = registry.Drain(context.Background()) })
				router := gin.New()
				router.POST(
					"/client-secrets", func(c *gin.Context) {
						c.Set("apiKey", "restricted-issuer")
						c.Set("accessProvider", "synthetic-provider")
						c.Set(ClientSecretIssuerGenerationContextKey, uint64(1))
						c.Next()
					}, handler.CreateClientSecret,
				)
				router.POST(
					"/calls", func(c *gin.Context) {
						authorization, matched, err := handler.AuthenticateClientSecret(c.Request)
						if err != nil || !matched {
							c.AbortWithStatus(401)
							return
						}
						c.Set("apiKey", authorization.IssuerPrincipal)
						c.Set("accessProvider", authorization.IssuerProvider)
						c.Set(ClientSecretSessionContextKey, authorization.Session)
						c.Set(ClientSecretPrincipalContextKey, authorization.Principal)
						c.Next()
					}, handler.Handle,
				)
				mint := httptest.NewRecorder()
				router.ServeHTTP(
					mint, httptest.NewRequest(
						http.MethodPost, "/client-secrets",
						strings.NewReader(`{"session":{"model":"future-live-model","instructions":"fixed"}}`),
					),
				)
				if mint.Code != 200 {
					t.Fatalf("custom model mint status=%d body=%s", mint.Code, mint.Body.String())
				}
				var issued clientSecretCreateResponse
				if err := json.Unmarshal(mint.Body.Bytes(), &issued); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/calls", strings.NewReader(tc.body))
				request.Header.Set("Content-Type", tc.contentType)
				request.Header.Set("Authorization", "Bearer "+issued.Value)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != 201 {
					t.Fatalf("fixed custom model bootstrap status=%d body=%s", response.Code, response.Body.String())
				}
				if dispatcher.model != "future-live-model" || executor.calls.Load() != 1 ||
					modelFromJSON(executor.body) != "future-live-model" {
					t.Fatalf(
						"dispatch model=%s calls=%d body=%s", dispatcher.model, executor.calls.Load(), executor.body,
					)
				}
				cfg.APIKeyRules["restricted-issuer"] = config.APIKeyRule{
					CanonicalModels: []string{"other-model"}, Providers: []string{"codex"},
				}
				request = httptest.NewRequest(http.MethodPost, "/calls", strings.NewReader(tc.body))
				request.Header.Set("Content-Type", tc.contentType)
				request.Header.Set("Authorization", "Bearer "+issued.Value)
				response = httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != 403 || executor.calls.Load() != 1 {
					t.Fatalf("revoked model reached upstream: status=%d calls=%d", response.Code, executor.calls.Load())
				}
			},
		)
	}
}
