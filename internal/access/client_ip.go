package access

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// ClientAddressSource identifies how the client address was resolved.
type ClientAddressSource string

const (
	ClientAddressSourcePeer             ClientAddressSource = "peer"
	ClientAddressSourceForwardedFor     ClientAddressSource = "x-forwarded-for"
	ClientAddressSourceRealIP           ClientAddressSource = "x-real-ip"
	ClientAddressSourceInvalidForwarded ClientAddressSource = "invalid-forwarded"
)

// ClientAddress contains the normalized client identity used by authentication.
type ClientAddress struct {
	IP                       string
	Key                      string
	Source                   ClientAddressSource
	PeerLoopback             bool
	TrustedPeer              bool
	ForwardingHeadersPresent bool
	DirectLoopback           bool
}

type clientAddressContextKey struct{}

// WithClientAddress stores the resolved client identity for downstream middleware.
func WithClientAddress(ctx context.Context, client ClientAddress) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientAddressContextKey{}, client)
}

// ClientAddressFromContext returns the client identity resolved at the trust boundary.
func ClientAddressFromContext(ctx context.Context) (ClientAddress, bool) {
	if ctx == nil {
		return ClientAddress{}, false
	}
	client, ok := ctx.Value(clientAddressContextKey{}).(ClientAddress)
	return client, ok && client.Key != ""
}

// ResolveClientAddress resolves a request address across explicitly trusted proxies.
func ResolveClientAddress(r *http.Request, trustedProxies []string) ClientAddress {
	if r == nil {
		return peerClientAddress("unknown", false, false, false)
	}

	peer, ok := parseClientAddress(r.RemoteAddr)
	if !ok {
		remote := strings.TrimSpace(r.RemoteAddr)
		if remote == "" {
			remote = "unknown"
		}
		return peerClientAddress(remote, false, false, false)
	}

	peerAddress := peer.String()
	forwardedForValues := r.Header.Values("X-Forwarded-For")
	realIPValues := r.Header.Values("X-Real-IP")
	forwardingHeadersPresent := len(forwardedForValues) > 0 || len(realIPValues) > 0
	forwardedFor := strings.TrimSpace(strings.Join(forwardedForValues, ","))
	realIP := ""
	if len(realIPValues) == 1 {
		realIP = strings.TrimSpace(realIPValues[0])
	}
	trustedPeer := trustedProxyContains(trustedProxies, peer)
	resolved := peerClientAddress(
		peerAddress,
		peer.IsLoopback(),
		trustedPeer,
		forwardingHeadersPresent,
	)
	if !trustedPeer || !forwardingHeadersPresent {
		return resolved
	}
	if len(realIPValues) > 1 {
		return invalidForwardedClientAddress(peerAddress, peer.IsLoopback())
	}

	if len(forwardedForValues) > 0 {
		if forwardedFor == "" {
			return invalidForwardedClientAddress(peerAddress, peer.IsLoopback())
		}
		forwarded, valid := resolveForwardedFor(forwardedFor, trustedProxies)
		if !valid {
			return invalidForwardedClientAddress(peerAddress, peer.IsLoopback())
		}
		return forwardedClientAddress(forwarded, ClientAddressSourceForwardedFor, peer.IsLoopback())
	}

	forwarded, errParse := netip.ParseAddr(realIP)
	if errParse != nil {
		return invalidForwardedClientAddress(peerAddress, peer.IsLoopback())
	}
	return forwardedClientAddress(forwarded.Unmap().WithZone(""), ClientAddressSourceRealIP, peer.IsLoopback())
}

func peerClientAddress(
	ip string,
	peerLoopback bool,
	trustedPeer bool,
	forwardingHeadersPresent bool,
) ClientAddress {
	return ClientAddress{
		IP:                       ip,
		Key:                      ip,
		Source:                   ClientAddressSourcePeer,
		PeerLoopback:             peerLoopback,
		TrustedPeer:              trustedPeer,
		ForwardingHeadersPresent: forwardingHeadersPresent,
		DirectLoopback:           peerLoopback && !trustedPeer && !forwardingHeadersPresent,
	}
}

func forwardedClientAddress(addr netip.Addr, source ClientAddressSource, peerLoopback bool) ClientAddress {
	ip := addr.Unmap().WithZone("").String()
	return ClientAddress{
		IP:                       ip,
		Key:                      ip,
		Source:                   source,
		PeerLoopback:             peerLoopback,
		TrustedPeer:              true,
		ForwardingHeadersPresent: true,
	}
}

func invalidForwardedClientAddress(peer string, peerLoopback bool) ClientAddress {
	key := "invalid-forwarded:" + peer
	return ClientAddress{
		IP:                       key,
		Key:                      key,
		Source:                   ClientAddressSourceInvalidForwarded,
		PeerLoopback:             peerLoopback,
		TrustedPeer:              true,
		ForwardingHeadersPresent: true,
	}
}

func resolveForwardedFor(value string, trustedProxies []string) (netip.Addr, bool) {
	parts := strings.Split(value, ",")
	var leftmost netip.Addr
	for _, part := range slices.Backward(parts) {
		part := strings.TrimSpace(part)
		addr, errParse := netip.ParseAddr(part)
		if errParse != nil {
			return netip.Addr{}, false
		}
		addr = addr.Unmap().WithZone("")
		leftmost = addr
		if !trustedProxyContains(trustedProxies, addr) {
			return addr, true
		}
	}
	return leftmost, leftmost.IsValid()
}

func trustedProxyContains(values []string, addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if strings.Contains(value, "/") {
			prefix, errParse := netip.ParsePrefix(value)
			if errParse == nil && prefix.Bits() != 0 && !prefix.Addr().Is4In6() && prefix.Contains(addr) {
				return true
			}
			continue
		}
		trusted, errParse := netip.ParseAddr(value)
		if errParse == nil && trusted.Unmap().WithZone("") == addr {
			return true
		}
	}
	return false
}

func parseClientAddress(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if host, _, errSplit := net.SplitHostPort(value); errSplit == nil {
		value = host
	}
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	addr, errParse := netip.ParseAddr(value)
	if errParse != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}
