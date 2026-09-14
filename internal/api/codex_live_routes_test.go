package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexLiveRoutesRegistered(t *testing.T) {
	server := newTestServer(t)
	want := map[string]bool{
		http.MethodPost + " /v1/live":                   false,
		http.MethodGet + " /v1/live/:call_id":           false,
		http.MethodPost + " /v1/realtime/calls":         false,
		http.MethodGet + " /v1/realtime/calls/:call_id": false,
		http.MethodGet + " /v1/realtime":                false,
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
