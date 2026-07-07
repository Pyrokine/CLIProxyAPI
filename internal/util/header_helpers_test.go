package util

import (
	"net/http"
	"testing"
)

func TestApplyCustomHeadersFromAttrsIgnoresRestrictedHeaders(t *testing.T) {
	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com/v1/messages", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}
	req.Header.Set("Authorization", "Bearer built-in")

	ApplyCustomHeadersFromAttrs(
		req, map[string]string{
			"header:Authorization":  "Bearer custom",
			"header:Cookie":         "session=custom",
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
	if got := req.Header.Get("Authorization"); got != "Bearer built-in" {
		t.Fatalf("Authorization = %q, want built-in value", got)
	}
	if got := req.Header.Get("Cookie"); got != "" {
		t.Fatalf("Cookie = %q, want empty", got)
	}
	if got := req.Header.Get("X-Custom-Value"); got != "custom" {
		t.Fatalf("X-Custom-Value = %q, want custom", got)
	}
}
