package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/gin-gonic/gin"
)

func TestCreateClientSecretScopesTokenToIssuerAndSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil)
	router := gin.New()
	router.POST("/v1/realtime/client_secrets", func(c *gin.Context) {
		c.Set("apiKey", "issuer-key")
		c.Set("accessProvider", "config-inline")
		c.Next()
	}, handler.CreateClientSecret)

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/realtime/client_secrets",
		strings.NewReader(`{
			"session":{"type":"realtime","model":"gpt-realtime","instructions":"help"},
			"expires_after":{"anchor":"created_at","seconds":60}
		}`),
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var response struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
		Session   struct {
			ID           string `json:"id"`
			Object       string `json:"object"`
			Type         string `json:"type"`
			Model        string `json:"model"`
			Instructions string `json:"instructions"`
		} `json:"session"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if !strings.HasPrefix(response.Value, clientSecretPrefix) || response.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("client secret = %q expires_at=%d", response.Value, response.ExpiresAt)
	}
	if response.Session.ID == "" || response.Session.Object != "realtime.session" ||
		response.Session.Type != "realtime" || response.Session.Model != "gpt-realtime" ||
		response.Session.Instructions != "help" {
		t.Fatalf("session = %+v", response.Session)
	}

	authRequest := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	authRequest.Header.Set("Authorization", "Bearer "+response.Value)
	authorization, matched, errAuthenticate := handler.AuthenticateClientSecret(authRequest)
	if errAuthenticate != nil || !matched {
		t.Fatalf("AuthenticateClientSecret() matched=%t error=%v", matched, errAuthenticate)
	}
	if authorization.Principal != response.Session.ID || authorization.IssuerPrincipal != "issuer-key" ||
		authorization.IssuerProvider != "config-inline" {
		t.Fatalf("authorization = %+v", authorization)
	}
	if got := modelFromJSON(authorization.Session); got != defaultLiveModel {
		t.Fatalf("upstream model = %q, want %q", got, defaultLiveModel)
	}
}

func TestLiveSelectionMetadataUsesIssuerPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("apiKey", "issuer-key")
	c.Set(ClientSecretPrincipalContextKey, "sess_ephemeral")

	metadata := liveSelectionMetadata(c, map[string]any{
		coreexecutor.ExecutionSessionMetadataKey: "call_1",
	})
	if got, want := metadata[coreexecutor.CallerScopeMetadataKey], coresession.CallerScope("issuer-key"); got != want {
		t.Fatalf("caller scope = %v, want %q", got, want)
	}
	if got := metadata[coreexecutor.ExecutionSessionMetadataKey]; got != "call_1" {
		t.Fatalf("execution session = %v, want call_1", got)
	}
}

func TestLiveSelectionHeadersRemoveEphemeralAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	c.Request.Header.Set("Authorization", "Bearer ek_secret")
	c.Request.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	c.Request.Header.Set("X-Session-Id", "session-1")
	c.Set(ClientSecretPrincipalContextKey, "sess_ephemeral")

	headers := liveSelectionHeaders(c)
	if headers.Get("Authorization") != "" || headers.Get("Proxy-Authorization") != "" {
		t.Fatalf("ephemeral authorization headers remained: %v", headers)
	}
	if got := headers.Get("X-Session-Id"); got != "session-1" {
		t.Fatalf("X-Session-Id = %q, want session-1", got)
	}
	if got := c.Request.Header.Get("Authorization"); got != "Bearer ek_secret" {
		t.Fatalf("source Authorization changed to %q", got)
	}
}

func TestCreateClientSecretRequiresStandardPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil)
	for _, test := range []struct {
		name       string
		middleware gin.HandlerFunc
		want       int
	}{
		{name: "missing principal", want: http.StatusUnauthorized},
		{
			name: "ephemeral principal",
			middleware: func(c *gin.Context) {
				c.Set("apiKey", "issuer-key")
				c.Set("accessProvider", "config-inline")
				c.Set(ClientSecretPrincipalContextKey, "sess_1")
				c.Next()
			},
			want: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			if test.middleware == nil {
				router.POST("/v1/realtime/client_secrets", handler.CreateClientSecret)
			} else {
				router.POST("/v1/realtime/client_secrets", test.middleware, handler.CreateClientSecret)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(
				recorder,
				httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", strings.NewReader(`{}`)),
			)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func TestClientSecretAuthenticationNeverFallsBackForInvalidEK(t *testing.T) {
	handler := NewHandler(nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	request.Header.Set("Authorization", "Bearer ek_invalid")
	_, matched, errAuthenticate := handler.AuthenticateClientSecret(request)
	if !matched || !errors.Is(errAuthenticate, errInvalidClientSecret) {
		t.Fatalf("matched=%t error=%v", matched, errAuthenticate)
	}

	request.Header.Set("Authorization", "Bearer standard-key")
	_, matched, errAuthenticate = handler.AuthenticateClientSecret(request)
	if matched || errAuthenticate != nil {
		t.Fatalf("standard key matched=%t error=%v", matched, errAuthenticate)
	}
}

func TestClientSecretStoreExpiryAndIssuerCapacity(t *testing.T) {
	store := newClientSecretStore()
	now := time.Unix(1700000000, 0)
	store.now = func() time.Time { return now }
	token, _, _, errCreate := store.create(
		json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex"}`),
		time.Minute,
		"issuer",
		"provider",
		1,
	)
	if errCreate != nil {
		t.Fatalf("create() error = %v", errCreate)
	}
	now = now.Add(time.Minute)
	if _, errAuthenticate := store.authenticate(token); !errors.Is(errAuthenticate, errInvalidClientSecret) {
		t.Fatalf("expired token error = %v", errAuthenticate)
	}

	now = now.Add(-time.Minute)
	for index := 0; index < clientSecretMaxEntriesPerIssuer; index++ {
		store.entries[string(rune(index+1))] = clientSecretEntry{
			authorization: ClientSecretAuthorization{IssuerPrincipal: "issuer", IssuerProvider: "provider", IssuerGeneration: 1},
			expiresAt:     now.Add(time.Hour),
		}
	}
	_, _, _, errCreate = store.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 1)
	if !errors.Is(errCreate, errClientSecretCapacity) {
		t.Fatalf("issuer capacity error = %v", errCreate)
	}
	_, _, _, errCreate = store.create(json.RawMessage(`{}`), time.Minute, "other", "provider", 1)
	if errCreate != nil {
		t.Fatalf("isolated issuer create error = %v", errCreate)
	}

	globalStore := newClientSecretStore()
	globalStore.now = func() time.Time { return now }
	for index := range clientSecretMaxEntries {
		key := string(rune(index + 1))
		globalStore.entries[key] = clientSecretEntry{
			authorization: ClientSecretAuthorization{IssuerPrincipal: key, IssuerProvider: "provider", IssuerGeneration: 1},
			expiresAt:     now.Add(time.Hour),
		}
	}
	_, _, _, errCreate = globalStore.create(json.RawMessage(`{}`), time.Minute, "extra", "provider", 1)
	if !errors.Is(errCreate, errClientSecretCapacity) {
		t.Fatalf("global capacity error = %v", errCreate)
	}
}

func TestClientSecretLifetimeBounds(t *testing.T) {
	expiresAfter := func(anchor string, seconds int64) *clientSecretExpiresAfter {
		return &clientSecretExpiresAfter{Anchor: anchor, Seconds: seconds}
	}
	for _, test := range []struct {
		name    string
		request *clientSecretExpiresAfter
		wantErr bool
	}{
		{name: "default", request: nil},
		{name: "minimum", request: expiresAfter("created_at", int64(clientSecretMinimumLifetime/time.Second))},
		{name: "maximum", request: expiresAfter("created_at", int64(clientSecretMaximumLifetime/time.Second))},
		{
			name:    "below minimum",
			request: expiresAfter("created_at", int64(clientSecretMinimumLifetime/time.Second)-1),
			wantErr: true,
		},
		{
			name:    "above maximum",
			request: expiresAfter("created_at", int64(clientSecretMaximumLifetime/time.Second)+1),
			wantErr: true,
		},
		{name: "unsupported anchor", request: expiresAfter("last_use", 60), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, errLifetime := clientSecretLifetime(test.request)
			if (errLifetime != nil) != test.wantErr {
				t.Fatalf("clientSecretLifetime() error = %v, wantErr=%t", errLifetime, test.wantErr)
			}
		})
	}
}

func TestCreateClientSecretRejectsUnsupportedSessionType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil)
	router := gin.New()
	router.POST("/v1/realtime/client_secrets", func(c *gin.Context) {
		c.Set("apiKey", "issuer-key")
		c.Set("accessProvider", "config-inline")
		c.Next()
	}, handler.CreateClientSecret)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/realtime/client_secrets",
			strings.NewReader(`{"session":{"type":"transcription","model":"gpt-4o-transcribe"}}`),
		),
	)
	if recorder.Code != http.StatusNotImplemented ||
		!strings.Contains(recorder.Body.String(), "realtime_capability_not_supported") {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerAppliesClientSecretSessionAndKeepsIssuerOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := auth.NewManager(nil, nil, nil)
	executor := &captureExecutor{responseBody: io.NopCloser(strings.NewReader("v=0\r\n"))}
	manager.RegisterExecutor(executor)
	registerCredential(t, manager, &auth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token"},
	})
	handler := NewHandler(manager, nil)
	router := gin.New()
	router.POST("/v1/realtime/calls", func(c *gin.Context) {
		c.Set("apiKey", "issuer-key")
		c.Set("accessProvider", "config-inline")
		c.Set(ClientSecretPrincipalContextKey, "sess_secret")
		c.Set(
			ClientSecretSessionContextKey,
			json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex","instructions":"help"}`),
		)
		c.Next()
	}, handler.Handle)
	request := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader("v=0\r\n"))
	request.Header.Set("Authorization", "Bearer ek_secret")
	request.Header.Set("Content-Type", "application/sdp")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if got := recorder.Header().Get("Location"); got != "/v1/realtime/calls/call-123" {
		t.Fatalf("Location = %q", got)
	}
	var upstream struct {
		SDP     string `json:"sdp"`
		Session struct {
			Model        string `json:"model"`
			Instructions string `json:"instructions"`
		} `json:"session"`
	}
	if errUnmarshal := json.Unmarshal(executor.body, &upstream); errUnmarshal != nil {
		t.Fatalf("unmarshal upstream body: %v; body=%s", errUnmarshal, executor.body)
	}
	if upstream.SDP != "v=0\r\n" || upstream.Session.Model != defaultLiveModel ||
		upstream.Session.Instructions != "help" {
		t.Fatalf("upstream = %+v", upstream)
	}
	if got := executor.request.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("upstream Authorization = %q", got)
	}
	stored, ok := handler.sessions.peek("call-123")
	if !ok || stored.ownerPrincipal != "issuer-key" || stored.ownerProvider != "config-inline" ||
		stored.clientSecretPrincipal != "sess_secret" {
		t.Fatalf("stored session = %+v, ok=%t", stored, ok)
	}
}

func TestSidebandRejectsMismatchedClientSecretBeforeSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	handler.sessions.put("call-123", liveSession{
		authID:                "codex-oauth",
		model:                 defaultLiveModel,
		clientSecretPrincipal: "sess_expected",
	})
	router := gin.New()
	router.GET("/v1/realtime/calls/:call_id", func(c *gin.Context) {
		c.Set(ClientSecretPrincipalContextKey, "sess_other")
		c.Next()
	}, handler.HandleSideband)
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime/calls/call-123", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestApplyClientSecretCallSession(t *testing.T) {
	session := json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex","instructions":"help"}`)
	body, contentType, model, errApply := applyClientSecretCallSession(
		[]byte("v=0\r\n"),
		"application/sdp",
		defaultLiveModel,
		session,
	)
	if errApply != nil {
		t.Fatalf("applyClientSecretCallSession() error = %v", errApply)
	}
	if contentType != "application/json" || model != defaultLiveModel {
		t.Fatalf("contentType=%q model=%q", contentType, model)
	}
	if !strings.Contains(string(body), `"instructions":"help"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestApplyClientSecretCallSessionRejectsNullRequest(t *testing.T) {
	_, _, _, err := applyClientSecretCallSession(
		[]byte(`null`), "application/json", defaultLiveModel,
		json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex"}`),
	)
	if err == nil {
		t.Fatal("applyClientSecretCallSession() accepted a null request")
	}
}

func TestCreateLegacySessionUsesSameIssuerScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil)
	router := gin.New()
	router.POST("/v1/realtime/sessions", func(c *gin.Context) {
		c.Set("apiKey", "issuer-key")
		c.Set("accessProvider", "config-inline")
		c.Next()
	}, handler.CreateLegacySession)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/realtime/sessions",
			strings.NewReader(`{"type":"realtime","model":"gpt-realtime"}`),
		),
	)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"client_secret"`) {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestClientSecretStoreReclaimsRevokedGeneration(t *testing.T) {
	store := newClientSecretStore()
	stale, _, _, err := store.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 1)
	if err != nil {
		t.Fatalf("create stale client secret: %v", err)
	}
	for index := 1; index < clientSecretMaxEntries; index++ {
		store.entries[fmt.Sprintf("stale-%d", index)] = clientSecretEntry{
			authorization: ClientSecretAuthorization{IssuerPrincipal: "other", IssuerGeneration: 1},
			expiresAt:     time.Now().Add(time.Minute),
		}
	}
	if _, _, _, err = store.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 2); err != nil {
		t.Fatalf("create client secret after provider rotation: %v", err)
	}
	if _, err = store.authenticate(stale); !errors.Is(err, errInvalidClientSecret) {
		t.Fatalf("stale client secret remains in store: %v", err)
	}
}

func TestClientSecretStoreConcurrentAuthentication(t *testing.T) {
	store := newClientSecretStore()
	token, _, _, errCreate := store.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 1)
	if errCreate != nil {
		t.Fatalf("create() error = %v", errCreate)
	}
	done := make(chan error, 16)
	for range 16 {
		go func() {
			_, errAuthenticate := store.authenticate(token)
			done <- errAuthenticate
		}()
	}
	for range 16 {
		if errAuthenticate := <-done; errAuthenticate != nil {
			t.Fatalf("authenticate() error = %v", errAuthenticate)
		}
	}
}

func TestClientSecretCloseInvalidatesToken(t *testing.T) {
	store := newClientSecretStore()
	token, _, _, errCreate := store.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 1)
	if errCreate != nil {
		t.Fatalf("create() error = %v", errCreate)
	}
	store.close()
	if _, errAuthenticate := store.authenticate(token); !errors.Is(errAuthenticate, errInvalidClientSecret) {
		t.Fatalf("authenticate after close error = %v", errAuthenticate)
	}
}

func TestHandlerCloseInvalidatesClientSecrets(t *testing.T) {
	handler := NewHandler(nil, nil)
	token, _, _, errCreate := handler.clientSecrets.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 1)
	if errCreate != nil {
		t.Fatalf("create() error = %v", errCreate)
	}
	if errClose := handler.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	_, matched, errAuthenticate := handler.AuthenticateClientSecret(request)
	if !matched || !errors.Is(errAuthenticate, errInvalidClientSecret) {
		t.Fatalf("matched=%t error=%v", matched, errAuthenticate)
	}
}

func TestClientSecretCanBeUsedAcrossRequestContexts(t *testing.T) {
	store := newClientSecretStore()
	token, _, _, errCreate := store.create(json.RawMessage(`{}`), time.Minute, "issuer", "provider", 1)
	if errCreate != nil {
		t.Fatalf("create() error = %v", errCreate)
	}
	request, errRequest := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/realtime", nil)
	if errRequest != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errRequest)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if _, errAuthenticate := store.authenticate(bearerToken(request)); errAuthenticate != nil {
		t.Fatalf("authenticate() error = %v", errAuthenticate)
	}
}
