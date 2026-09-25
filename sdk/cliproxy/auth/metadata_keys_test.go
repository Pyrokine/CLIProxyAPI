package auth

import (
	"reflect"
	"testing"
)

func TestNormalizeCredentialMetadata(t *testing.T) {
	metadata := map[string]any{
		"api-key":               "legacy-key",
		"base-url":              "https://legacy.example",
		"disable-cooling":       true,
		"excluded-models":       []any{"legacy-model"},
		"fingerprint-profile":   "oauth-cli",
		"model-aliases":         []any{"legacy-alias"},
		"proxy-url":             "http://legacy-proxy",
		"request-retry":         float64(3),
		"request-scoped-errors": []any{"legacy-rule"},
		"tool-prefix-disabled":  true,
		"proxy_url":             "http://canonical-proxy",
		"keep":                  "value",
	}

	NormalizeCredentialMetadata(metadata)

	want := map[string]any{
		"api_key":               "legacy-key",
		"base_url":              "https://legacy.example",
		"disable_cooling":       true,
		"excluded_models":       []any{"legacy-model"},
		"fingerprint_profile":   "oauth-cli",
		"model_aliases":         []any{"legacy-alias"},
		"proxy_url":             "http://canonical-proxy",
		"request_retry":         float64(3),
		"request_scoped_errors": []any{"legacy-rule"},
		"tool_prefix_disabled":  true,
		"keep":                  "value",
	}
	if !reflect.DeepEqual(metadata, want) {
		t.Fatalf("NormalizeCredentialMetadata() = %#v, want %#v", metadata, want)
	}
}

func TestCanonicalCredentialMetadataKeyLeavesUnknownKeysUntouched(t *testing.T) {
	if got := CanonicalCredentialMetadataKey("custom-key"); got != "custom-key" {
		t.Fatalf("CanonicalCredentialMetadataKey() = %q, want custom-key", got)
	}
}
