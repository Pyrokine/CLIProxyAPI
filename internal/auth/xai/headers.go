package xai

import "net/http"

const (
	ShellTokenAuthHeader     = "X-XAI-Token-Auth"
	ShellTokenAuthValue      = "xai-grok-cli"
	ShellClientVersionHeader = "x-grok-client-version"
	// Chat proxy rejects outdated Grok CLI versions with HTTP 426.
	ShellClientVersionValue         = "1.0.46"
	ShellClientIdentifierHeader     = "x-grok-client-identifier"
	ShellClientIdentifierValue      = "grok-shell"
	ShellAuthenticateResponseHeader = "x-authenticateresponse"
	ShellAuthenticateResponseValue  = "authenticate-response"
)

// ApplyShellIdentityHeaders adds the identity headers used by Grok Shell.
func ApplyShellIdentityHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set(ShellTokenAuthHeader, ShellTokenAuthValue)
	headers.Set(ShellClientVersionHeader, ShellClientVersionValue)
	headers.Set("User-Agent", "xai-grok-workspace/"+ShellClientVersionValue)
	headers.Set(ShellClientIdentifierHeader, ShellClientIdentifierValue)
	headers.Set(ShellAuthenticateResponseHeader, ShellAuthenticateResponseValue)
}

// ShellIdentityHeaderValues returns an independent copy for proxied API calls.
func ShellIdentityHeaderValues() map[string]string {
	headers := make(http.Header)
	ApplyShellIdentityHeaders(headers)
	values := make(map[string]string, len(headers))
	for name := range headers {
		values[name] = headers.Get(name)
	}
	return values
}
