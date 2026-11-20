package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
)

func TestAuthTLSRejectionIsNeverStored(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "fixture-management-password")
	server := newTestServer(t)
	updated := *server.configSnapshot()
	updated.TLS.RequireForAuth = true
	server.UpdateClients(&updated)
	server.mgmt.SetConfig(&updated)
	server.AttachWebsocketRoute(
		"/v1/fixture-ws", http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
		),
	)
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1internal:generateContent"},
		{http.MethodGet, "/v1/realtime"},
		{http.MethodGet, "/v1/live/fixture-call"},
		{http.MethodGet, "/v1/fixture-ws"},
		{http.MethodGet, "/v0/management/config"},
		{http.MethodGet, "/v0/management/oauth-callback"},
	} {
		t.Run(
			test.path, func(t *testing.T) {
				request := httptest.NewRequest(test.method, test.path, nil)
				request.RemoteAddr = "203.0.113.20:12345"
				request.Header.Set("Authorization", "Bearer fixture-management-password")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				if response.Code != http.StatusMisdirectedRequest ||
					response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf(
						"TLS rejection: status=%d cache=%q body=%s", response.Code,
						response.Header().Get("Cache-Control"), response.Body,
					)
				}
			},
		)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("loopback control: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestAuthRejectionIsNeverStored(t *testing.T) {
	server := newTestServer(t)
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1internal:generateContent"},
		{http.MethodGet, "/v1/realtime"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf(
				"%s authentication rejection: status=%d cache=%q body=%s", test.path, response.Code,
				response.Header().Get("Cache-Control"), response.Body,
			)
		}
	}
}

func TestManagementBuildHeadersRequireSuccessfulAuthentication(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "fixture-management-password")
	version, commit, date := buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate
	buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate = "fixture-release", "fixture-commit", "fixture-date"
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate = version, commit, date })
	server := newTestServer(t)
	request := func(method, path, key string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, req)
		return response
	}
	authorized := request(http.MethodGet, "/v0/management/config", "fixture-management-password")
	if authorized.Code != http.StatusOK || authorized.Header().Get("X-Cpa-Version") != "fixture-release" ||
		authorized.Header().Get("X-Cpa-Commit") != "fixture-commit" ||
		authorized.Header().Get("X-Cpa-Build-Date") != "fixture-date" {
		t.Fatalf("authenticated build metadata: status=%d headers=%v", authorized.Code, authorized.Header())
	}
	checkHidden := func(response *httptest.ResponseRecorder) {
		t.Helper()
		for _, header := range []string{"X-Cpa-Version", "X-Cpa-Commit", "X-Cpa-Build-Date"} {
			if response.Header().Get(header) != "" {
				t.Errorf("response %d disclosed %s", response.Code, header)
			}
		}
	}
	for _, test := range []struct{ method, path, key string }{
		{http.MethodGet, "/v0/management/config", ""},
		{http.MethodGet, "/healthz", ""},
		{http.MethodGet, "/v1/models", "test-key"},
		{http.MethodOptions, "/v0/management/config", ""},
	} {
		checkHidden(request(test.method, test.path, test.key))
	}
	limited := false
	for attempt := range 12 {
		response := request(http.MethodGet, "/v0/management/config", fmt.Sprintf("fixture-invalid-%d", attempt))
		checkHidden(response)
		if response.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("authentication rejection status=%d body=%s", response.Code, response.Body)
		}
	}
	if !limited {
		t.Fatal("management rate-limit branch was not exercised")
	}
}
