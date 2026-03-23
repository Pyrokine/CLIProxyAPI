package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
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
	if cfg.UsageRetention.MaxFileSizeMB != 0 && cfg.UsageRetention.MaxFileSizeMB < -1 {
		warnings = append(
			warnings, fmt.Sprintf(
				"usage-retention.max-file-size-mb %d is invalid (use -1 to disable or a positive integer)",
				cfg.UsageRetention.MaxFileSizeMB,
			),
		)
	}
	if cfg.UsageRetention.ArchiveMonths < -1 {
		warnings = append(
			warnings, fmt.Sprintf(
				"usage-retention.archive-months %d is invalid (use -1 to disable or a positive integer)",
				cfg.UsageRetention.ArchiveMonths,
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
