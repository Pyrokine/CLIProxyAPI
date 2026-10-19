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
		{name: "Kimi.ai provider", auth: &cliproxyauth.Auth{Provider: "kimi-ai"}, want: kimiauth.KimiAIAPIBaseURL + "/v1/responses"},
		{name: "Kimi.ai OAuth domain", auth: &cliproxyauth.Auth{Provider: "kimi", Metadata: map[string]any{"domain": "kimi.ai"}}, want: kimiauth.KimiAIAPIBaseURL + "/v1/responses"},
		{name: "Kimi.ai custom endpoint", auth: &cliproxyauth.Auth{Provider: "kimi-ai", Attributes: map[string]string{"base_url": "https://example.test/ai/v1"}}, want: "https://example.test/ai/v1/responses"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ResolveKimiResponsesURL(testCase.auth); got != testCase.want {
				t.Fatalf("ResolveKimiResponsesURL() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestResolveKimiAIChatAndClaudeURLs(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "kimi-ai", Metadata: map[string]any{"domain": "kimi.ai"}}
	if got, want := ResolveKimiChatURL(auth), kimiauth.KimiAIAPIBaseURL+"/v1/chat/completions"; got != want {
		t.Errorf("ResolveKimiChatURL() = %q, want %q", got, want)
	}
	if got, want := ResolveKimiClaudeBaseURL(auth), kimiauth.KimiAIAPIBaseURL; got != want {
		t.Errorf("ResolveKimiClaudeBaseURL() = %q, want %q", got, want)
	}
}
