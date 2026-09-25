package management

import (
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBuildAuthFileEntryNeverProjectsDevinAPIKeyAsAccount(t *testing.T) {
	const secret = "devin-session-token$secret-value"
	for _, test := range []struct {
		name          string
		auth          *coreauth.Auth
		wantAccount   string
		wantType      string
		wantSecretOut bool
	}{
		{
			name: "legacy Devin API key attribute",
			auth: &coreauth.Auth{
				ID:       "devin-legacy",
				Provider: "devin",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"runtime_only": "true",
					"api_key":      secret,
				},
			},
		},
		{
			name: "Devin email remains visible",
			auth: &coreauth.Auth{
				ID:       "devin-email",
				Provider: "devin",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"runtime_only": "true",
					"api_key":      secret,
				},
				Metadata: map[string]any{"email": "user@example.com"},
			},
			wantAccount: "user@example.com",
			wantType:    "oauth",
		},
		{
			name: "other provider API key account is unchanged",
			auth: &coreauth.Auth{
				ID:       "custom-api-key",
				Provider: "custom",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"runtime_only": "true",
					"api_key":      secret,
				},
			},
			wantAccount:   secret,
			wantType:      "api_key",
			wantSecretOut: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := (&Handler{}).buildAuthFileEntry(test.auth)
			if entry == nil {
				t.Fatal("buildAuthFileEntry() = nil")
			}
			account, _ := entry["account"].(string)
			accountType, _ := entry["account_type"].(string)
			if account != test.wantAccount || accountType != test.wantType {
				t.Fatalf("account = (%q, %q), want (%q, %q)", accountType, account, test.wantType, test.wantAccount)
			}
			encoded, errMarshal := json.Marshal(entry)
			if errMarshal != nil {
				t.Fatalf("marshal entry: %v", errMarshal)
			}
			if got := strings.Contains(string(encoded), secret); got != test.wantSecretOut {
				t.Fatalf("secret visible = %t, want %t: %s", got, test.wantSecretOut, encoded)
			}
		})
	}
}
