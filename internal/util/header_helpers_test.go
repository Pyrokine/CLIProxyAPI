package util

import (
	"net/http"
	"testing"
)

func TestApplyCustomHeadersFromAttrsIgnoresHost(t *testing.T) {
	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com/v1/messages", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	ApplyCustomHeadersFromAttrs(
		req, map[string]string{
			"header:Host":           "api.example.test",
			"header:X-Custom-Value": "custom",
		},
	)

	if req.Host != "example.com" {
		t.Fatalf("req.Host = %q, want example.com", req.Host)
	}
	if got := req.Header.Get("Host"); got != "" {
		t.Fatalf("Header Host = %q, want empty", got)
	}
	if got := req.Header.Get("X-Custom-Value"); got != "custom" {
		t.Fatalf("X-Custom-Value = %q, want custom", got)
	}
}
