package api

import (
	"net/http"
	"testing"
)

func TestOAuthRequestScopedErrorRoutesRegistered(t *testing.T) {
	server := newTestServerWithOptions(t, WithLocalManagementPassword("test-management-password"))
	want := map[string]bool{
		http.MethodGet + " /v0/management/oauth-request-scoped-errors":    false,
		http.MethodPut + " /v0/management/oauth-request-scoped-errors":    false,
		http.MethodPatch + " /v0/management/oauth-request-scoped-errors":  false,
		http.MethodDelete + " /v0/management/oauth-request-scoped-errors": false,
	}
	for _, route := range server.engine.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := want[key]; exists {
			want[key] = true
		}
	}
	for route, registered := range want {
		if !registered {
			t.Errorf("OAuth request-scoped error route %s is not registered", route)
		}
	}
}
