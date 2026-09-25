package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPluginQuotaRoutesUseManagementPolicyAndNoStore(t *testing.T) {
	const password = "test-management-password"
	server := newTestServerWithOptions(t, WithLocalManagementPassword(password))

	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodGet, "/v0/management/quota/providers", nil)
	unauthorizedRequest.RemoteAddr = "127.0.0.1:12345"
	server.engine.ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d body=%s", unauthorized.Code, http.StatusUnauthorized, unauthorized.Body.String())
	}
	if cacheControl := unauthorized.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("unauthorized Cache-Control = %q, want no-store", cacheControl)
	}

	authorized := httptest.NewRecorder()
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/v0/management/quota/providers", nil)
	authorizedRequest.RemoteAddr = "127.0.0.1:12345"
	authorizedRequest.Header.Set("X-Management-Key", password)
	server.engine.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d body=%s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
	if body := authorized.Body.String(); !strings.Contains(body, `"providers":[]`) {
		t.Fatalf("authorized body = %s, want empty provider list", body)
	}
	if cacheControl := authorized.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("authorized Cache-Control = %q, want no-store", cacheControl)
	}

	for _, target := range []string{
		"/v0/management/quota/fetch",
		"/v0/management/quota/reset",
		"/v0/management/plugins/example/quota",
		"/v0/management/plugins/example/quota/reset",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("X-Management-Key", password)
		request.Header.Set("Content-Type", "application/json")
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound {
			t.Fatalf("%s was not registered: body=%s", target, recorder.Body.String())
		}
		if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", target, cacheControl)
		}
	}
}
