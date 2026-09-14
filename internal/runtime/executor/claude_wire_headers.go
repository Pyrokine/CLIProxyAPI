package executor

import (
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
)

func doClaudeUpstreamRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	applyClaudeWireHeaderCasing(req)
	return client.Do(req)
}

var claudeWireHeaderCasing = map[string]string{
	"X-Stainless-Os":      "X-Stainless-OS",
	"Anthropic-Beta":      "anthropic-beta",
	"Anthropic-Version":   "anthropic-version",
	"X-App":               "x-app",
	"X-Client-Request-Id": "x-client-request-id",

	"Anthropic-Dangerous-Direct-Browser-Access": "anthropic-dangerous-direct-browser-access",
}

func applyClaudeWireHeaderCasing(r *http.Request) {
	if r == nil || r.Header == nil || !helps.IsAnthropicUpstreamURL(r.URL) {
		return
	}
	for canonical, wire := range claudeWireHeaderCasing {
		values, ok := r.Header[canonical]
		if !ok {
			continue
		}
		delete(r.Header, canonical)
		r.Header[wire] = values
	}
}
