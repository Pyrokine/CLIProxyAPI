package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/google/uuid"
)

// StableClaudeCLIAccountUUID returns a deterministic account UUID for a stable seed.
func StableClaudeCLIAccountUUID(seed string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cpa-claude-code-cli-account|"+seed)).String()
}

// ClaudeCLIAuthIdentitySeed remains stable across delegated token refreshes.
func ClaudeCLIAuthIdentitySeed(auth *cliproxyauth.Auth) string {
	if auth != nil {
		for _, field := range []struct{ label, value string }{{"auth-id", auth.ID}, {"auth-index", auth.Index}, {"auth-file", auth.FileName}} {
			if value := strings.TrimSpace(field.value); value != "" {
				return field.label + "|" + value
			}
		}
	}
	return ""
}

// PrepareClaudeCLIFingerprintAuth isolates synthesized identity from shared credentials.
func PrepareClaudeCLIFingerprintAuth(auth *cliproxyauth.Auth, seed string, synthesize bool) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, errors.New("claude CLI identity: auth is nil")
	}
	if !synthesize {
		if ClaudeCredentialAccountUUID(auth) == "" || !claudeauth.HasCanonicalDeviceIDPool(claudeauth.ReadDeviceIDPool(&auth.Metadata)) {
			return nil, errors.New("claude OAuth credential identity is not prepared")
		}
		return auth, nil
	}
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return nil, errors.New("claude CLI identity: stable credential identity is empty")
	}
	local := auth.Clone()
	if ClaudeCredentialAccountUUID(local) == "" {
		claudeauth.StoreMetadataString(&local.Metadata, "account_uuid", StableClaudeCLIAccountUUID(seed))
	}
	if !claudeauth.HasCanonicalDeviceIDPool(claudeauth.ReadDeviceIDPool(&local.Metadata)) {
		digest := sha256.Sum256([]byte("cpa-claude-code-cli-device|" + seed))
		claudeauth.StoreDeviceIDPool(&local.Metadata, []string{hex.EncodeToString(digest[:])})
	}
	return local, nil
}
