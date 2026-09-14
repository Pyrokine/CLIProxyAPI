// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"errors"
	"sort"
	"strings"
)

// APIKeyPolicyLimit defines one persistent hard-limit window for an API key.
type APIKeyPolicyLimit struct {
	Window    string `yaml:"window" json:"window"`
	Requests  int64  `yaml:"requests,omitempty" json:"requests,omitempty"`
	Tokens    int64  `yaml:"tokens,omitempty" json:"tokens,omitempty"`
	CostMicro int64  `yaml:"cost-micro,omitempty" json:"cost-micro,omitempty"`
}

// APIKeyRule defines model access and persistent usage policy for one API key.
type APIKeyRule struct {
	BlockedModels   []string            `yaml:"blocked-models,omitempty" json:"blocked-models,omitempty"`
	Providers       []string            `yaml:"providers,omitempty" json:"providers,omitempty"`
	CanonicalModels []string            `yaml:"canonical-models,omitempty" json:"canonical-models,omitempty"`
	ModelMappings   map[string]string   `yaml:"model-mappings,omitempty" json:"model-mappings,omitempty"`
	PolicyProfile   string              `yaml:"policy-profile,omitempty" json:"policy-profile,omitempty"`
	ReservationTTL  string              `yaml:"reservation-ttl,omitempty" json:"reservation-ttl,omitempty"`
	Limits          []APIKeyPolicyLimit `yaml:"limits,omitempty" json:"limits,omitempty"`
}

func (rule APIKeyRule) isEmpty() bool {
	return len(rule.BlockedModels) == 0 &&
		len(rule.Providers) == 0 &&
		len(rule.CanonicalModels) == 0 &&
		len(rule.ModelMappings) == 0 &&
		strings.TrimSpace(rule.PolicyProfile) == "" &&
		strings.TrimSpace(rule.ReservationTTL) == "" &&
		len(rule.Limits) == 0
}

func (cfg *SDKConfig) APIKeyRule(apiKey string) (APIKeyRule, bool) {
	if cfg == nil || len(cfg.APIKeyRules) == 0 {
		return APIKeyRule{}, false
	}
	rule, ok := cfg.APIKeyRules[strings.TrimSpace(apiKey)]
	return rule, ok
}

func (cfg *SDKConfig) ResolveAPIKeyModel(apiKey string, model string) (string, bool, error) {
	rule, ok := cfg.APIKeyRule(apiKey)
	if !ok || len(rule.ModelMappings) == 0 {
		return model, false, nil
	}
	parsed := strings.TrimSpace(model)
	base := parsed
	suffix := ""
	if index := strings.Index(parsed, "("); index > 0 && strings.HasSuffix(parsed, ")") {
		base = strings.TrimSpace(parsed[:index])
		suffix = parsed[index:]
	}
	candidates := modelMatchCandidates(base)
	keys := make([]string, 0, len(rule.ModelMappings))
	for key := range rule.ModelMappings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	matched := ""
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(strings.TrimPrefix(key, "models/"))
		if trimmedKey == "" {
			return model, false, errors.New("API key model mapping contains an empty source")
		}
		for _, candidate := range candidates {
			trimmedCandidate := strings.TrimSpace(strings.TrimPrefix(candidate, "models/"))
			if !strings.EqualFold(trimmedKey, trimmedCandidate) {
				continue
			}
			if matched != "" {
				return model, false, errors.New("API key model mapping contains ambiguous sources")
			}
			matched = strings.TrimSpace(rule.ModelMappings[key])
			if matched == "" {
				return model, false, errors.New("API key model mapping contains an empty target")
			}
		}
	}
	if matched == "" {
		return model, false, nil
	}
	return matched + suffix, true, nil
}

func (cfg *SDKConfig) APIKeyAllowsCanonicalModel(apiKey string, model string) bool {
	rule, ok := cfg.APIKeyRule(apiKey)
	if !ok || len(rule.CanonicalModels) == 0 {
		return true
	}
	return blockedModelCandidateMatches(rule.CanonicalModels, cfg.modelAccessCandidates(model))
}

func (cfg *SDKConfig) FilterAPIKeyProviders(apiKey string, providers []string) []string {
	rule, ok := cfg.APIKeyRule(apiKey)
	if !ok || len(rule.Providers) == 0 {
		return append([]string(nil), providers...)
	}
	allowed := make(map[string]struct{}, len(rule.Providers))
	for _, provider := range rule.Providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider != "" {
			allowed[provider] = struct{}{}
		}
	}
	if _, all := allowed["*"]; all {
		return append([]string(nil), providers...)
	}
	filtered := make([]string, 0, len(providers))
	for _, provider := range providers {
		if _, permitted := allowed[strings.ToLower(strings.TrimSpace(provider))]; permitted {
			filtered = append(filtered, provider)
		}
	}
	return filtered
}

func (cfg *SDKConfig) APIKeyBlocksModel(apiKey string, model string) bool {
	if cfg == nil || len(cfg.APIKeyRules) == 0 {
		return false
	}
	apiKey = strings.TrimSpace(apiKey)
	model = strings.TrimSpace(model)
	if apiKey == "" || model == "" {
		return false
	}
	rule, ok := cfg.APIKeyRules[apiKey]
	if !ok {
		return false
	}
	return cfg.blockedModelMatches(rule.BlockedModels, model)
}

func (cfg *Config) PruneGloballyExcludedAPIKeyRules() {
	if cfg == nil || len(cfg.APIKeyRules) == 0 || len(cfg.OAuthExcludedModels) == 0 {
		return
	}
	excluded := make([]string, 0)
	for _, models := range cfg.OAuthExcludedModels {
		excluded = append(excluded, models...)
	}
	if len(excluded) == 0 {
		return
	}
	for apiKey, rule := range cfg.APIKeyRules {
		blockedModels := make([]string, 0, len(rule.BlockedModels))
		for _, model := range rule.BlockedModels {
			if strings.TrimSpace(model) == "" || blockedModelMatches(excluded, model) {
				continue
			}
			blockedModels = append(blockedModels, model)
		}
		rule.BlockedModels = blockedModels
		if rule.isEmpty() {
			delete(cfg.APIKeyRules, apiKey)
			continue
		}
		cfg.APIKeyRules[apiKey] = rule
	}
	if len(cfg.APIKeyRules) == 0 {
		cfg.APIKeyRules = nil
	}
}

func (cfg *SDKConfig) blockedModelMatches(patterns []string, model string) bool {
	return blockedModelCandidateMatches(patterns, cfg.modelAccessCandidates(model))
}

func blockedModelMatches(patterns []string, model string) bool {
	return blockedModelCandidateMatches(patterns, modelMatchCandidates(model))
}

func blockedModelCandidateMatches(patterns []string, modelCandidates []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		for _, candidate := range modelCandidates {
			if matchAPIKeyModelPattern(pattern, candidate) {
				return true
			}
		}
	}
	return false
}

func (cfg *SDKConfig) modelAccessCandidates(model string) []string {
	candidates := modelMatchCandidates(model)
	if entry, ok := cfg.modelCatalogEntry(model); ok {
		candidates = append(candidates, modelMatchCandidates(entry.Name)...)
		for _, alias := range entry.Aliases {
			candidates = append(candidates, modelMatchCandidates(alias)...)
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		key := strings.ToLower(strings.TrimPrefix(candidate, "models/"))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func modelMatchCandidates(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	candidates := []string{model}
	withoutResource := strings.TrimPrefix(model, "models/")
	if withoutResource != model {
		candidates = append(candidates, withoutResource)
	}
	for _, candidate := range append([]string(nil), candidates...) {
		if idx := strings.Index(candidate, "("); idx > 0 {
			base := strings.TrimSpace(candidate[:idx])
			if base != "" {
				candidates = append(candidates, base)
			}
		}
	}
	return candidates
}

func matchAPIKeyModelPattern(pattern string, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(pattern, "models/")))
	model = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(model, "models/")))
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" || pattern == model {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == model
	}
	cursor := 0
	if first := parts[0]; first != "" {
		if !strings.HasPrefix(model, first) {
			return false
		}
		cursor = len(first)
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		idx := strings.Index(model[cursor:], part)
		if idx < 0 {
			return false
		}
		cursor += idx + len(part)
	}
	last := parts[len(parts)-1]
	if last == "" {
		return true
	}
	idx := strings.Index(model[cursor:], last)
	if idx < 0 {
		return false
	}
	return strings.HasSuffix(model, last)
}

type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	ModelCatalog ModelCatalogConfig `yaml:"model-catalog,omitempty" json:"model-catalog,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// EnableGeminiCLIEndpoint controls access to Gemini CLI internal endpoints.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// AllowQueryAuth allows API key authentication via URL query parameters (?key= / ?auth_token=).
	// Disabled by default because query parameters leak credentials to logs, Referer headers, and browser history.
	AllowQueryAuth bool `yaml:"allow-query-auth" json:"allow-query-auth"`

	// APIKeyAliases maps API keys to human-readable aliases for display purposes.
	APIKeyAliases map[string]string `yaml:"api-key-aliases,omitempty" json:"api-key-aliases,omitempty"`

	APIKeyRules map[string]APIKeyRule `yaml:"api-key-rules,omitempty" json:"api-key-rules,omitempty"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// DisableImageGeneration controls whether image generation endpoints and tools are available.
	// "off" enables them; "all" returns 404 for image endpoints and strips chat tools; "chat" strips chat tools only; "passthrough" keeps client-supplied tools untouched.
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base model used by the legacy hosted image_generation tool path
	// when a Codex image request is not proxied directly through the Image API.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
