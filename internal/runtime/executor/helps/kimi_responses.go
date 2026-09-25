package helps

import (
	"strings"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ResolveKimiResponsesURL resolves the upstream URL for Kimi Responses API requests.
func ResolveKimiResponsesURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/"); raw != "" {
			if strings.HasSuffix(raw, "/v1") {
				return raw + "/responses"
			}
			return raw + "/v1/responses"
		}
	}
	return kimiauth.APIBaseURL + "/v1/responses"
}
