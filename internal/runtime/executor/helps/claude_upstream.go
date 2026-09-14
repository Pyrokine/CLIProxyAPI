package helps

import (
	"net/url"
	"strings"
)

// IsAnthropicUpstreamURL reports whether a request targets Anthropic's
// first-party API origin.
func IsAnthropicUpstreamURL(target *url.URL) bool {
	if target == nil || target.User != nil || !strings.EqualFold(target.Scheme, "https") ||
		!strings.EqualFold(target.Hostname(), "api.anthropic.com") {
		return false
	}
	port := target.Port()
	return port == "" || port == "443"
}
