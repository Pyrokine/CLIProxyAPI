package wsrelay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultMaxSessions(t *testing.T) {
	mgr := NewManager(Options{})
	if mgr.maxSessions != 256 {
		t.Fatalf("expected default maxSessions=256, got %d", mgr.maxSessions)
	}
}

func TestSessionLimit(t *testing.T) {
	mgr := NewManager(Options{MaxSessions: 1})
	mgr.sessMutex.Lock()
	mgr.sessions["a"] = &session{}
	mgr.sessMutex.Unlock()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+mgr.Path(), nil)
	mgr.handleWebsocket(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}
}

func TestCheckOrigin(t *testing.T) {
	mgr := NewManager(Options{})

	tests := []struct {
		name    string
		origin  string
		host    string
		allowed bool
	}{
		{
			name:    "no origin header (CLI client)",
			origin:  "",
			host:    "localhost:8317",
			allowed: true,
		},
		{
			name:    "localhost origin",
			origin:  "http://localhost:8317",
			host:    "localhost:8317",
			allowed: true,
		},
		{
			name:    "127.0.0.1 origin",
			origin:  "http://127.0.0.1:8317",
			host:    "127.0.0.1:8317",
			allowed: true,
		},
		{
			name:    "evil.com origin",
			origin:  "http://evil.com",
			host:    "localhost:8317",
			allowed: false,
		},
		{
			name:    "IPv6 loopback origin",
			origin:  "http://[::1]:8317",
			host:    "[::1]:8317",
			allowed: true,
		},
		{
			name:    "malformed origin",
			origin:  "://not-a-valid-url",
			host:    "localhost:8317",
			allowed: false,
		},
		{
			name:    "origin matches request host",
			origin:  "http://myhost.example.com:8317",
			host:    "myhost.example.com:8317",
			allowed: true,
		},
		{
			name:    "origin host mismatch",
			origin:  "http://attacker.com:8317",
			host:    "localhost:8317",
			allowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				req := &http.Request{
					Header: http.Header{},
					Host:   tt.host,
				}
				if tt.origin != "" {
					req.Header.Set("Origin", tt.origin)
				}

				got := mgr.upgrader.CheckOrigin(req)
				if got != tt.allowed {
					t.Fatalf(
						"CheckOrigin(origin=%q, host=%q) = %v, want %v",
						tt.origin, tt.host, got, tt.allowed,
					)
				}
			},
		)
	}
}
