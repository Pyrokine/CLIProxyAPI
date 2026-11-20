package util

import (
	"net/url"
	"strings"
	"testing"
)

func TestSanitizeUpstreamURL(t *testing.T) {
	raw := "https://name:synthetic-password@upstream.example.test/v1/responses?model=forwarded&api_key=synthetic-key&access_token=synthetic-token&X-Amz-Signature=synthetic-signature&code=synthetic-code&state=synthetic-state#synthetic-fragment"
	got := SanitizeUpstreamURL(raw)
	parsed, err := url.Parse(got)
	if err != nil || parsed.Host != "upstream.example.test" || parsed.Path != "/v1/responses" || parsed.User != nil || parsed.Fragment != "" {
		t.Fatalf("sanitized URL = %q, error = %v", got, err)
	}
	if parsed.Query().Get("model") != "forwarded" {
		t.Fatalf("lost non-secret query: %q", got)
	}
	if strings.Contains(got, "synthetic-") || strings.Contains(got, "name") {
		t.Fatalf("URL disclosed credentials: %q", got)
	}
	if SanitizeUpstreamURL(got) != got {
		t.Fatal("URL sanitization is not idempotent")
	}
	for _, invalid := range []string{
		"", "https://name:secret@upstream.example.test/?key=%zz", "not a URL", "file:///tmp/private", "https://%zz",
	} {
		if got := SanitizeUpstreamURL(invalid); got != "" {
			t.Fatalf("invalid URL %q became %q", invalid, got)
		}
	}
}
