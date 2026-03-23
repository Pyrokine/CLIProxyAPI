package cliproxy

import (
	"net/http"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRoundTripperForDirectReturnsNil(t *testing.T) {
	t.Parallel()

	// "direct" has no URL scheme, so url.Parse sees it as a path-only URL.
	// The provider returns nil because the scheme is unrecognized.
	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "direct"})
	if rt != nil {
		t.Fatalf("expected nil RoundTripper for unrecognized proxy URL %q, got %T", "direct", rt)
	}
}

func TestRoundTripperForHTTPProxyReturnsTransport(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "http://proxy.example.com:3128"})
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", rt)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy function to be set for HTTP proxy URL")
	}
}

func TestRoundTripperForEmptyProxyReturnsNil(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: ""})
	if rt != nil {
		t.Fatalf("expected nil RoundTripper for empty proxy URL, got %T", rt)
	}
}

func TestRoundTripperForNilAuthReturnsNil(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(nil)
	if rt != nil {
		t.Fatalf("expected nil RoundTripper for nil auth, got %T", rt)
	}
}
