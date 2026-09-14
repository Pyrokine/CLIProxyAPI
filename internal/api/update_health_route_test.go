package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpdateHealthRouteIsRegisteredOutsideManagementAuthentication(t *testing.T) {
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/v0/management/update-health", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("update health route status = %d, want 401", response.Code)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("update health cache control = %q", response.Header().Get("Cache-Control"))
	}
}
