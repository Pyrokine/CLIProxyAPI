package config

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
)

// sanitizePayloadRules validates raw JSON payload rule params and drops invalid rules.
func (cfg *Config) sanitizePayloadRules() {
	if cfg == nil {
		return
	}
	cfg.Payload.DefaultRaw = sanitizePayloadRawRules(cfg.Payload.DefaultRaw, "default-raw")
	cfg.Payload.OverrideRaw = sanitizePayloadRawRules(cfg.Payload.OverrideRaw, "override-raw")
}

func sanitizePayloadRawRules(rules []PayloadRule, section string) []PayloadRule {
	if len(rules) == 0 {
		return rules
	}
	out := make([]PayloadRule, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if len(rule.Params) == 0 {
			continue
		}
		invalid := false
		for path, value := range rule.Params {
			raw, ok := payloadRawString(value)
			if !ok {
				continue
			}
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || !json.Valid(trimmed) {
				log.WithFields(
					log.Fields{
						"section":    section,
						"rule_index": i + 1,
						"param":      path,
					},
				).Warn("payload rule dropped: invalid raw JSON")
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func payloadRawString(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		return nil, false
	}
}

// SanitizeOAuthModelAlias normalizes and deduplicates global OAuth model name aliases.
// It trims whitespace, normalizes channel keys to lower-case, drops empty entries,
// allows multiple aliases per upstream name, and ensures aliases are unique within each channel.
func (cfg *Config) SanitizeOAuthModelAlias() {
	if cfg == nil || len(cfg.OAuthModelAlias) == 0 {
		return
	}
	cfg.OAuthModelAlias = SanitizeOAuthModelAliasMap(cfg.OAuthModelAlias)
}

func SanitizeOAuthModelAliasMap(entries map[string][]OAuthModelAlias) map[string][]OAuthModelAlias {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]OAuthModelAlias, len(entries))
	for rawChannel, aliases := range entries {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(aliases) == 0 {
			continue
		}
		seenAlias := make(map[string]struct{}, len(aliases))
		clean := make([]OAuthModelAlias, 0, len(aliases))
		for _, entry := range aliases {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, ok := seenAlias[aliasKey]; ok {
				continue
			}
			seenAlias[aliasKey] = struct{}{}
			clean = append(
				clean,
				OAuthModelAlias{
					Name:         name,
					Alias:        alias,
					Fork:         entry.Fork,
					DisplayName:  strings.TrimSpace(entry.DisplayName),
					ForceMapping: entry.ForceMapping,
				},
			)
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SanitizeOAuthRequestScopedErrors normalizes global OAuth request-scoped error rules.
func (cfg *Config) SanitizeOAuthRequestScopedErrors() {
	if cfg == nil || len(cfg.OAuthRequestScopedErrors) == 0 {
		return
	}
	out := make(map[string][]RequestScopedErrorRule, len(cfg.OAuthRequestScopedErrors))
	for rawChannel, rules := range cfg.OAuthRequestScopedErrors {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(rules) == 0 {
			continue
		}
		clean := make([]RequestScopedErrorRule, 0, len(rules))
		for _, rule := range rules {
			match := trimNonEmptyStrings(rule.Match)
			matchRegexr := trimNonEmptyStrings(rule.MatchRegexr)
			action := strings.ToLower(strings.TrimSpace(rule.Action))
			if rule.Status <= 0 || (len(match) == 0 && len(matchRegexr) == 0) || action == "" {
				continue
			}
			clean = append(clean, RequestScopedErrorRule{
				Status:      rule.Status,
				Match:       match,
				MatchRegexr: matchRegexr,
				Action:      action,
			})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	if len(out) == 0 {
		cfg.OAuthRequestScopedErrors = nil
		return
	}
	cfg.OAuthRequestScopedErrors = out
}

func trimNonEmptyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SanitizeOpenAICompatibility removes OpenAI-compatibility provider entries that are
// not actionable, specifically those missing a BaseURL. It trims whitespace before
// evaluation and preserves the relative order of remaining entries.
func (cfg *Config) SanitizeOpenAICompatibility() {
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return
	}
	out := make([]OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		e := cfg.OpenAICompatibility[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Prefix = NormalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		for j := range e.APIKeyEntries {
			e.APIKeyEntries[j].APIKey = strings.TrimSpace(e.APIKeyEntries[j].APIKey)
			e.APIKeyEntries[j].ProxyURL = strings.TrimSpace(e.APIKeyEntries[j].ProxyURL)
		}
		if e.BaseURL == "" {
			// Skip providers with no base-url; treated as removed
			continue
		}
		out = append(out, e)
	}
	cfg.OpenAICompatibility = out
}

// sanitizeCodexHeaderDefaults removes Codex API key entries missing a BaseURL.
// It trims whitespace and preserves order for remaining entries.
func (cfg *Config) sanitizeCodexHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.CodexHeaderDefaults.UserAgent = strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent)
	cfg.CodexHeaderDefaults.BetaFeatures = strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

func (cfg *Config) sanitizeClaudeHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.ClaudeHeaderDefaults.UserAgent = strings.TrimSpace(cfg.ClaudeHeaderDefaults.UserAgent)
	cfg.ClaudeHeaderDefaults.PackageVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.PackageVersion)
	cfg.ClaudeHeaderDefaults.RuntimeVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.RuntimeVersion)
	cfg.ClaudeHeaderDefaults.OS = strings.TrimSpace(cfg.ClaudeHeaderDefaults.OS)
	cfg.ClaudeHeaderDefaults.Arch = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Arch)
	cfg.ClaudeHeaderDefaults.Timeout = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timeout)
	cfg.ClaudeHeaderDefaults.Timezone = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timezone)
}

func (cfg *Config) SanitizeCodexKeys() {
	if cfg == nil || len(cfg.CodexKey) == 0 {
		return
	}
	out := make([]CodexKey, 0, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		e := cfg.CodexKey[i]
		e.Prefix = NormalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		e.ExcludedModels = NormalizeExcludedModels(e.ExcludedModels)
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	cfg.CodexKey = out
}

// SanitizeXAIKeys normalizes xAI API key entries and removes unusable entries.
func (cfg *Config) SanitizeXAIKeys() {
	if cfg == nil || len(cfg.XAIKey) == 0 {
		return
	}
	out := make([]XAIKey, 0, len(cfg.XAIKey))
	for i := range cfg.XAIKey {
		e := cfg.XAIKey[i]
		e.APIKey = strings.TrimSpace(e.APIKey)
		e.AlphaSearch = false
		e.Prefix = NormalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.ProxyURL = strings.TrimSpace(e.ProxyURL)
		e.Headers = NormalizeHeaders(e.Headers)
		e.ExcludedModels = NormalizeExcludedModels(e.ExcludedModels)
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	cfg.XAIKey = out
}

// SanitizeClaudeKeys normalizes headers for Claude credentials.
func (cfg *Config) SanitizeClaudeKeys() {
	if cfg == nil || len(cfg.ClaudeKey) == 0 {
		return
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		if profile, ok := NormalizeClaudeFingerprintProfile(entry.FingerprintProfile); ok {
			entry.FingerprintProfile = profile
		}
		entry.Prefix = NormalizeModelPrefix(entry.Prefix)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
	}
}

func sanitizeGeminiKeyEntries(entries []GeminiKey) []GeminiKey {
	seen := make(map[string]struct{}, len(entries))
	out := entries[:0]
	for i := range entries {
		entry := entries[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		entry.Prefix = NormalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		if entry.APIKey == "" && entry.BaseURL == "" {
			continue
		}
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		uniqueKey := formatGeminiKeyDedupID(entry)
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	return out
}

func formatGeminiKeyDedupID(entry GeminiKey) string {
	var b strings.Builder
	b.WriteString(entry.APIKey)
	b.WriteByte(0)
	b.WriteString(entry.BaseURL)
	b.WriteByte(0)
	b.WriteString(entry.ProxyURL)
	b.WriteByte(0)
	b.WriteString(entry.Prefix)
	b.WriteByte(0)
	b.WriteString(FormatSortedHeaders(entry.Headers))
	return b.String()
}

// FormatSortedHeaders serializes headers deterministically with null byte separators.
func FormatSortedHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteByte(0)
		b.WriteString(headers[key])
		b.WriteByte(0)
	}
	return b.String()
}

// SanitizeGeminiKeys deduplicates and normalizes Gemini credentials.
// It uses API key, base URL, proxy URL, prefix, and custom headers as the uniqueness key.
func (cfg *Config) SanitizeGeminiKeys() {
	if cfg == nil {
		return
	}
	cfg.GeminiKey = sanitizeGeminiKeyEntries(cfg.GeminiKey)
}

// SanitizeInteractionsKeys deduplicates and normalizes native Interactions credentials.
// It uses API key, base URL, proxy URL, prefix, and custom headers as the uniqueness key.
func (cfg *Config) SanitizeInteractionsKeys() {
	if cfg == nil {
		return
	}
	cfg.InteractionsKey = sanitizeGeminiKeyEntries(cfg.InteractionsKey)
}

// NormalizeModelPrefix trims whitespace and surrounding slashes from a model route prefix.
func NormalizeModelPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// NormalizeHeaders trims header keys and values and removes empty pairs.
func NormalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// NormalizeExcludedModels trims, lowercases, and deduplicates model exclusion patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeOAuthExcludedModels cleans provider -> excluded models mappings by normalizing provider keys
// and applying model exclusion normalization to each entry.
func NormalizeOAuthExcludedModels(entries map[string][]string) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for provider, models := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		normalized := NormalizeExcludedModels(models)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
