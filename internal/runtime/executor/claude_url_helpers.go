package executor

import (
	"net/url"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
)

func isAnthropicUpstreamBase(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return helps.IsAnthropicUpstreamURL(parsed)
}
