package proxyutil

import "testing"

func TestValidRequestProxy(t *testing.T) {
	tests := []struct {
		proxy string
		valid bool
	}{
		{"http://127.0.0.1:8080", true},
		{" socks5h://user:pass@[::1]:1080 ", true},
		{"https://proxy.example", true},
		{"", false},
		{"direct", false},
		{"none", false},
		{"file:///tmp/proxy", false},
		{"http://", false},
		{"http://proxy.example:0", false},
		{"http://proxy.example:65536", false},
		{"http://proxy.example:invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.proxy, func(t *testing.T) {
			if got := ValidRequestProxy(tt.proxy); got != tt.valid {
				t.Fatalf("ValidRequestProxy(%q) = %t, want %t", tt.proxy, got, tt.valid)
			}
		})
	}
}
