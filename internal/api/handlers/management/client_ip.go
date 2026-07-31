package management

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type managementClientAddress struct {
	ip    string
	local bool
}

func resolveManagementClientAddress(r *http.Request, cfg *config.Config) managementClientAddress {
	if r == nil {
		return managementClientAddress{ip: "unknown"}
	}

	peer, ok := parseManagementAddress(r.RemoteAddr)
	if !ok {
		remote := strings.TrimSpace(r.RemoteAddr)
		if remote == "" {
			remote = "unknown"
		}
		return managementClientAddress{ip: remote}
	}

	resolved := managementClientAddress{ip: peer.String(), local: peer.IsLoopback()}
	forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	realIP := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if forwardedFor == "" && realIP == "" {
		return resolved
	}
	if cfg == nil || !managementTrustedProxyContains(cfg.RemoteManagement.TrustedProxies, peer) {
		return resolved
	}

	if forwardedFor != "" {
		forwarded, valid := resolveManagementForwardedFor(forwardedFor, cfg.RemoteManagement.TrustedProxies)
		if !valid {
			return invalidManagementForwardedAddress(peer, forwardedFor, realIP)
		}
		return managementClientAddress{ip: forwarded.String(), local: forwarded.IsLoopback()}
	}

	forwarded, errParse := netip.ParseAddr(realIP)
	if errParse != nil {
		return invalidManagementForwardedAddress(peer, forwardedFor, realIP)
	}
	forwarded = forwarded.Unmap().WithZone("")
	return managementClientAddress{ip: forwarded.String(), local: forwarded.IsLoopback()}
}

func invalidManagementForwardedAddress(
	peer netip.Addr,
	forwardedFor string,
	realIP string,
) managementClientAddress {
	digest := sha256.Sum256([]byte(peer.String() + "\x00" + forwardedFor + "\x00" + realIP))
	return managementClientAddress{ip: "invalid-forwarded:" + hex.EncodeToString(digest[:])}
}

func resolveManagementForwardedFor(value string, trustedProxies []string) (netip.Addr, bool) {
	parts := strings.Split(value, ",")
	var leftmost netip.Addr
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		addr, errParse := netip.ParseAddr(part)
		if errParse != nil {
			return netip.Addr{}, false
		}
		addr = addr.Unmap().WithZone("")
		leftmost = addr
		if !managementTrustedProxyContains(trustedProxies, addr) {
			return addr, true
		}
	}
	return leftmost, leftmost.IsValid()
}

func managementTrustedProxyContains(values []string, addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	for _, value := range values {
		value = strings.TrimSpace(value)
		if strings.Contains(value, "/") {
			prefix, errParse := netip.ParsePrefix(value)
			if errParse == nil && prefix.Contains(addr) {
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

func parseManagementAddress(value string) (netip.Addr, bool) {
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
