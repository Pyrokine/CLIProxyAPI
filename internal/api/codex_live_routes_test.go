package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func TestCodexLiveRoutesRegistered(t *testing.T) {
	server := newTestServer(t)
	want := map[string]bool{
		http.MethodPost + " /v1/live":                                 false,
		http.MethodGet + " /v1/live/:call_id":                         false,
		http.MethodGet + " /v1/realtime":                              false,
		http.MethodPost + " /v1/realtime":                             false,
		http.MethodPost + " /v1/realtime/calls":                       false,
		http.MethodGet + " /v1/realtime/calls/:call_id":               false,
		http.MethodPost + " /v1/realtime/client_secrets":              false,
		http.MethodPost + " /v1/realtime/sessions":                    false,
		http.MethodPost + " /v1/realtime/transcription_sessions":      false,
		http.MethodGet + " /v1/realtime/translations":                 false,
		http.MethodPost + " /v1/realtime/translations":                false,
		http.MethodPost + " /v1/realtime/translations/client_secrets": false,
		http.MethodPost + " /v1/realtime/calls/:call_id/hangup":       false,
		http.MethodPost + " /v1/realtime/calls/:call_id/accept":       false,
		http.MethodPost + " /v1/realtime/calls/:call_id/reject":       false,
		http.MethodPost + " /v1/realtime/calls/:call_id/refer":        false,
	}
	for _, route := range server.engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, registered := range want {
		if !registered {
			t.Errorf("Codex Live route %s is not registered", route)
		}
	}
}

func TestCodexLiveRoutesRequirePublicAuthentication(t *testing.T) {
	server := newTestServer(t)
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/live"},
		{method: http.MethodGet, path: "/v1/live/call-1"},
		{method: http.MethodPost, path: "/v1/realtime/calls"},
		{method: http.MethodGet, path: "/v1/realtime/calls/call-1"},
		{method: http.MethodGet, path: "/v1/realtime?call_id=call-1"},
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, recorder.Code, http.StatusUnauthorized)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s Cache-Control = %q, want no-store", test.method, test.path, got)
		}
	}
}

func TestCodexLiveRoutesReachHandlersAfterAuthentication(t *testing.T) {
	server := newTestServer(t)
	for _, test := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{
			name:       "live bootstrap",
			method:     http.MethodPost,
			path:       "/v1/live",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "realtime calls bootstrap",
			method:     http.MethodPost,
			path:       "/v1/realtime/calls",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "live sideband without upgrade",
			method:     http.MethodGet,
			path:       "/v1/live/call-1",
			wantStatus: http.StatusUpgradeRequired,
		},
		{
			name:       "realtime calls sideband without upgrade",
			method:     http.MethodGet,
			path:       "/v1/realtime/calls/call-1",
			wantStatus: http.StatusUpgradeRequired,
		},
		{
			name:       "realtime query sideband without upgrade",
			method:     http.MethodGet,
			path:       "/v1/realtime?call_id=call-1",
			wantStatus: http.StatusUpgradeRequired,
		},
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != test.wantStatus {
			t.Errorf(
				"%s status = %d, want %d; body=%s", test.name, recorder.Code, test.wantStatus, recorder.Body.String(),
			)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", test.name, got)
		}
	}
}

func TestCodexLiveSidebandUpgradeReachesSessionLookup(t *testing.T) {
	server := newTestServer(t)
	for _, path := range []string{
		"/v1/live/call-missing",
		"/v1/realtime/calls/call-missing",
		"/v1/realtime?call_id=call-missing",
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNotFound {
			t.Errorf(
				"GET %s status = %d, want %d; body=%s", path, recorder.Code, http.StatusNotFound, recorder.Body.String(),
			)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestRealtimeRoutesUseStandardAndEphemeralAuthentication(t *testing.T) {
	server := newTestServer(t)

	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/realtime/client_secrets",
		strings.NewReader(`{"session":{"type":"realtime","model":"gpt-realtime"}}`),
	)
	server.engine.ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	var unauthorizedBody struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(unauthorized.Body.Bytes(), &unauthorizedBody); errUnmarshal != nil {
		t.Fatalf("unmarshal unauthorized response: %v", errUnmarshal)
	}
	if unauthorizedBody.Error.Type != "authentication_error" || unauthorizedBody.Error.Code != "invalid_api_key" {
		t.Fatalf("unauthorized error = %+v", unauthorizedBody.Error)
	}

	clientSecret := createRealtimeClientSecret(t, server)
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{
			name:   "ephemeral call",
			method: http.MethodPost,
			path:   "/v1/realtime/calls",
			body:   "v=0\r\n",
			want:   http.StatusServiceUnavailable,
		},
		{
			name:   "ephemeral direct websocket without upgrade",
			method: http.MethodGet,
			path:   "/v1/realtime?model=gpt-realtime",
			want:   http.StatusUpgradeRequired,
		},
		{
			name:   "ephemeral translation capability",
			method: http.MethodGet,
			path:   "/v1/realtime/translations",
			want:   http.StatusNotImplemented,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+clientSecret)
			if test.body != "" {
				request.Header.Set("Content-Type", "application/sdp")
			}
			server.engine.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}

	for _, path := range []string{
		"/v1/realtime/client_secrets",
		"/v1/realtime/sessions",
		"/v1/realtime/transcription_sessions",
		"/v1/realtime/translations/client_secrets",
		"/v1/realtime/calls/call-123/hangup",
		"/v1/realtime/calls/call-123/accept",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+clientSecret)
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("ephemeral key reached standard-only route %s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}

	invalid := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	invalidRequest.Header.Set("Authorization", "Bearer ek_invalid")
	invalidRequest.Header.Set("X-Api-Key", "test-key")
	server.engine.ServeHTTP(invalid, invalidRequest)
	if invalid.Code != http.StatusUnauthorized ||
		!strings.Contains(invalid.Body.String(), `"code":"invalid_realtime_client_secret"`) {
		t.Fatalf("invalid ephemeral key status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestRealtimeEphemeralAuthenticationUsesIssuerIdentity(t *testing.T) {
	server := newTestServer(t)
	clientSecret := createRealtimeClientSecret(t, server)

	var principal string
	var provider string
	var identity string
	handlers := append([]gin.HandlerFunc(nil), server.realtimeAuthMiddlewares(true)...)
	handlers = append(handlers, func(c *gin.Context) {
		principalValue, _ := c.Get("apiKey")
		providerValue, _ := c.Get("accessProvider")
		principal, _ = principalValue.(string)
		provider, _ = providerValue.(string)
		identity = coreusage.APIKeyIdentityFromContext(c.Request.Context())
		c.Status(http.StatusNoContent)
	})
	server.engine.GET("/test/realtime-ephemeral-identity", handlers...)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/test/realtime-ephemeral-identity", nil)
	request.Header.Set("Authorization", "Bearer "+clientSecret)
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if principal != "test-key" || provider != "config-inline" {
		t.Fatalf("issuer identity principal=%q provider=%q", principal, provider)
	}
	if want := coreusage.HashAPIKeyIdentity("test-key"); identity != want {
		t.Fatalf("API key identity = %q, want %q", identity, want)
	}
}

type emptyRealtimeAccessProvider struct{}

func (*emptyRealtimeAccessProvider) Identifier() string { return "empty-realtime" }

func (*emptyRealtimeAccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, nil
}

type opaqueRealtimeAccessProvider struct {
	credential string
}

func (*opaqueRealtimeAccessProvider) Identifier() string { return "opaque-realtime" }

func (p *opaqueRealtimeAccessProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if r.Header.Get("Authorization") != "Bearer "+p.credential {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: "opaque-tenant"}, nil
}

func TestRealtimeInvalidClientSecretCannotUseLoopbackFallback(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders([]sdkaccess.Provider{&emptyRealtimeAccessProvider{}})

	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	request.RemoteAddr = "127.0.0.1:10000"
	request.Header.Set("Authorization", "Bearer ek_invalid")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid client secret on loopback status=%d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeConfiguredKeyWithClientSecretPrefix(t *testing.T) {
	server := newTestServer(t)
	updated := *server.configSnapshot()
	updated.APIKeys = []string{"ek_validLongKey"}
	server.UpdateClients(&updated)

	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	request.Header.Set("Authorization", "Bearer ek_validLongKey")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("configured ek_ key status=%d, want 426; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeClientSecretOpaqueIssuerRevocation(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders([]sdkaccess.Provider{&opaqueRealtimeAccessProvider{credential: "ek_initial-credential"}})

	configuredRequest := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	configuredRequest.Header.Set("Authorization", "Bearer ek_initial-credential")
	configured := httptest.NewRecorder()
	server.engine.ServeHTTP(configured, configuredRequest)
	if configured.Code != http.StatusUpgradeRequired {
		t.Fatalf("opaque ek_ credential status=%d, want 426; body=%s", configured.Code, configured.Body.String())
	}

	issued := httptest.NewRecorder()
	issueRequest := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", strings.NewReader(`{"session":{"model":"gpt-realtime"}}`))
	issueRequest.Header.Set("Authorization", "Bearer ek_initial-credential")
	issueRequest.Header.Set("Content-Type", "application/json")
	server.engine.ServeHTTP(issued, issueRequest)
	if issued.Code != http.StatusOK {
		t.Fatalf("issue client secret status=%d body=%s", issued.Code, issued.Body.String())
	}
	var secret struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &secret); err != nil || secret.Value == "" {
		t.Fatalf("decode issued client secret: %v", err)
	}

	check := func(want int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
		request.Header.Set("Authorization", "Bearer "+secret.Value)
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("client secret status=%d, want %d; body=%s", recorder.Code, want, recorder.Body.String())
		}
	}
	check(http.StatusUpgradeRequired)
	server.accessManager.SetProviders([]sdkaccess.Provider{&opaqueRealtimeAccessProvider{credential: "replacement-credential"}})
	check(http.StatusUnauthorized)
}

func TestRealtimeClientSecretSurvivesUnrelatedConfigReload(t *testing.T) {
	server := newTestServer(t)
	clientSecret := createRealtimeClientSecret(t, server)
	updated := *server.configSnapshot()
	updated.Debug = !updated.Debug
	server.UpdateClients(&updated)

	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	request.Header.Set("Authorization", "Bearer "+clientSecret)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("unrelated configuration reload status=%d, want 426; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeClientSecretRevokedWhenIssuerKeyRemoved(t *testing.T) {
	server := newTestServer(t)
	clientSecret := createRealtimeClientSecret(t, server)
	updated := *server.configSnapshot()
	updated.APIKeys = nil
	server.UpdateClients(&updated)

	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	request.Header.Set("Authorization", "Bearer "+clientSecret)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("removed issuer status=%d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeClientSecretRevokedWhenAccessProvidersChange(t *testing.T) {
	server := newTestServer(t)
	clientSecret := createRealtimeClientSecret(t, server)

	requestRealtime := func() *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
		request.Header.Set("Authorization", "Bearer "+clientSecret)
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := requestRealtime(); recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("before revocation status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	server.accessManager.SetProviders(nil)
	if recorder := requestRealtime(); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("after revocation status=%d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeAPIKeyRuleBlocksNewAndExistingSessions(t *testing.T) {
	server := newTestServer(t)
	clientSecret := createRealtimeClientSecret(t, server)
	updated := *server.configSnapshot()
	updated.APIKeyRules = map[string]proxyconfig.APIKeyRule{
		"test-key": {BlockedModels: []string{"gpt-realtime"}},
	}
	server.UpdateClients(&updated)

	for _, auth := range []string{"test-key", clientSecret} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
		request.Header.Set("Authorization", "Bearer "+auth)
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden && !(auth == clientSecret && recorder.Code == http.StatusUnauthorized) {
			t.Errorf("restricted Realtime request status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", strings.NewReader(`{"session":{"type":"realtime","model":"gpt-realtime"}}`))
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("restricted client secret issuance status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeCallsRejectNullSession(t *testing.T) {
	server := newTestServer(t)
	for _, body := range []string{`null`, `{"session":null}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-key")
		request.Header.Set("Content-Type", "application/json")
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d, want 400; response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestRealtimeAPIKeyPolicyRestrictions(t *testing.T) {
	for _, test := range []struct {
		name string
		rule proxyconfig.APIKeyRule
		want int
	}{
		{name: "canonical model blocked", rule: proxyconfig.APIKeyRule{BlockedModels: []string{"gpt-live-1-codex"}}, want: http.StatusForbidden},
		{name: "canonical model not allowed", rule: proxyconfig.APIKeyRule{CanonicalModels: []string{"other-model"}}, want: http.StatusForbidden},
		{name: "provider not allowed", rule: proxyconfig.APIKeyRule{Providers: []string{"gemini"}}, want: http.StatusForbidden},
		{name: "usage limit cannot be enforced", rule: proxyconfig.APIKeyRule{Limits: []proxyconfig.APIKeyPolicyLimit{{Window: "1m", Requests: 1}}}, want: http.StatusForbidden},
		{name: "model mapping cannot be applied", rule: proxyconfig.APIKeyRule{ModelMappings: map[string]string{"gpt-realtime": "other-model"}}, want: http.StatusForbidden},
		{name: "permitted policy", rule: proxyconfig.APIKeyRule{Providers: []string{"codex"}, CanonicalModels: []string{"gpt-live-1-codex"}}, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			updated := *server.configSnapshot()
			updated.APIKeyRules = map[string]proxyconfig.APIKeyRule{"test-key": test.rule}
			server.UpdateClients(&updated)

			issued := httptest.NewRecorder()
			issueRequest := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", strings.NewReader(`{"session":{"model":"gpt-realtime"}}`))
			issueRequest.Header.Set("Authorization", "Bearer test-key")
			server.engine.ServeHTTP(issued, issueRequest)
			if issued.Code != test.want {
				t.Errorf("issuance status=%d, want %d; body=%s", issued.Code, test.want, issued.Body.String())
			}

			for _, request := range []*http.Request{
				httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil),
				httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader(`{"model":"gpt-realtime"}`)),
			} {
				request.Header.Set("Authorization", "Bearer test-key")
				if request.Method == http.MethodPost {
					request.Header.Set("Content-Type", "application/json")
				}
				recorder := httptest.NewRecorder()
				server.engine.ServeHTTP(recorder, request)
				if test.want == http.StatusForbidden && recorder.Code != http.StatusForbidden {
					t.Errorf("%s %s status=%d, want 403; body=%s", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
				}
				if test.want == http.StatusOK && recorder.Code == http.StatusForbidden {
					t.Errorf("%s %s rejected allowed policy: %s", request.Method, request.URL.Path, recorder.Body.String())
				}
			}
		})
	}
}

func TestRealtimeCORSExposesOnlyRealtimeResponseHeaders(t *testing.T) {
	server := newTestServer(t)
	for _, test := range []struct {
		path       string
		wantHeader bool
	}{
		{path: "/v1/realtime", wantHeader: true},
		{path: "/v1/models", wantHeader: false},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodOptions, test.path, nil)
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("OPTIONS %s status = %d, want %d", test.path, recorder.Code, http.StatusNoContent)
		}
		exposed := recorder.Header().Get("Access-Control-Expose-Headers")
		for _, header := range []string{"Location", "Retry-After", "X-Request-Id", "OpenAI-Request-Id"} {
			if got := strings.Contains(exposed, header); got != test.wantHeader {
				t.Errorf("OPTIONS %s exposed %s=%t, want %t: %s", test.path, header, got, test.wantHeader, exposed)
			}
		}
	}
}

func createRealtimeClientSecret(t *testing.T, server *Server) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/realtime/client_secrets",
		strings.NewReader(`{"session":{"type":"realtime","model":"gpt-realtime"}}`),
	)
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create client secret status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		Value string `json:"value"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("unmarshal client secret: %v", errUnmarshal)
	}
	if response.Value == "" {
		t.Fatal("client secret is empty")
	}
	return response.Value
}
