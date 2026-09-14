package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// normalizeTrustedProxies validates and canonicalizes trusted proxy IP addresses and CIDR ranges.
func normalizeTrustedProxies(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}

	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("remote-management.trusted-proxies contains an empty value")
		}

		canonical := ""
		if strings.Contains(value, "/") {
			prefix, errParse := netip.ParsePrefix(value)
			if errParse != nil {
				return nil, fmt.Errorf("remote-management.trusted-proxies contains invalid CIDR %q", value)
			}
			if prefix.Addr().Is4In6() {
				return nil, fmt.Errorf(
					"remote-management.trusted-proxies contains unsupported IPv4-mapped CIDR %q", value,
				)
			}
			if prefix.Bits() == 0 {
				return nil, fmt.Errorf(
					"remote-management.trusted-proxies cannot trust the full address space %q", value,
				)
			}
			canonical = prefix.Masked().String()
		} else {
			addr, errParse := netip.ParseAddr(value)
			if errParse != nil {
				return nil, fmt.Errorf("remote-management.trusted-proxies contains invalid IP %q", value)
			}
			canonical = addr.Unmap().String()
		}

		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	return normalized, nil
}
