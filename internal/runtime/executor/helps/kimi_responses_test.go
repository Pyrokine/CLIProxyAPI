package helps

import (
	"testing"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolveKimiResponsesURL(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{name: "nil auth", want: kimiauth.APIBaseURL + "/v1/responses"},
		{name: "empty attributes", auth: &cliproxyauth.Auth{Attributes: map[string]string{}}, want: kimiauth.APIBaseURL + "/v1/responses"},
		{name: "base URL without v1", auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://example.test/kimi"}}, want: "https://example.test/kimi/v1/responses"},
		{name: "base URL with trailing slash", auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://example.test/kimi/"}}, want: "https://example.test/kimi/v1/responses"},
		{name: "base URL with v1", auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://example.test/kimi/v1"}}, want: "https://example.test/kimi/v1/responses"},
		{name: "base URL with v1 and trailing slash", auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://example.test/kimi/v1/"}}, want: "https://example.test/kimi/v1/responses"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ResolveKimiResponsesURL(testCase.auth); got != testCase.want {
				t.Fatalf("ResolveKimiResponsesURL() = %q, want %q", got, testCase.want)
			}
		})
	}
}
