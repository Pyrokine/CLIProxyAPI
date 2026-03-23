package wsrelay

import (
	"net/http"
	"testing"
)

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
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{
				Header: http.Header{},
				Host:   tt.host,
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			got := mgr.upgrader.CheckOrigin(req)
			if got != tt.allowed {
				t.Fatalf("CheckOrigin(origin=%q, host=%q) = %v, want %v",
					tt.origin, tt.host, got, tt.allowed)
			}
		})
	}
}
