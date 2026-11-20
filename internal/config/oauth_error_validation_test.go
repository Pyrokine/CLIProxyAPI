package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateConfigRejectsOAuthErrorActionsAndInvalidRegex(t *testing.T) {
	for _, test := range []struct {
		name, action, pattern, diagnostic string
	}{
		{"unknown action", "pause", "quota.*", "action"},
		{"invalid regex", "stop", "[", "match-regexr"},
		{"stop", "stop", "quota.*", ""},
		{"stop cooldown", "stop-and-cooldown", "^quota$", ""},
		{"continue", "continue", "quota|rate_limit", ""},
		{"continue cooldown", "continue-and-cooldown", "(?i)quota", ""},
		{"normalized action", "  STOP  ", " quota.* ", ""},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				raw := fmt.Sprintf(
					"host: 127.0.0.1\nport: 8317\napi-keys: [synthetic-validation-key]\noauth-request-scoped-"+
						"errors:\n  codex:\n    - status: 429\n      match: [quota]\n      match-regexr: [%q]"+
						"\n      action: %q\n",
					test.pattern, test.action,
				)
				cfg, err := ParseConfigBytes([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				warnings := ValidateConfig(cfg)
				matched := false
				for _, warning := range warnings {
					if strings.Contains(warning, "oauth-request-scoped-errors") {
						if test.diagnostic == "" || !strings.Contains(warning, test.diagnostic) {
							t.Fatalf("unexpected OAuth diagnostic=%q", warning)
						}
						matched = true
					}
				}
				if matched != (test.diagnostic != "") {
					t.Fatalf("OAuth validation warnings=%v wanted=%q", warnings, test.diagnostic)
				}
				if test.diagnostic == "" && len(warnings) != 0 {
					t.Fatalf("valid control produced warnings=%v", warnings)
				}
			},
		)
	}
}
