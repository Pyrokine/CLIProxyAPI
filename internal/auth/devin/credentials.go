package devin

import (
	"strings"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// CredentialsFromAuth resolves Devin wire credentials without exposing them outside the server process.
func CredentialsFromAuth(auth *coreauth.Auth) (token, baseURL, deviceSeed string) {
	baseURL = DefaultServerURL
	if auth == nil {
		return token, baseURL, deviceSeed
	}
	if auth.Attributes != nil {
		token = firstNonEmptyString(
			auth.Attributes["api_key"],
			auth.Attributes["session_token"],
			auth.Attributes["token"],
		)
		baseURL = firstNonEmptyString(auth.Attributes["base_url"], baseURL)
		deviceSeed = strings.TrimSpace(auth.Attributes["device_seed"])
	}
	if auth.Metadata != nil {
		if token == "" {
			token = firstNonEmptyMetadata(auth.Metadata, "api_key", "session_token", "token")
		}
		if baseURL == DefaultServerURL {
			if metadataBaseURL := firstNonEmptyMetadata(auth.Metadata, "base_url"); metadataBaseURL != "" {
				baseURL = metadataBaseURL
			}
		}
		if deviceSeed == "" {
			deviceSeed = firstNonEmptyMetadata(auth.Metadata, "device_seed")
		}
	}
	if deviceSeed == "" {
		deviceSeed = firstNonEmptyString(auth.Index, auth.ID, auth.FileName, token)
	}
	return strings.TrimSpace(token), strings.TrimSpace(baseURL), strings.TrimSpace(deviceSeed)
}

func firstNonEmptyMetadata(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		value, _ := metadata[key].(string)
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
