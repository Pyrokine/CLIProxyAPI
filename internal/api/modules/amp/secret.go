package amp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// secretSource provides Amp API keys with configurable precedence and caching
type secretSource interface {
	Get(ctx context.Context) (string, error)
}

// cachedSecret holds a secret value with expiration
type cachedSecret struct {
	value     string
	expiresAt time.Time
}

// multiSourceSecret implements precedence-based secret lookup:
// 1. Explicit config value (highest priority)
// 2. Environment variable AMP_API_KEY
// 3. File-based secret (lowest priority)
type multiSourceSecret struct {
	explicitKey string
	envKey      string
	filePath    string
	cacheTTL    time.Duration

	mu    sync.RWMutex
	cache *cachedSecret
}

// newMultiSourceSecret creates a secret source with precedence and caching
func newMultiSourceSecret(explicitKey string, cacheTTL time.Duration) *multiSourceSecret {
	if cacheTTL == 0 {
		cacheTTL = 5 * time.Minute // Default 5 minute cache
	}

	home, _ := os.UserHomeDir()
	filePath := filepath.Join(home, ".local", "share", "amp", "secrets.json")

	return &multiSourceSecret{
		explicitKey: strings.TrimSpace(explicitKey),
		envKey:      "AMP_API_KEY",
		filePath:    filePath,
		cacheTTL:    cacheTTL,
	}
}

// newMultiSourceSecretWithPath creates a secret source with a custom file path (for testing)
func newMultiSourceSecretWithPath(explicitKey string, filePath string, cacheTTL time.Duration) *multiSourceSecret {
	if cacheTTL == 0 {
		cacheTTL = 5 * time.Minute
	}

	return &multiSourceSecret{
		explicitKey: strings.TrimSpace(explicitKey),
		envKey:      "AMP_API_KEY",
		filePath:    filePath,
		cacheTTL:    cacheTTL,
	}
}

// Get retrieves the Amp API key using precedence: config > env > file
// Results are cached for cacheTTL duration to avoid excessive file reads
func (s *multiSourceSecret) Get(_ context.Context) (string, error) {
	// Precedence 1: Explicit config key (highest priority, no caching needed)
	if s.explicitKey != "" {
		return s.explicitKey, nil
	}

	// Precedence 2: Environment variable
	if envValue := strings.TrimSpace(os.Getenv(s.envKey)); envValue != "" {
		return envValue, nil
	}

	// Precedence 3: File-based secret (lowest priority, cached)
	// Check cache first
	s.mu.RLock()
	if s.cache != nil && time.Now().Before(s.cache.expiresAt) {
		value := s.cache.value
		s.mu.RUnlock()
		return value, nil
	}
	s.mu.RUnlock()

	// Cache miss or expired - read from file
	key, err := s.readFromFile()
	if err != nil {
		// Cache empty result to avoid repeated file reads on missing files
		s.updateCache("")
		return "", err
	}

	// Cache the result
	s.updateCache(key)
	return key, nil
}

// readFromFile reads the Amp API key from the secrets file
func (s *multiSourceSecret) readFromFile() (string, error) {
	content, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // Missing file is not an error, just no key available
		}
		return "", fmt.Errorf("failed to read amp secrets from %s: %w", s.filePath, err)
	}

	var secrets map[string]string
	if err := json.Unmarshal(content, &secrets); err != nil {
		return "", fmt.Errorf("failed to parse amp secrets from %s: %w", s.filePath, err)
	}

	key := strings.TrimSpace(secrets["apiKey@https://ampcode.com/"])
	return key, nil
}

// updateCache updates the cached secret value
func (s *multiSourceSecret) updateCache(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = &cachedSecret{
		value:     value,
		expiresAt: time.Now().Add(s.cacheTTL),
	}
}

// invalidateCache clears the cached secret, forcing a fresh read on next Get
func (s *multiSourceSecret) invalidateCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = nil
}

// updateExplicitKey refreshes the config-provided key and clears cache.
func (s *multiSourceSecret) updateExplicitKey(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.explicitKey = strings.TrimSpace(key)
	s.cache = nil
	s.mu.Unlock()
}

// staticSecretSource returns a fixed API key (for testing)
type staticSecretSource struct {
	key string
}

// newStaticSecretSource creates a secret source with a fixed key
func newStaticSecretSource(key string) *staticSecretSource {
	return &staticSecretSource{key: strings.TrimSpace(key)}
}

// Get returns the static API key
func (s *staticSecretSource) Get(_ context.Context) (string, error) {
	return s.key, nil
}

// mappedSecretSource wraps a default SecretSource and adds per-client API key mapping.
// When a request context contains a client API key that matches a configured mapping,
// the corresponding upstream key is returned. Otherwise, falls back to the default source.
type mappedSecretSource struct {
	defaultSource secretSource
	mu            sync.RWMutex
	lookup        map[string]string // clientKey -> upstreamKey
}

// newMappedSecretSource creates a MappedSecretSource wrapping the given default source.
func newMappedSecretSource(defaultSource secretSource) *mappedSecretSource {
	return &mappedSecretSource{
		defaultSource: defaultSource,
		lookup:        make(map[string]string),
	}
}

// Get retrieves the Amp API key, checking per-client mappings first.
// If the request context contains a client API key that matches a configured mapping,
// returns the corresponding upstream key. Otherwise, falls back to the default source.
func (s *mappedSecretSource) Get(ctx context.Context) (string, error) {
	// Try to get client API key from request context
	clientKey := getClientAPIKeyFromContext(ctx)
	if clientKey != "" {
		s.mu.RLock()
		if upstreamKey, ok := s.lookup[clientKey]; ok && upstreamKey != "" {
			s.mu.RUnlock()
			return upstreamKey, nil
		}
		s.mu.RUnlock()
	}

	// Fall back to default source
	return s.defaultSource.Get(ctx)
}

// updateMappings rebuilds the client-to-upstream key mapping from configuration entries.
// If the same client key appears in multiple entries, logs a warning and uses the first one.
func (s *mappedSecretSource) updateMappings(entries []config.AmpUpstreamAPIKeyEntry) {
	newLookup := make(map[string]string)

	for _, entry := range entries {
		upstreamKey := strings.TrimSpace(entry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		for _, clientKey := range entry.APIKeys {
			trimmedKey := strings.TrimSpace(clientKey)
			if trimmedKey == "" {
				continue
			}
			if _, exists := newLookup[trimmedKey]; exists {
				// Log warning for duplicate client key, first one wins
				log.Warnf("amp upstream-api-keys: client API key appears in multiple entries; using first mapping.")
				continue
			}
			newLookup[trimmedKey] = upstreamKey
		}
	}

	s.mu.Lock()
	s.lookup = newLookup
	s.mu.Unlock()
}

// updateDefaultExplicitKey updates the explicit key on the underlying MultiSourceSecret (if applicable).
func (s *mappedSecretSource) updateDefaultExplicitKey(key string) {
	if ms, ok := s.defaultSource.(*multiSourceSecret); ok {
		ms.updateExplicitKey(key)
	}
}

// invalidateCache invalidates cache on the underlying MultiSourceSecret (if applicable).
func (s *mappedSecretSource) invalidateCache() {
	if ms, ok := s.defaultSource.(*multiSourceSecret); ok {
		ms.invalidateCache()
	}
}
