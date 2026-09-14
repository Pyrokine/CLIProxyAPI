package access

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveClientAddressIgnoresHeadersFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	req.Header.Set("X-Real-IP", "198.51.100.11")

	got := ResolveClientAddress(req, nil)
	if got.IP != "127.0.0.1" || got.Key != "127.0.0.1" || got.DirectLoopback || got.Source != ClientAddressSourcePeer {
		t.Fatalf("ResolveClientAddress = %+v, want non-local loopback peer with ignored forwarding headers", got)
	}
}

func TestResolveClientAddressDirectLoopbackWithoutForwardingHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	got := ResolveClientAddress(req, nil)
	if got.IP != "127.0.0.1" || !got.DirectLoopback || got.Source != ClientAddressSourcePeer {
		t.Fatalf("ResolveClientAddress = %+v, want direct loopback peer", got)
	}
}

func TestResolveClientAddressTrustedLoopbackPeerIsNotDirect(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	got := ResolveClientAddress(req, []string{"127.0.0.1/32"})
	if got.DirectLoopback || !got.PeerLoopback || !got.TrustedPeer || got.ForwardingHeadersPresent {
		t.Fatalf("ResolveClientAddress = %+v, want trusted non-direct loopback peer", got)
	}
}

func TestResolveClientAddressUsesRightmostUntrustedForwardedHop(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.10, 10.1.2.3")
	req.Header.Set("X-Real-IP", "198.51.100.11")

	got := ResolveClientAddress(req, []string{"127.0.0.1/32", "10.0.0.0/8"})
	if got.IP != "198.51.100.10" || got.Key != "198.51.100.10" || got.DirectLoopback || got.Source != ClientAddressSourceForwardedFor {
		t.Fatalf("ResolveClientAddress = %+v, want forwarded client", got)
	}
}

func TestResolveClientAddressCombinesForwardedForFieldLines(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header["X-Forwarded-For"] = []string{"203.0.113.99", "198.51.100.10"}

	got := ResolveClientAddress(req, []string{"127.0.0.1/32"})
	if got.IP != "198.51.100.10" || got.Source != ClientAddressSourceForwardedFor {
		t.Fatalf("ResolveClientAddress = %+v, want last untrusted forwarded hop", got)
	}
}

func TestResolveClientAddressRejectsDuplicateRealIPFieldLines(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Add("X-Real-IP", "198.51.100.10")
	req.Header.Add("X-Real-IP", "203.0.113.99")

	got := ResolveClientAddress(req, []string{"127.0.0.1/32"})
	if got.Key != "invalid-forwarded:127.0.0.1" || got.Source != ClientAddressSourceInvalidForwarded {
		t.Fatalf("ResolveClientAddress = %+v, want invalid forwarded classification", got)
	}
}

func TestResolveClientAddressRejectsEmptyForwardedForField(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header["X-Forwarded-For"] = []string{""}

	got := ResolveClientAddress(req, []string{"127.0.0.1/32"})
	if got.Key != "invalid-forwarded:127.0.0.1" || got.DirectLoopback {
		t.Fatalf("ResolveClientAddress = %+v, want invalid non-local classification", got)
	}
}

func TestResolveClientAddressForwardedLoopbackIsNotDirect(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Real-IP", "::1")

	got := ResolveClientAddress(req, []string{"127.0.0.1/32"})
	if got.IP != "::1" || got.Key != "::1" || got.DirectLoopback || got.Source != ClientAddressSourceRealIP {
		t.Fatalf("ResolveClientAddress = %+v, want forwarded non-direct loopback", got)
	}
}

func TestResolveClientAddressMalformedForwardingUsesStablePeerKey(t *testing.T) {
	resolve := func(value string) ClientAddress {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", value)
		return ResolveClientAddress(req, []string{"127.0.0.1/32"})
	}

	first := resolve("not-an-ip")
	second := resolve("still-not-an-ip")
	if first.Key != "invalid-forwarded:127.0.0.1" || second.Key != first.Key {
		t.Fatalf("malformed keys = %q and %q, want stable peer key", first.Key, second.Key)
	}
	if first.DirectLoopback || first.Source != ClientAddressSourceInvalidForwarded {
		t.Fatalf("malformed address = %+v, want remote invalid-forwarded source", first)
	}
}

func TestResolveClientAddressRejectsProgrammaticFullNetworkTrust(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.10")

	got := ResolveClientAddress(req, []string{"0.0.0.0/0", "::/0"})
	if got.IP != "203.0.113.10" || got.Source != ClientAddressSourcePeer {
		t.Fatalf("ResolveClientAddress = %+v, want untrusted peer", got)
	}
}

func TestResolveClientAddressNormalizesIPv4MappedPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[::ffff:127.0.0.1]:12345"

	got := ResolveClientAddress(req, nil)
	if got.IP != "127.0.0.1" || !got.DirectLoopback {
		t.Fatalf("ResolveClientAddress = %+v, want normalized loopback", got)
	}
}

func TestResolveClientAddressUsesIPv6ForwardedChain(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[::1]:12345"
	req.Header.Set("X-Forwarded-For", "2001:db8::10, 2001:db8:ffff::1")

	got := ResolveClientAddress(req, []string{"::1/128", "2001:db8:ffff::/48"})
	if got.IP != "2001:db8::10" || got.Key != "2001:db8::10" || got.DirectLoopback {
		t.Fatalf("ResolveClientAddress = %+v, want forwarded IPv6 client", got)
	}
}

func TestClientAddressContextRoundTrip(t *testing.T) {
	want := ClientAddress{IP: "198.51.100.10", Key: "198.51.100.10", Source: ClientAddressSourceForwardedFor}
	ctx := WithClientAddress(context.Background(), want)
	got, ok := ClientAddressFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("ClientAddressFromContext = %+v, %t, want %+v, true", got, ok, want)
	}
	if _, ok = ClientAddressFromContext(context.Background()); ok {
		t.Fatal("empty context unexpectedly contained a client address")
	}
}
