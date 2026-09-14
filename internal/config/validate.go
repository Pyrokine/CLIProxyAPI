package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// ValidateConfig checks the configuration for common issues and returns a list of warnings.
// It does not prevent the server from starting, but helps users catch problems early.
func ValidateConfig(cfg *Config) []string {
	if cfg == nil {
		return []string{"configuration is nil"}
	}

	var warnings []string

	// Port
	if cfg.Port < 0 || cfg.Port > 65535 {
		warnings = append(warnings, fmt.Sprintf("port %d is out of range [0, 65535]", cfg.Port))
	}
	if cfg.Port == 0 {
		warnings = append(warnings, "port is 0, server will not listen on any port")
	}

	// API Keys: presence + format + duplicates
	if len(cfg.APIKeys) == 0 {
		warnings = append(
			warnings,
			"no api-keys configured: all API requests will be unauthenticated (add api-keys or set host to 127.0.0.1 to limit exposure)",
		)
	}
	{
		seen := make(map[string]struct{}, len(cfg.APIKeys))
		for i, key := range cfg.APIKeys {
			trimmed := strings.TrimSpace(key)
			if trimmed == "" {
				warnings = append(warnings, fmt.Sprintf("api-keys[%d] is empty", i))
				continue
			}
			if len(trimmed) < 8 {
				warnings = append(
					warnings, fmt.Sprintf("api-keys[%d] is too short (%d chars, minimum 8)", i, len(trimmed)),
				)
			}
			for _, b := range []byte(trimmed) {
				if b < 0x21 || b > 0x7E {
					warnings = append(warnings, fmt.Sprintf("api-keys[%d] contains non-ASCII-printable characters", i))
					break
				}
			}
			if _, dup := seen[trimmed]; dup {
				warnings = append(warnings, fmt.Sprintf("api-keys[%d] is a duplicate", i))
			}
			seen[trimmed] = struct{}{}
		}
	}

	// Gemini Keys
	for i, key := range cfg.GeminiKey {
		if strings.TrimSpace(key.APIKey) == "" {
			warnings = append(warnings, fmt.Sprintf("gemini-api-key[%d] has empty api-key", i))
		}
	}

	// Claude Keys
	for i, key := range cfg.ClaudeKey {
		if strings.TrimSpace(key.APIKey) == "" {
			warnings = append(warnings, fmt.Sprintf("claude-api-key[%d] has empty api-key", i))
		}
	}

	// Codex Keys
	for i, key := range cfg.CodexKey {
		if strings.TrimSpace(key.BaseURL) == "" {
			warnings = append(warnings, fmt.Sprintf("codex-api-key[%d] has empty base-url", i))
		}
	}

	// xAI Keys
	for i, key := range cfg.XAIKey {
		if strings.TrimSpace(key.APIKey) == "" {
			warnings = append(warnings, fmt.Sprintf("xai-api-key[%d] has empty api-key", i))
		}
		if strings.TrimSpace(key.BaseURL) == "" {
			warnings = append(warnings, fmt.Sprintf("xai-api-key[%d] has empty base-url", i))
		}
	}

	// OpenAI Compatibility
	for i, entry := range cfg.OpenAICompatibility {
		if strings.TrimSpace(entry.Name) == "" {
			warnings = append(warnings, fmt.Sprintf("openai-compatibility[%d] has empty name", i))
		}
		if strings.TrimSpace(entry.BaseURL) == "" {
			warnings = append(warnings, fmt.Sprintf("openai-compatibility[%d] has empty base-url", i))
		}
	}

	// Vertex Compat Keys
	for i, key := range cfg.VertexCompatAPIKey {
		if strings.TrimSpace(key.APIKey) == "" {
			warnings = append(warnings, fmt.Sprintf("vertex-api-key[%d] has empty api-key", i))
		}
		if strings.TrimSpace(key.BaseURL) == "" {
			warnings = append(warnings, fmt.Sprintf("vertex-api-key[%d] has empty base-url", i))
		}
	}

	// Request retry
	if cfg.RequestRetry < 0 || cfg.RequestRetry > 10 {
		warnings = append(
			warnings, fmt.Sprintf("request-retry %d is out of recommended range [0, 10]", cfg.RequestRetry),
		)
	}

	// Logs max total size
	if cfg.LogsMaxTotalSizeMB < 0 {
		warnings = append(warnings, fmt.Sprintf("logs-max-total-size-mb %d is negative", cfg.LogsMaxTotalSizeMB))
	}

	if cfg.AutoRefreshInterval < 0 {
		warnings = append(warnings, fmt.Sprintf("auto-refresh-interval %d is negative", cfg.AutoRefreshInterval))
	}
	if cfg.ModelRefreshInterval < 0 {
		warnings = append(warnings, fmt.Sprintf("model-refresh-interval %d is negative", cfg.ModelRefreshInterval))
	}

	for i, origin := range cfg.CORSAllowedOrigins {
		trimmed := strings.TrimSpace(origin)
		if trimmed == "" {
			warnings = append(warnings, fmt.Sprintf("cors-allowed-origins[%d] is empty", i))
			continue
		}
		if trimmed == "*" {
			continue
		}
		parsed, errParseOrigin := url.Parse(trimmed)
		if errParseOrigin != nil || parsed.Scheme == "" || parsed.Host == "" {
			warnings = append(warnings, fmt.Sprintf("cors-allowed-origins[%d] %q is not a valid origin", i, origin))
		}
	}

	if cfg.QuotaRefresh.Interval < 0 {
		warnings = append(warnings, fmt.Sprintf("quota-refresh.interval %d is negative", cfg.QuotaRefresh.Interval))
	}
	if cfg.QuotaRefresh.MaxInterval < 0 {
		warnings = append(
			warnings, fmt.Sprintf("quota-refresh.max-interval %d is negative", cfg.QuotaRefresh.MaxInterval),
		)
	}
	if cfg.QuotaRefresh.Enabled {
		if cfg.QuotaRefresh.Interval < 60 {
			warnings = append(
				warnings,
				fmt.Sprintf("quota-refresh.interval %d is below minimum 60 seconds", cfg.QuotaRefresh.Interval),
			)
		}
		if cfg.QuotaRefresh.MaxInterval < 300 {
			warnings = append(
				warnings,
				fmt.Sprintf("quota-refresh.max-interval %d is below minimum 300 seconds", cfg.QuotaRefresh.MaxInterval),
			)
		}
		if cfg.QuotaRefresh.Interval > 0 && cfg.QuotaRefresh.MaxInterval > 0 && cfg.QuotaRefresh.MaxInterval < cfg.QuotaRefresh.Interval {
			warnings = append(warnings, "quota-refresh.max-interval is smaller than quota-refresh.interval")
		}
	}

	switch strings.TrimSpace(cfg.Routing.Strategy) {
	case "", "round-robin", "weighted-round-robin", "quota-aware", "fill-first":
	default:
		warnings = append(
			warnings,
			fmt.Sprintf(
				"routing.strategy %q is invalid (use round-robin, weighted-round-robin, quota-aware, or fill-first)",
				cfg.Routing.Strategy,
			),
		)
	}
	if ttl := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); ttl != "" {
		parsedTTL, errParseTTL := time.ParseDuration(ttl)
		if errParseTTL != nil || parsedTTL <= 0 {
			warnings = append(
				warnings, fmt.Sprintf(
					"routing.session-affinity-ttl %q is not a positive duration", cfg.Routing.SessionAffinityTTL,
				),
			)
		}
	}

	pluginConfigIDs := make(map[string]string, len(cfg.Plugins.Configs))
	for id := range cfg.Plugins.Configs {
		if !validPluginConfigID(id) {
			warnings = append(warnings, fmt.Sprintf("plugins.configs contains invalid plugin id %q", id))
		}
		canonicalID := canonicalPluginConfigID(id)
		if existingID, duplicate := pluginConfigIDs[canonicalID]; duplicate {
			warnings = append(
				warnings,
				fmt.Sprintf("plugins.configs contains duplicate plugin ids %q and %q", existingID, id),
			)
			continue
		}
		pluginConfigIDs[canonicalID] = id
	}

	// TLS
	if cfg.TLS.Enable {
		if strings.TrimSpace(cfg.TLS.Cert) == "" {
			warnings = append(warnings, "tls is enabled but cert is empty")
		} else if _, err := os.Stat(cfg.TLS.Cert); err != nil {
			warnings = append(warnings, fmt.Sprintf("tls cert file does not exist: %s", cfg.TLS.Cert))
		}
		if strings.TrimSpace(cfg.TLS.Key) == "" {
			warnings = append(warnings, "tls is enabled but key is empty")
		} else if _, err := os.Stat(cfg.TLS.Key); err != nil {
			warnings = append(warnings, fmt.Sprintf("tls key file does not exist: %s", cfg.TLS.Key))
		}
	}

	// Usage retention
	if cfg.UsageRetention.Days < -1 {
		warnings = append(
			warnings, fmt.Sprintf(
				"usage-retention.days %d is invalid (use -1 to disable or a positive integer)", cfg.UsageRetention.Days,
			),
		)
	}
	if cfg.UsageRetention.MaxDBSizeMB < 0 {
		warnings = append(
			warnings, fmt.Sprintf(
				"usage-retention.max-db-size-mb %d is invalid (use 0 to disable or a positive integer)",
				cfg.UsageRetention.MaxDBSizeMB,
			),
		)
	}
	if cfg.UsageRetention.WarningThresholdPct < 0 || cfg.UsageRetention.WarningThresholdPct > 100 {
		warnings = append(
			warnings, fmt.Sprintf(
				"usage-retention.warning-threshold-pct %d is invalid (use 0 for default or 1-100)",
				cfg.UsageRetention.WarningThresholdPct,
			),
		)
	}

	// Image artifact cache
	if cfg.ImageArtifactCache.RetentionDays < 0 {
		warnings = append(
			warnings, fmt.Sprintf(
				"image-artifact-cache.retention-days %d is invalid (use 0 to disable or a positive integer)",
				cfg.ImageArtifactCache.RetentionDays,
			),
		)
	}
	if cfg.ImageArtifactCache.MaxTotalSizeMB < 0 {
		warnings = append(
			warnings, fmt.Sprintf(
				"image-artifact-cache.max-total-size-mb %d is invalid (use 0 to disable or a positive integer)",
				cfg.ImageArtifactCache.MaxTotalSizeMB,
			),
		)
	}

	// GitHub repository URLs
	if repo := strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository); repo != "" {
		if !isValidGitHubRepoURL(repo) {
			warnings = append(warnings, fmt.Sprintf("panel-github-repository %q is not a valid GitHub URL", repo))
		}
	}
	if repo := strings.TrimSpace(cfg.RemoteManagement.CPAGitHubRepository); repo != "" {
		if !isValidGitHubRepoURL(repo) {
			warnings = append(warnings, fmt.Sprintf("cpa-github-repository %q is not a valid GitHub URL", repo))
		}
	}

	warnings = append(warnings, validateAPIKeyRules(cfg)...)

	// API Key Aliases: reference integrity + duplicate values
	if len(cfg.APIKeyAliases) > 0 {
		apiKeySet := make(map[string]struct{}, len(cfg.APIKeys))
		for _, key := range cfg.APIKeys {
			apiKeySet[key] = struct{}{}
		}
		aliasValues := make(map[string]bool, len(cfg.APIKeyAliases))
		for key, alias := range cfg.APIKeyAliases {
			if _, exists := apiKeySet[key]; !exists {
				warnings = append(
					warnings, fmt.Sprintf("api-key-aliases: alias %q references non-existent api key", alias),
				)
			}
			if aliasValues[alias] {
				warnings = append(warnings, fmt.Sprintf("api-key-aliases: duplicate alias value %q", alias))
			}
			aliasValues[alias] = true
		}
	}

	return warnings
}

// isValidGitHubRepoURL checks if the string is a valid GitHub repository URL or owner/repo format.
func validateAPIKeyRules(cfg *Config) []string {
	if cfg == nil || len(cfg.APIKeyRules) == 0 {
		return nil
	}
	configuredKeys := make(map[string]struct{}, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		configuredKeys[strings.TrimSpace(key)] = struct{}{}
	}
	warnings := make([]string, 0)
	for apiKey, rule := range cfg.APIKeyRules {
		trimmedKey := strings.TrimSpace(apiKey)
		if trimmedKey == "" {
			warnings = append(warnings, "api-key-rules contains an empty API key")
		} else if _, exists := configuredKeys[trimmedKey]; !exists {
			warnings = append(warnings, "api-key-rules contains a rule for an API key not present in api-keys")
		}
		for index, provider := range rule.Providers {
			if strings.TrimSpace(provider) == "" {
				warnings = append(warnings, fmt.Sprintf("api-key-rules[%q].providers[%d] is empty", apiKey, index))
			}
		}
		mappingKeys := make(map[string]string, len(rule.ModelMappings))
		for source, target := range rule.ModelMappings {
			normalizedSource := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(source, "models/")))
			if normalizedSource == "" || strings.TrimSpace(target) == "" {
				warnings = append(
					warnings, fmt.Sprintf("api-key-rules[%q].model-mappings contains an empty model", apiKey),
				)
				continue
			}
			if previous, duplicate := mappingKeys[normalizedSource]; duplicate {
				warnings = append(
					warnings,
					fmt.Sprintf(
						"api-key-rules[%q].model-mappings contains duplicate sources %q and %q",
						apiKey,
						previous,
						source,
					),
				)
			}
			mappingKeys[normalizedSource] = source
		}
		profile := strings.TrimSpace(rule.PolicyProfile)
		if profile != "" && !validAPIKeyPolicyProfile(profile) {
			warnings = append(
				warnings, fmt.Sprintf("api-key-rules[%q].policy-profile %q is invalid", apiKey, rule.PolicyProfile),
			)
		}
		if ttl := strings.TrimSpace(rule.ReservationTTL); ttl != "" {
			parsedTTL, err := time.ParseDuration(ttl)
			if err != nil || parsedTTL <= 0 {
				warnings = append(
					warnings, fmt.Sprintf(
						"api-key-rules[%q].reservation-ttl %q is not a positive duration", apiKey, rule.ReservationTTL,
					),
				)
			}
		}
		windows := make(map[time.Duration]struct{}, len(rule.Limits))
		for index, limit := range rule.Limits {
			window, err := time.ParseDuration(strings.TrimSpace(limit.Window))
			if err != nil || window <= 0 {
				warnings = append(
					warnings, fmt.Sprintf(
						"api-key-rules[%q].limits[%d].window %q is not a positive duration", apiKey, index, limit.Window,
					),
				)
			} else if _, duplicate := windows[window]; duplicate {
				warnings = append(
					warnings, fmt.Sprintf("api-key-rules[%q].limits contains duplicate window %q", apiKey, limit.Window),
				)
			} else {
				windows[window] = struct{}{}
			}
			if limit.Requests < 0 || limit.Tokens < 0 || limit.CostMicro < 0 {
				warnings = append(
					warnings, fmt.Sprintf("api-key-rules[%q].limits[%d] contains a negative limit", apiKey, index),
				)
			}
			if limit.Requests == 0 && limit.Tokens == 0 && limit.CostMicro == 0 {
				warnings = append(
					warnings, fmt.Sprintf("api-key-rules[%q].limits[%d] has no enabled dimension", apiKey, index),
				)
			}
		}
	}
	return warnings
}

func validAPIKeyPolicyProfile(profile string) bool {
	if len(profile) == 0 || len(profile) > 128 {
		return false
	}
	for index := range len(profile) {
		ch := profile[index]
		if !isASCIILetterDigit(ch) && ch != '.' && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

func isValidGitHubRepoURL(s string) bool {
	// Accept "owner/repo" format
	if !strings.Contains(s, "://") {
		parts := strings.Split(s, "/")
		return len(parts) == 2 && parts[0] != "" && parts[1] != ""
	}
	// Accept full GitHub URL
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Host != "github.com" && u.Host != "www.github.com" {
		return false
	}
	trimmed := strings.Trim(u.Path, "/")
	parts := strings.Split(trimmed, "/")
	return len(parts) >= 2 && parts[0] != "" && parts[1] != ""
}

func validPluginConfigID(id string) bool {
	if len(id) == 0 || len(id) > 128 || !isASCIILetterDigit(id[0]) {
		return false
	}
	for i := 1; i < len(id); i++ {
		if !isASCIILetterDigit(id[i]) && id[i] != '.' && id[i] != '_' && id[i] != '-' {
			return false
		}
	}
	return true
}

func isASCIILetterDigit(ch byte) bool {
	return (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
}
