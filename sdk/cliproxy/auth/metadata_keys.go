package auth

var credentialMetadataAliases = map[string]string{
	"api-key":               "api_key",
	"base-url":              "base_url",
	"disable-cooling":       "disable_cooling",
	"excluded-models":       "excluded_models",
	"fingerprint-profile":   "fingerprint_profile",
	"model-aliases":         "model_aliases",
	"proxy-url":             "proxy_url",
	"request-retry":         "request_retry",
	"request-scoped-errors": "request_scoped_errors",
	"tool-prefix-disabled":  "tool_prefix_disabled",
}

// CanonicalCredentialMetadataKey returns the canonical name for a credential metadata key.
func CanonicalCredentialMetadataKey(key string) string {
	if canonical, ok := credentialMetadataAliases[key]; ok {
		return canonical
	}
	return key
}

// NormalizeCredentialMetadata migrates recognized legacy aliases to canonical names.
// An explicitly present canonical value takes precedence over its legacy alias.
func NormalizeCredentialMetadata(metadata map[string]any) {
	for key, value := range metadata {
		canonical := CanonicalCredentialMetadataKey(key)
		if canonical == key {
			continue
		}
		if _, exists := metadata[canonical]; !exists {
			metadata[canonical] = value
		}
		delete(metadata, key)
	}
}
