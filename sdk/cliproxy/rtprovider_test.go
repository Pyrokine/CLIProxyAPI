package cliproxy

import (
	"net/http"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRoundTripperForDirectReturnsTransport(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "direct"})
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", rt)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestRoundTripperForHTTPProxyReturnsTransport(t *testing.T) {
	t.Parallel()

	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "http://8.8.8.8:3128"})
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
