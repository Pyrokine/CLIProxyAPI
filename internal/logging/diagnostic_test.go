package logging

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSafeDiagnosticForLog(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		want    string
		secrets []string
	}{
		{name: "empty"},
		{name: "plain expiry", message: "access token expired", want: "access token expired"},
		{name: "inline fields", message: `upstream failed: access_token=access-secret refresh token: refresh-secret Authorization=Bearer bearer-secret`, secrets: []string{"access-secret", "refresh-secret", "bearer-secret"}},
		{name: "shared field names", message: `apiKey=api-secret management-key=management-secret auth_key=auth-secret cookie=session-secret`, secrets: []string{"api-secret", "management-secret", "auth-secret", "session-secret"}},
		{name: "quoted values", message: `{"message":"access token expired", "private_key":"private secret", "clientSecret":"client secret"}`, want: "access token expired", secrets: []string{"private secret", "client secret"}},
		{name: "unterminated quote", message: `error: token="a secret with spaces`, secrets: []string{"a secret", "with spaces"}},
		{name: "unterminated escape", message: "error: token=\"secret tail\\", secrets: []string{"secret", "tail"}},
		{name: "authorization", message: `failed Bearer abcdefghijklmnop; Proxy-Authorization: Basic c2VjcmV0`, secrets: []string{"abcdefghijklmnop", "c2VjcmV0"}},
		{name: "URL and proxy", message: `Post "https://user:password@oauth.example/token?code=oauth-secret&access_token=query-secret" via socks5://proxy-user:proxy-password@127.0.0.1:1080`, secrets: []string{"user:password", "oauth-secret", "query-secret", "proxy-user", "proxy-password"}},
		{name: "encoded query field", message: `upstream access%5Ftoken=encoded-secret`, secrets: []string{"encoded-secret"}},
		{name: "control characters", message: "access token expired\nforged\rline\t\x00\x1b" + string([]rune{0x2028, 0x202e}), want: "access token expired"},
		{name: "invalid UTF8", message: "upstream\xff error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := SafeDiagnosticForLog(test.message)
			if test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("diagnostic %q does not contain %q", got, test.want)
			}
			for _, secret := range test.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("diagnostic retained synthetic credential %q: %q", secret, got)
				}
			}
			if len(test.secrets) > 0 && !strings.Contains(got, "REDACTED") {
				t.Fatalf("missing redaction marker: %q", got)
			}
			if !utf8.ValidString(got) || strings.ContainsFunc(got, func(r rune) bool {
				return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ')
			}) {
				t.Fatalf("diagnostic is not a valid single line: %q", got)
			}
			if test.message == "" && got != "" {
				t.Fatalf("empty diagnostic = %q", got)
			}
		})
	}
}

func TestSafeDiagnosticForLogBoundsAndRetainsSignal(t *testing.T) {
	for _, message := range []string{
		strings.Repeat("界", 900),
		strings.Repeat("upstream context ", 1000) + "ACCESS TOKEN EXPIRED\nforged line",
		strings.Repeat(`token="long synthetic credential" `, 20) + "https://proxy-user:password@proxy.example",
		`token="` + strings.Repeat("secret", 200),
		`https://proxy-user:` + strings.Repeat("secret", 200),
	} {
		got := SafeDiagnosticForLog(message)
		if utf8.RuneCountInString(got) > diagnosticLogRuneLimit+3 || !strings.HasSuffix(got, "...") {
			t.Fatalf("invalid diagnostic length/truncation: %q", got)
		}
		if strings.Contains(strings.ToLower(message), "access token expired") && !strings.Contains(strings.ToLower(got), "access token expired") {
			t.Fatalf("expiry signal missing: %q", got)
		}
		for _, secret := range []string{"secret", "synthetic credential", "proxy-user", "password@"} {
			if strings.Contains(got, secret) {
				t.Fatalf("truncated diagnostic retained %q: %q", secret, got)
			}
		}
	}
}
