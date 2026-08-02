package usage

import (
	"encoding/base64"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// The unit-separator sentinel cannot appear in a valid HTTP credential value,
// so historical source strings cannot impersonate typed storage values.
const (
	credentialSourceAPIKeyPrefix   = "\x1fcpa:usage-source:v1:api-key:"
	credentialSourceIdentityPrefix = "\x1fcpa:usage-source:v1:identity:"

	credentialDisplayAPIKeySuffix   = " [api_key]"
	credentialDisplayIdentitySuffix = " [identity]"
	credentialFilterPrefix          = "~cpa-credential-v1:"
)

func TagAPIKeyCredentialSource(source string, redacted bool) string {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return ""
	}
	if !redacted {
		trimmed = util.HideAPIKey(trimmed)
	}
	return credentialSourceAPIKeyPrefix + trimmed
}

func TagIdentityCredentialSource(source string) string {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return ""
	}
	return credentialSourceIdentityPrefix + trimmed
}

func DecodeCredentialSource(
	source string,
) (display string, kind coreusage.CredentialSourceKind, tagged bool) {
	trimmed := strings.TrimSpace(source)
	if strings.HasPrefix(trimmed, credentialSourceAPIKeyPrefix) {
		return strings.TrimPrefix(trimmed, credentialSourceAPIKeyPrefix), coreusage.CredentialSourceKindAPIKey, true
	}
	if strings.HasPrefix(trimmed, credentialSourceIdentityPrefix) {
		return strings.TrimPrefix(trimmed, credentialSourceIdentityPrefix), coreusage.CredentialSourceKindIdentity, true
	}
	return trimmed, "", false
}

func NormalizeImportedCredentialSource(source string, preserveEncryptedBackup bool) string {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return ""
	}
	if preserveEncryptedBackup {
		return trimmed
	}
	return TagAPIKeyCredentialSource(trimmed, false)
}

func DisambiguateCredentialSource(display string, kind coreusage.CredentialSourceKind) string {
	switch kind {
	case coreusage.CredentialSourceKindAPIKey:
		return display + credentialDisplayAPIKeySuffix
	case coreusage.CredentialSourceKindIdentity:
		return display + credentialDisplayIdentitySuffix
	default:
		return display
	}
}

func EncodeCredentialFilter(display string, kind coreusage.CredentialSourceKind) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(display))
	return credentialFilterPrefix + string(kind) + ":" + encoded
}

func DecodeCredentialFilter(
	value string,
) (display string, kind coreusage.CredentialSourceKind, typed bool) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, credentialFilterPrefix) {
		return trimmed, "", false
	}
	kindValue, encoded, ok := strings.Cut(strings.TrimPrefix(trimmed, credentialFilterPrefix), ":")
	if !ok {
		return trimmed, "", false
	}
	switch coreusage.CredentialSourceKind(kindValue) {
	case coreusage.CredentialSourceKindAPIKey:
		kind = coreusage.CredentialSourceKindAPIKey
	case coreusage.CredentialSourceKindIdentity:
		kind = coreusage.CredentialSourceKindIdentity
	default:
		return trimmed, "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return trimmed, "", false
	}
	return string(decoded), kind, true
}
