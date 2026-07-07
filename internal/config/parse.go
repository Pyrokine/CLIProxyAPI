package config

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// ParseConfigBytes parses a YAML configuration payload into Config and applies the same
// in-memory normalizations as LoadConfigOptional, without persisting any changes to disk.
func ParseConfigBytes(data []byte) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config payload is empty")
	}

	var cfg Config
	cfg.Host = "127.0.0.1"
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.DisableCooling = false
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.RemoteManagement.CPAGitHubRepository = defaultCPAGitHubRepository
	cfg.WebsocketAuth = true
	cfg.QuotaRefresh.Enabled = false
	cfg.QuotaRefresh.Interval = 600
	cfg.QuotaRefresh.MaxInterval = 1800
	cfg.AutoRefreshInterval = 3
	cfg.ModelRefreshInterval = 3

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config payload: %w", err)
	}

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

	cfg.SanitizeGeminiKeys()
	cfg.SanitizeVertexCompatKeys()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeCodexHeaderDefaults()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeOpenAICompatibility()
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)
	cfg.SanitizeOAuthModelAlias()
	cfg.SanitizeModelCatalog()
	cfg.sanitizePayloadRules()

	return &cfg, nil
}
