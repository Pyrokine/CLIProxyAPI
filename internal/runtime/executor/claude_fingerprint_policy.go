package executor

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// A CLI wire profile does not confer OAuth lifecycle authority.
type claudeFingerprintPolicy struct {
	AuthIsOAuthToken     bool
	ProfileClaudeCodeCLI bool
	UseOAuthBetas        bool
}

func resolveClaudeFingerprintPolicy(cfg *config.Config, auth *cliproxyauth.Auth, apiKey string) claudeFingerprintPolicy {
	raw, present, err := cliproxyauth.ClaudeFingerprintProfile(auth)
	if err != nil {
		log.Warnf("invalid Claude fingerprint profile: %v; using caller-owned profile", err)
		raw = ""
	}
	if !present {
		if entry := resolveClaudeKeyConfig(cfg, auth); entry != nil {
			raw = entry.FingerprintProfile
		}
	}
	profile, valid := config.NormalizeClaudeFingerprintProfile(raw)
	if !valid {
		log.Warn("invalid Claude fingerprint profile; using caller-owned profile")
		profile = config.ClaudeFingerprintProfileDefault
	}
	oauth := isClaudeOAuthToken(apiKey)
	profileCLI := oauth || profile == config.ClaudeFingerprintProfileClaudeCodeCLI
	return claudeFingerprintPolicy{
		AuthIsOAuthToken:     oauth,
		ProfileClaudeCodeCLI: profileCLI,
		UseOAuthBetas:        profileCLI,
	}
}

func applyClaudeCLIIdentity(body []byte, auth *cliproxyauth.Auth, apiKey, sessionID string, policy claudeFingerprintPolicy) ([]byte, error) {
	seed := apiKey
	if auth != nil && strings.TrimSpace(auth.Attributes["api_key"]) == "" && !policy.AuthIsOAuthToken {
		seed = helps.ClaudeCLIAuthIdentitySeed(auth)
	}
	identityAuth, err := helps.PrepareClaudeCLIFingerprintAuth(auth, seed, !policy.AuthIsOAuthToken)
	if err != nil {
		return nil, err
	}
	updated, _, err := helps.ApplyClaudeCredentialMetadata(body, identityAuth, sessionID)
	if err != nil {
		return nil, fmt.Errorf("apply Claude credential metadata: %w", err)
	}
	return updated, nil
}

func claudeCCHSigningEnabled(apiKey string, cliFingerprint bool, origin string) bool {
	return isClaudeOAuthToken(apiKey) || (cliFingerprint && isAnthropicUpstreamBase(origin))
}

func isKimiMessagesUpstream(auth *cliproxyauth.Auth, endpoint string) bool {
	if auth != nil && strings.EqualFold(auth.Provider, "kimi") {
		return true
	}
	u, err := url.Parse(endpoint)
	return err == nil && strings.EqualFold(u.Hostname(), "api.kimi.com")
}
