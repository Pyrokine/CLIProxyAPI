package config

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// ParseConfigBytes parses a YAML configuration payload into Config and applies the same
// in-memory normalizations as LoadConfigOptional, without persisting any changes to disk.
func ParseConfigBytes(data []byte) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config payload is empty")
	}

	if errValidate := validateManagementSecretYAML(data); errValidate != nil {
		return nil, fmt.Errorf("validate config payload: %w", errValidate)
	}
	if errValidate := validateCredentialWeightYAML(data); errValidate != nil {
		return nil, fmt.Errorf("validate config payload: %w", errValidate)
	}
	if errValidate := validateTransientErrorCooldownYAML(data); errValidate != nil {
		return nil, fmt.Errorf("validate config payload: %w", errValidate)
	}

	var cfg Config
	cfg.Host = "127.0.0.1"
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.ImageArtifactCache.RetentionDays = 7
	cfg.ImageArtifactCache.MaxTotalSizeMB = 10240
	cfg.DisableCooling = false
	cfg.SaveCooldownStatus = false
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.RemoteManagement.CPAGitHubRepository = defaultCPAGitHubRepository
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	cfg.WebsocketAuth = true
	cfg.QuotaRefresh.Enabled = false
	cfg.QuotaRefresh.Interval = 600
	cfg.QuotaRefresh.MaxInterval = 1800
	cfg.AutoRefreshInterval = 3
	cfg.ModelRefreshInterval = 3

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config payload: %w", err)
	}
	cfg.CredentialConcurrency = cfg.CredentialConcurrency.WithDefaults()
	if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := cfg.Codex.LiveMediaRelay.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := cfg.ValidateCredentialWeights(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := validateXAIKeys(cfg.XAIKey); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := validateMetaKeys(cfg.MetaKey); errValidate != nil {
		return nil, errValidate
	}
	if err := validateImageArtifactCacheConfig(cfg.ImageArtifactCache); err != nil {
		return nil, err
	}
	cfg.APIKeys = mergeConfigAPIKeys(data, cfg.APIKeys)

	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		hashed, errHash := bcrypt.GenerateFromPassword([]byte(cfg.RemoteManagement.SecretKey), bcrypt.DefaultCost)
		if errHash != nil {
			return nil, fmt.Errorf("hash remote management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = string(hashed)
	}

	cfg.RemoteManagement.PanelGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}
	cfg.RemoteManagement.CPAGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.CPAGitHubRepository)
	if cfg.RemoteManagement.CPAGitHubRepository == "" {
		cfg.RemoteManagement.CPAGitHubRepository = defaultCPAGitHubRepository
	}
	trustedProxies, errTrustedProxies := normalizeTrustedProxies(cfg.RemoteManagement.TrustedProxies)
	if errTrustedProxies != nil {
		return nil, errTrustedProxies
	}
	cfg.RemoteManagement.TrustedProxies = trustedProxies

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
	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
	} else if cfg.RedisUsageQueueRetentionSeconds > 3600 {
		cfg.RedisUsageQueueRetentionSeconds = 3600
	}
	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	cfg.NormalizePluginsConfig()
	if errResolvePluginsDir := cfg.ResolvePluginsDir(); errResolvePluginsDir != nil && cfg.Plugins.Enabled {
		return nil, errResolvePluginsDir
	}

	cfg.SanitizeGeminiKeys()
	cfg.SanitizeInteractionsKeys()
	cfg.SanitizeVertexCompatKeys()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeXAIKeys()
	cfg.SanitizeMetaKeys()
	cfg.sanitizeCodexHeaderDefaults()
	cfg.sanitizeClaudeHeaderDefaults()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeOpenAICompatibility()
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)
	cfg.SanitizeOAuthModelAlias()
	cfg.SanitizeOAuthSettings()
	cfg.SanitizeOAuthRequestScopedErrors()
	cfg.sanitizeModelCatalog()
	cfg.sanitizePayloadRules()

	return &cfg, nil
}

func validateXAIKeys(entries []XAIKey) error {
	for i := range entries {
		entry := entries[i]
		if strings.TrimSpace(entry.APIKey) == "" {
			return fmt.Errorf("xai-api-key[%d].api-key is empty", i)
		}
		baseURL := strings.TrimSpace(entry.BaseURL)
		if baseURL == "" {
			return fmt.Errorf("xai-api-key[%d].base-url is empty", i)
		}
		if errValidate := proxyutil.ValidateHTTPURL(baseURL); errValidate != nil {
			return fmt.Errorf("xai-api-key[%d].base-url: %w", i, errValidate)
		}
		if proxyURL := strings.TrimSpace(entry.ProxyURL); proxyURL != "" {
			if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
				return fmt.Errorf("xai-api-key[%d].proxy-url: %w", i, errValidate)
			}
		}
		if errValidate := validateXAIHeaders(entry.Headers); errValidate != nil {
			return fmt.Errorf("xai-api-key[%d].headers: %w", i, errValidate)
		}
	}
	return nil
}

func validateMetaKeys(entries []MetaKey) error {
	for i := range entries {
		entry := entries[i]
		apiKey := strings.TrimSpace(entry.APIKey)
		if apiKey == "" || strings.HasPrefix(strings.ToLower(apiKey), "dca:") {
			return fmt.Errorf("meta-api-key[%d].api-key requires a Meta API key; DCA tokens require OAuth storage", i)
		}
		if baseURL := strings.TrimSpace(entry.BaseURL); baseURL != "" {
			if err := proxyutil.ValidateHTTPURL(baseURL); err != nil {
				return fmt.Errorf("meta-api-key[%d].base-url: %w", i, err)
			}
		}
		if proxyURL := strings.TrimSpace(entry.ProxyURL); proxyURL != "" {
			if err := proxyutil.ValidateProxyURL(proxyURL); err != nil {
				return fmt.Errorf("meta-api-key[%d].proxy-url: %w", i, err)
			}
		}
		if err := validateXAIHeaders(entry.Headers); err != nil {
			return fmt.Errorf("meta-api-key[%d].headers: %w", i, err)
		}
	}
	return nil
}

func validateXAIHeaders(headers map[string]string) error {
	for name, value := range headers {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		if proxyutil.IsRestrictedCustomHeader(name) {
			return fmt.Errorf("custom header %q is restricted", name)
		}
	}
	return nil
}
