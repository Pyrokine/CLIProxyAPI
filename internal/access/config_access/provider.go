package configaccess

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.ProviderTypeConfigAPIKey)
		return
	}

	keys := normalizeKeys(cfg.APIKeys)
	if len(keys) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.ProviderTypeConfigAPIKey)
		return
	}

	if cfg.AllowQueryAuth {
		log.Warnf(
			"security: allow-query-auth is enabled — API keys in URL query (?key=..., ?auth_token=...) " +
				"will be accepted. Keys may leak via access logs, referrers, and browser history. " +
				"Disable allow-query-auth unless a legacy client requires it.",
		)
	}

	sdkaccess.RegisterProvider(
		sdkaccess.ProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keys, cfg.AllowQueryAuth),
	)
}

type provider struct {
	name           string
	keys           map[string]struct{}
	allowQueryAuth bool
}

func newProvider(name string, keys []string, allowQueryAuth bool) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	return &provider{name: providerName, keys: keySet, allowQueryAuth: allowQueryAuth}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	offered := r.Header.Get("Authorization") != ""
	// Use constant-time comparison and always iterate all keys to avoid timing leaks.
	for _, candidate := range requestCredentialCandidates(r, p.allowQueryAuth) {
		if candidate.value == "" {
			continue
		}
		offered = true
		candidateBytes := []byte(candidate.value)
		matched := false
		for storedKey := range p.keys {
			if subtle.ConstantTimeCompare(candidateBytes, []byte(storedKey)) == 1 {
				matched = true
			}
		}
		if matched {
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: candidate.value,
				Metadata: map[string]string{
					"source": candidate.source,
				},
			}, nil
		}
	}

	if !offered {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	return nil, sdkaccess.NewInvalidCredentialError()
}

type credentialCandidate struct {
	value  string
	source string
}

func requestCredentialCandidates(r *http.Request, allowQuery bool) [5]credentialCandidate {
	candidates := [5]credentialCandidate{
		{extractBearerToken(r.Header.Get("Authorization")), "authorization"},
		{r.Header.Get("X-Goog-Api-Key"), "x-goog-api-key"},
		{r.Header.Get("X-Api-Key"), "x-api-key"},
		{"", "query-key"},
		{"", "query-auth-token"},
	}
	if allowQuery {
		query := r.URL.Query()
		candidates[3].value = query.Get("key")
		candidates[4].value = query.Get("auth_token")
	}
	return candidates
}

// FirstCredentialCandidate returns the first offered credential for failure accounting.
func FirstCredentialCandidate(r *http.Request, allowQuery bool) string {
	for _, candidate := range requestCredentialCandidates(r, allowQuery) {
		if candidate.value != "" {
			return candidate.value
		}
	}
	return ""
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

func normalizeKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if !validConfigAPIKey(trimmedKey) {
			log.Warn("config API key rejected: keys must be at least 8 printable ASCII characters")
			continue
		}
		if _, exists := seen[trimmedKey]; exists {
			continue
		}
		seen[trimmedKey] = struct{}{}
		normalized = append(normalized, trimmedKey)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func validConfigAPIKey(key string) bool {
	if len(key) < 8 {
		return false
	}
	for _, b := range []byte(key) {
		if b < 0x21 || b > 0x7E {
			return false
		}
	}
	return true
}
