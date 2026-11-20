package executor

import (
	"strings"
	"sync"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolveClaudeFingerprintPolicy(t *testing.T) {
	tests := []struct {
		name, provider, apiKey string
		attrs                  map[string]string
		metadata               map[string]any
		cfg                    *config.Config
		wantAuthOAuth          bool
		wantProfileOAuth       bool
	}{
		{
			name: "real oauth token", apiKey: "sk-ant-oat-real",
			attrs:         map[string]string{"api_key": "sk-ant-oat-real"},
			wantAuthOAuth: true, wantProfileOAuth: true,
		},
		{
			name: "api key default", apiKey: "key-default",
			attrs: map[string]string{"api_key": "key-default"},
		},
		{
			name: "official anthropic api key opts in via claude-code-cli attribute", apiKey: "key-attr",
			attrs: map[string]string{
				"api_key":             "key-attr",
				"fingerprint_profile": "claude-code-cli",
			},
			wantProfileOAuth: true,
		},
		{
			name: "official anthropic api key opts in via oauth-cli alias", apiKey: "key-attr-legacy",
			attrs: map[string]string{
				"api_key":             "key-attr-legacy",
				"fingerprint_profile": "oauth-cli",
			},
			wantProfileOAuth: true,
		},
		{
			name: "official anthropic explicit 443 api key opts in via profile", apiKey: "key-official-443",
			attrs: map[string]string{
				"api_key": "key-official-443", "base_url": "https://api.anthropic.com:443",
				"fingerprint_profile": "claude-code-cli",
			},
			wantProfileOAuth: true,
		},
		{
			name: "api key claude-code-cli attribute on gateway", apiKey: "key-attr-gateway",
			attrs: map[string]string{
				"api_key": "key-attr-gateway", "base_url": "https://gateway.example",
				"fingerprint_profile": "claude-code-cli",
			},
			wantProfileOAuth: true,
		},
		{
			name: "api key claude-code-cli config entry", apiKey: "key-config",
			attrs: map[string]string{"api_key": "key-config", "base_url": "https://gateway.example"},
			cfg: &config.Config{
				ClaudeKey: []config.ClaudeKey{
					{
						APIKey:  "key-config",
						BaseURL: "https://gateway.example", FingerprintProfile: "claude-code-cli",
					},
				},
			},
			wantProfileOAuth: true,
		},
		{
			name: "official anthropic api key opts in via metadata profile", apiKey: "key-metadata",
			attrs:    map[string]string{"api_key": "key-metadata"},
			metadata: map[string]any{"fingerprint_profile": "claude-code-cli"}, wantProfileOAuth: true,
		},
		{
			name: "kimi default token has no fingerprint", provider: "kimi", apiKey: "kimi-access-token",
			metadata: map[string]any{"access_token": "kimi-access-token"},
		},
		{
			name: "kimi with claude-code-cli profile opts in", provider: "kimi", apiKey: "kimi-access-token",
			metadata: map[string]any{
				"access_token": "kimi-access-token", "fingerprint_profile": "claude-code-cli",
			},
			wantProfileOAuth: true,
		},
		{
			name: "kimi oauth json hyphenated fingerprint-profile opts in", provider: "kimi",
			apiKey: "kimi-access-token",
			metadata: map[string]any{
				"access_token": "kimi-access-token", "fingerprint-profile": "claude-code-cli",
			},
			wantProfileOAuth: true,
		},
		{
			name: "unknown profile ignored", apiKey: "key-unknown",
			attrs: map[string]string{"api_key": "key-unknown", "fingerprint_profile": "not-a-profile"},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				auth := &cliproxyauth.Auth{
					Provider:   test.provider,
					Attributes: test.attrs,
					Metadata:   test.metadata,
				}
				policy := resolveClaudeFingerprintPolicy(test.cfg, auth, test.apiKey)
				if policy.AuthIsOAuthToken != test.wantAuthOAuth ||
					policy.ProfileClaudeCodeCLI !=
						test.wantProfileOAuth ||
					policy.UseOAuthBetas !=
						test.wantProfileOAuth {
					t.Fatalf(
						"policy = %+v, want OAuth authority=%t, CLI profile/betas=%t",
						policy, test.wantAuthOAuth, test.wantProfileOAuth,
					)
				}
			},
		)
	}
}

func TestClaudeFingerprintProfileFromAuthConcurrentMetadata(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"fingerprint_profile": "claude-code-cli"}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 1_000 {
			got, present, err := cliproxyauth.ClaudeFingerprintProfile(auth)
			if err != nil || !present || got != config.ClaudeFingerprintProfileClaudeCodeCLI {
				t.Errorf("ClaudeFingerprintProfile = %q, present=%t, error=%v", got, present, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 1_000 {
			//goland:noinspection SpellCheckingInspection
			claudeauth.StoreMetadataString(
				&auth.Metadata,
				"account_uuid",
				"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			)
		}
	}()
	wg.Wait()
}

func TestNormalizeClaudeFingerprintProfileWarnsOncePerValue(t *testing.T) {
	capture := captureClaudeProfileWarnings(t)
	const firstTypo = "claude-code-cli-test-typo-a"
	const secondTypo = "claude-code-cli-test-typo-b"
	for _, value := range []string{firstTypo, secondTypo} {
		claudeFingerprintProfileWarned.Delete("string:" + value)
		t.Cleanup(func() { claudeFingerprintProfileWarned.Delete("string:" + value) })
	}
	resolve := func(value string) claudeFingerprintPolicy {
		auth := fingerprintAuth(value, "https://gateway.example")
		return resolveClaudeFingerprintPolicy(&config.Config{}, auth, auth.Attributes["api_key"])
	}
	for range 5 {
		if policy := resolve(firstTypo); policy.ProfileClaudeCodeCLI || policy.AuthIsOAuthToken ||
			policy.UseOAuthBetas {
			t.Fatalf("invalid profile policy = %+v, want caller-owned defaults", policy)
		}
	}
	resolve(secondTypo)
	for range 3 {
		resolve("claude-code-cli")
		resolve("")
	}
	first, second, total := 0, 0, 0
	for _, entry := range capture.AllEntries() {
		first += strings.Count(entry.Message, firstTypo)
		second += strings.Count(entry.Message, secondTypo)
		if strings.HasPrefix(entry.Message, "invalid Claude fingerprint profile") {
			total++
		}
	}
	if first != 1 || second != 1 || total != 2 {
		t.Fatalf("warning counts first=%d, second=%d, total=%d, want 1/1/2", first, second, total)
	}
}

func TestResolveClaudeFingerprintPolicyIsOriginIndependent(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey: "key-origin-independent", FingerprintProfile: "claude-code-cli",
			},
		},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-origin-independent"}}
	policy := resolveClaudeFingerprintPolicy(cfg, auth, "key-origin-independent")
	if !policy.ProfileClaudeCodeCLI || !policy.UseOAuthBetas || policy.AuthIsOAuthToken {
		t.Fatalf("policy = %+v, want opted-in non-OAuth profile", policy)
	}
	for _, origin := range []string{
		"https://api.anthropic.com/v1/messages?beta=true",
		"https://gateway.example/v1/messages?beta=true",
		"https://api.kimi.com/v1/messages",
	} {
		wantCCH := origin == "https://api.anthropic.com/v1/messages?beta=true"
		if got := claudeCCHSigningEnabled(
			"key-origin-independent", policy.ProfileClaudeCodeCLI, origin,
		); got != wantCCH {
			t.Fatalf("claudeCCHSigningEnabled(%q) = %t, want %t", origin, got, wantCCH)
		}
	}
}
