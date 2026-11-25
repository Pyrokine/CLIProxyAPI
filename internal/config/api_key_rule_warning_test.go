package config

import (
	"strings"
	"testing"
)

func TestValidateAPIKeyRulesKeepsCredentialOutOfWarnings(t *testing.T) {
	const key = "synthetic-configured-client-key"
	cfg := &Config{
		SDKConfig: SDKConfig{
			APIKeys: []string{key},
			APIKeyRules: map[string]APIKeyRule{
				key: {
					Providers: []string{" "},
					Limits:    []APIKeyPolicyLimit{{Window: "24h", Tokens: -1}},
				},
			},
		},
	}
	warnings := strings.Join(ValidateConfig(cfg), "\n")
	if strings.Contains(warnings, key) || !strings.Contains(warnings, "api-key-rules.providers[0] is empty") ||
		!strings.Contains(warnings, "api-key-rules.limits[0] contains a negative limit") {
		t.Fatal("validation must report the failing fields without the configured key")
	}
}
