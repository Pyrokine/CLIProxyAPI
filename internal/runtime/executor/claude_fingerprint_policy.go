package executor

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// 单次请求会多次解析同一配置值，已告警的值保留到进程结束
var claudeFingerprintProfileWarned sync.Map

// A CLI wire profile does not confer OAuth lifecycle authority.
type claudeFingerprintPolicy struct {
	AuthIsOAuthToken     bool
	ProfileClaudeCodeCLI bool
	UseOAuthBetas        bool
}

func resolveClaudeFingerprintPolicy(
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	apiKey string,
) claudeFingerprintPolicy {
	raw, present, err := cliproxyauth.ClaudeFingerprintProfile(auth)
	if err != nil {
		var invalidValue any
		if value, exists := auth.Attributes[cliproxyauth.AttributeClaudeFingerprintProfile]; exists {
			invalidValue = value
		} else {
			invalidValue, exists = util.ReadClaudeMetadataValue(
				&auth.Metadata, cliproxyauth.AttributeClaudeFingerprintProfile,
			)
			if !exists {
				invalidValue, _ = util.ReadClaudeMetadataValue(&auth.Metadata, "fingerprint-profile")
			}
		}
		warningValue := fmt.Sprint(invalidValue)
		warningKey := fmt.Sprintf("%T:%s", invalidValue, warningValue)
		if value, ok := invalidValue.(string); ok {
			warningValue = strings.TrimSpace(value)
			warningKey = "string:" + warningValue
		}
		if _, warned := claudeFingerprintProfileWarned.LoadOrStore(warningKey, struct{}{}); !warned {
			log.Warnf("invalid Claude fingerprint profile %q: %v; using caller-owned profile", warningValue, err)
		}
		raw = ""
	}
	if !present {
		if entry := resolveClaudeKeyConfig(cfg, auth); entry != nil {
			raw = entry.FingerprintProfile
		}
	}
	profile, valid := config.NormalizeClaudeFingerprintProfile(raw)
	if !valid {
		key := "string:" + strings.TrimSpace(raw)
		if _, warned := claudeFingerprintProfileWarned.LoadOrStore(key, struct{}{}); !warned {
			log.Warnf("invalid Claude fingerprint profile %q; using caller-owned profile", strings.TrimSpace(raw))
		}
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

func applyClaudeCLIIdentity(
	body []byte,
	auth *cliproxyauth.Auth,
	apiKey, sessionID string,
	policy claudeFingerprintPolicy,
) ([]byte, error) {
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
	if auth != nil && (strings.EqualFold(strings.TrimSpace(auth.Provider), "kimi") ||
		strings.EqualFold(strings.TrimSpace(auth.Provider), "kimi-ai")) {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(endpoint))
	return err == nil && (strings.EqualFold(u.Hostname(), "api.kimi.com") ||
		strings.EqualFold(u.Hostname(), "api.kimi.ai"))
}
