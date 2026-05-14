package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads a YAML configuration file from the given path,
// unmarshals it into a Config struct, applies environment variable overrides,
// and returns it.
//
// Parameters:
//   - configFile: The path to the YAML configuration file
//
// Returns:
//   - *Config: The loaded configuration
//   - error: An error if the configuration could not be loaded
func LoadConfig(configFile string) (*Config, error) {
	return LoadConfigOptional(configFile, false)
}

func mergeConfigAPIKeys(rawYAML []byte, current []string) []string {
	keys := append([]string(nil), current...)
	var parsed struct {
		Auth struct {
			Providers map[string]struct {
				APIKeys []string `yaml:"api-keys"`
				Entries []struct {
					APIKey string `yaml:"api-key"`
				} `yaml:"api-key-entries"`
			} `yaml:"providers"`
		} `yaml:"auth"`
	}
	if err := yaml.Unmarshal(rawYAML, &parsed); err != nil {
		return normalizeStringSlice(keys)
	}
	provider, ok := parsed.Auth.Providers["config-api-key"]
	if !ok {
		return normalizeStringSlice(keys)
	}
	for _, key := range provider.APIKeys {
		keys = append(keys, key)
	}
	for _, entry := range provider.Entries {
		keys = append(keys, entry.APIKey)
	}
	return normalizeStringSlice(keys)
}

func normalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// LoadConfigOptional reads YAML from configFile.
// If optional is true and the file is missing, it returns an empty Config.
// If optional is true and the file is empty or invalid, it returns an empty Config.
func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	// Read the entire configuration file into memory.
	data, err := os.ReadFile(configFile)
	if err != nil {
		if optional {
			if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
				// Missing and optional: return empty config (cloud deploy standby).
				return &Config{}, nil
			}
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// In cloud deploy mode (optional=true), if file is empty or contains only whitespace, return empty config.
	if optional && len(data) == 0 {
		return &Config{}, nil
	}

	// Unmarshal the YAML data into the Config struct.
	var cfg Config
	// Set defaults before unmarshal so that absent keys keep defaults.
	cfg.Host = "127.0.0.1" // Default to loopback only; explicit config required for network exposure
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.DisableCooling = false
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.AmpCode.RestrictManagementToLocalhost = false // Default to false: API key auth is sufficient
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.RemoteManagement.CPAGitHubRepository = defaultCPAGitHubRepository
	cfg.WebsocketAuth = true // Default to true: require authentication for WebSocket connections
	// Quota refresh defaults (must match quota.DefaultConfig).
	// Enabled defaults to false (R-095): auto-polling provider endpoints risks triggering anti-abuse detection.
	// Frontend shows a confirmation dialog when user opts in (GlobalSettings.tsx:67).
	cfg.QuotaRefresh.Enabled = false
	cfg.QuotaRefresh.Interval = 600     // 10 minutes
	cfg.QuotaRefresh.MaxInterval = 1800 // 30 minutes
	// NOTE: UsageRetention zero-values are handled by persister.NewPersister (0 → default, -1 → disabled).
	// AutoRefreshInterval (default 3) and ModelRefreshInterval (default 3) use 0 as "disabled",
	// so their defaults must be set here rather than relying on zero-value semantics.
	cfg.AutoRefreshInterval = 3  // 3 seconds
	cfg.ModelRefreshInterval = 3 // 3 hours
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		if optional {
			// In cloud deploy mode, if YAML parsing fails, return empty config instead of error.
			return &Config{}, nil
		}
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	cfg.APIKeys = mergeConfigAPIKeys(data, cfg.APIKeys)

	// NOTE: Startup legacy key migration is intentionally disabled.
	// Reason: avoid mutating config.yaml during server startup.
	// Re-enable the block below if automatic startup migration is needed again.
	// var legacy legacyConfigData
	// if errLegacy := yaml.Unmarshal(data, &legacy); errLegacy == nil {
	// 	if cfg.migrateLegacyGeminiKeys(legacy.LegacyGeminiKeys) {
	// 		cfg.legacyMigrationPending = true
	// 	}
	// 	if cfg.migrateLegacyOpenAICompatibilityKeys(legacy.OpenAICompat) {
	// 		cfg.legacyMigrationPending = true
	// 	}
	// 	if cfg.migrateLegacyAmpConfig(&legacy) {
	// 		cfg.legacyMigrationPending = true
	// 	}
	// }

	// Hash remote management key if plaintext is detected (nested)
	// We consider a value to be already hashed if it looks like a bcrypt hash ($2a$, $2b$, or $2y$ prefix).
	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		hashed, errHash := hashSecret(cfg.RemoteManagement.SecretKey)
		if errHash != nil {
			return nil, fmt.Errorf("failed to hash remote management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = hashed

		// Persist the hashed value back to the config file to avoid re-hashing on next startup.
		// Preserve YAML comments and ordering; update only the nested key.
		_ = SaveConfigPreserveCommentsUpdateNestedScalar(
			configFile, []string{"remote-management", "secret-key"}, hashed,
		)
	}

	cfg.RemoteManagement.PanelGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}

	cfg.RemoteManagement.CPAGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.CPAGitHubRepository)
	if cfg.RemoteManagement.CPAGitHubRepository == "" {
		cfg.RemoteManagement.CPAGitHubRepository = defaultCPAGitHubRepository
	}

	cfg.Pprof.Addr = strings.TrimSpace(cfg.Pprof.Addr)
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}

	if cfg.LogsMaxTotalSizeMB < 0 {
		cfg.LogsMaxTotalSizeMB = 0
	}

	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 10
	}

	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	// Sanitize Gemini API key configuration and migrate legacy entries.
	cfg.SanitizeGeminiKeys()

	// Sanitize Vertex-compatible API keys: drop entries without base-url
	cfg.SanitizeVertexCompatKeys()

	// Sanitize Codex keys: drop entries without base-url
	cfg.SanitizeCodexKeys()

	// Sanitize Claude key headers
	cfg.SanitizeClaudeKeys()

	// Sanitize OpenAI compatibility providers: drop entries without base-url
	cfg.SanitizeOpenAICompatibility()

	// Normalize OAuth provider model exclusion map.
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)

	// Normalize global OAuth model name aliases.
	cfg.SanitizeOAuthModelAlias()

	// Validate raw payload rules and drop invalid entries.
	cfg.sanitizePayloadRules()

	// NOTE: Legacy migration persistence is intentionally disabled together with
	// startup legacy migration to keep startup read-only for config.yaml.
	// Re-enable the block below if automatic startup migration is needed again.
	// if cfg.legacyMigrationPending {
	// 	fmt.Println("Detected legacy configuration keys, attempting to persist the normalized config...")
	// 	if !optional && configFile != "" {
	// 		if err := SaveConfigPreserveComments(configFile, &cfg); err != nil {
	// 			return nil, fmt.Errorf("failed to persist migrated legacy config: %w", err)
	// 		}
	// 		fmt.Println("Legacy configuration normalized and persisted.")
	// 	} else {
	// 		fmt.Println("Legacy configuration normalized in memory; persistence skipped.")
	// 	}
	// }

	// Return the populated configuration struct.
	return &cfg, nil
}

// LastGoodPath returns the path to the last-known-good config backup.
func LastGoodPath(configFilePath string) string {
	return configFilePath + ".last-good"
}

// SaveLastGood copies the current config file to config.yaml.last-good.
func SaveLastGood(configFilePath string) error {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return err
	}
	return os.WriteFile(LastGoodPath(configFilePath), data, 0o600)
}

// LoadWithFallback tries to load configFilePath; on failure, falls back to config.yaml.last-good.
// Returns (config, actualPath, fallback, error).
// fallback is true when the last-good file was used instead of the primary config.
func LoadWithFallback(configFilePath string) (*Config, string, bool, error) {
	cfg, err := LoadConfig(configFilePath)
	if err == nil {
		// Primary config loaded successfully — save as last-good
		_ = SaveLastGood(configFilePath)
		return cfg, configFilePath, false, nil
	}

	// Primary config failed — try last-good
	lastGood := LastGoodPath(configFilePath)
	if _, statErr := os.Stat(lastGood); statErr != nil {
		// No last-good backup available
		return nil, configFilePath, false, fmt.Errorf("config load failed and no last-good backup: %w", err)
	}

	cfgFallback, errFallback := LoadConfig(lastGood)
	if errFallback != nil {
		return nil, configFilePath, false, fmt.Errorf(
			"config load failed (primary: %v, last-good: %v)", err, errFallback,
		)
	}

	return cfgFallback, configFilePath, true, nil
}

// looksLikeBcrypt returns true if the provided string appears to be a bcrypt hash.
func looksLikeBcrypt(s string) bool {
	return len(s) > 4 && (s[:4] == "$2a$" || s[:4] == "$2b$" || s[:4] == "$2y$")
}

// hashSecret hashes the given secret using bcrypt.
func hashSecret(secret string) (string, error) {
	// Use default cost for simplicity.
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashedBytes), nil
}
