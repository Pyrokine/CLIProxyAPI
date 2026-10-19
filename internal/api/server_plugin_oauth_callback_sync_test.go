package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPluginOAuthGETCallbackRouteUsesPendingStateWithoutManagementKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "callback-test-password")
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state=missing&code=ignored", nil)
	request.RemoteAddr = "203.0.113.20:12345"
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("uncredentialed callback status = %d headers = %v, body = %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}

	next, err := proxyconfig.Clone(server.cfg)
	if err != nil {
		t.Fatal(err)
	}
	next.TLS.RequireForAuth = true
	server.mgmt.SetConfig(next)
	secureRequest := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state=missing&code=ignored", nil)
	secureRequest.RemoteAddr = "203.0.113.21:12345"
	secureRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(secureRecorder, secureRequest)
	if secureRecorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("HTTP callback status = %d, want TLS requirement", secureRecorder.Code)
	}
	loopbackRequest := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state=missing&code=ignored", nil)
	loopbackRequest.RemoteAddr = "127.0.0.1:12345"
	loopbackRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(loopbackRecorder, loopbackRequest)
	if loopbackRecorder.Code != http.StatusNotFound {
		t.Fatalf("local callback status = %d, want unknown state", loopbackRecorder.Code)
	}
}
