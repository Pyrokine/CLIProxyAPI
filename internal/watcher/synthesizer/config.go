package synthesizer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ConfigSynthesizer generates Auth entries from configuration API keys.
// It handles Gemini, Claude, Codex, OpenAI-compat, and Vertex-compat providers.
type ConfigSynthesizer struct{}

func stableConfigAuthRouteIdentity(parts ...any) (string, error) {
	data, err := json.Marshal(parts)
	if err != nil {
		return "", fmt.Errorf("marshal config auth route identity: %w", err)
	}
	return string(data), nil
}

func appendCredentialOverrideIdentity(parts []string, cooling *bool, retry *int) []string {
	if cooling != nil {
		parts = append(parts, "disable-cooling="+strconv.FormatBool(*cooling))
	}
	if retry != nil && *retry >= 0 {
		parts = append(parts, "request-retry="+strconv.Itoa(*retry))
	}
	return parts
}

func credentialOverrideMetadata(cooling *bool, retry *int) map[string]any {
	if cooling == nil && (retry == nil || *retry < 0) {
		return nil
	}
	metadata := make(map[string]any)
	if cooling != nil {
		metadata["disable_cooling"] = *cooling
	}
	if retry != nil && *retry >= 0 {
		metadata["request_retry"] = *retry
	}
	return metadata
}

func credentialMetadata(
	cooling *bool,
	retry *int,
	rules []config.RequestScopedErrorRule,
) map[string]any {
	metadata := credentialOverrideMetadata(cooling, retry)
	if len(rules) == 0 {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata["request_scoped_errors"] = cloneRequestScopedErrorRules(rules)
	return metadata
}

func cloneRequestScopedErrorRules(rules []config.RequestScopedErrorRule) []config.RequestScopedErrorRule {
	cloned := make([]config.RequestScopedErrorRule, len(rules))
	for i := range rules {
		cloned[i] = rules[i]
		cloned[i].Match = append([]string(nil), rules[i].Match...)
		cloned[i].MatchRegexr = append([]string(nil), rules[i].MatchRegexr...)
	}
	return cloned
}

func addWeightToAttrs(weight *int, attrs map[string]string) {
	if weight == nil {
		return
	}
	attrs[coreauth.AttributeWeight] = strconv.Itoa(max(*weight, 0))
}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}
	if errValidate := ctx.Config.ValidateCredentialWeights(); errValidate != nil {
		return nil, fmt.Errorf("synthesize config API key auths: %w", errValidate)
	}

	geminiKeys, err := s.synthesizeGeminiKeys(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, geminiKeys...)
	interactionsKeys, err := s.synthesizeInteractionsKeys(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, interactionsKeys...)
	claudeKeys, err := s.synthesizeClaudeKeys(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, claudeKeys...)
	codexKeys, err := s.synthesizeCodexKeys(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, codexKeys...)
	xaiKeys, err := s.synthesizeXAIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, xaiKeys...)
	openAICompat, err := s.synthesizeOpenAICompat(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, openAICompat...)
	vertexCompat, err := s.synthesizeVertexCompat(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, vertexCompat...)

	return out, nil
}

// synthesizeGeminiKeys creates Auth entries for Gemini API keys.
func (s *ConfigSynthesizer) synthesizeGeminiKeys(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	return s.synthesizeGeminiKeyEntries(ctx, ctx.Config.GeminiKey, "gemini:apikey", "gemini", "gemini-apikey", "gemini")
}

// synthesizeInteractionsKeys creates Auth entries for native Interactions API keys.
func (s *ConfigSynthesizer) synthesizeInteractionsKeys(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	return s.synthesizeGeminiKeyEntries(
		ctx, ctx.Config.InteractionsKey, "gemini-interactions:apikey", "interactions", "interactions-apikey",
		"gemini-interactions",
	)
}

func (s *ConfigSynthesizer) synthesizeGeminiKeyEntries(
	ctx *SynthesisContext,
	entries []config.GeminiKey,
	idKind string,
	sourceName string,
	label string,
	provider string,
) ([]*coreauth.Auth, error) {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		key := strings.TrimSpace(entry.APIKey)
		base := strings.TrimSpace(entry.BaseURL)
		if key == "" && base == "" {
			continue
		}
		prefix := config.NormalizeModelPrefix(entry.Prefix)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		headers := config.NormalizeHeaders(entry.Headers)
		idParts := []string{key, base, proxyURL, prefix, config.FormatSortedHeaders(headers)}
		idParts = appendCredentialOverrideIdentity(idParts, entry.DisableCooling, entry.RequestRetry)
		id, token := idGen.next(idKind, idParts...)
		attrs := map[string]string{
			"source":                      fmt.Sprintf("config:%s[%s]", sourceName, token),
			coreauth.AttributeConfigIndex: strconv.Itoa(i),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		addWeightToAttrs(entry.Weight, attrs)
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		if err := addConfigHeadersToAttrs(headers, attrs); err != nil {
			return nil, err
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   provider,
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata: credentialMetadata(
				entry.DisableCooling, entry.RequestRetry, entry.RequestScopedErrors,
			),
			CreatedAt: now,
			UpdatedAt: now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		out = append(out, a)
	}
	return out, nil
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		base := strings.TrimSpace(ck.BaseURL)
		if key == "" && base == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		headers := config.NormalizeHeaders(ck.Headers)
		modelsHash := diff.ComputeClaudeModelsHash(ck.Models)
		excludedModelsHash := diff.ComputeExcludedModelsHash(ck.ExcludedModels)
		profile, errProfile := config.ParseClaudeFingerprintProfile(ck.FingerprintProfile)
		if errProfile != nil {
			return nil, fmt.Errorf("claude-api-key[%d]: %w", i, errProfile)
		}
		idParts := []string{key, base}
		if profile != "" {
			idParts = append(idParts, "fingerprint-profile="+profile)
		}
		if ck.Priority != 0 || ck.Weight != nil || prefix != "" || proxyURL != "" || modelsHash != "" ||
			len(headers) > 0 || excludedModelsHash != "" || ck.RebuildMidSystemMessage || ck.Cloak != nil ||
			ck.ExperimentalCCHSigning {
			routeIdentity, errIdentity := stableConfigAuthRouteIdentity(
				ck.Priority,
				ck.Weight,
				prefix,
				proxyURL,
				modelsHash,
				headers,
				excludedModelsHash,
				ck.RebuildMidSystemMessage,
				ck.Cloak,
				ck.ExperimentalCCHSigning,
			)
			if errIdentity != nil {
				return nil, errIdentity
			}
			idParts = append(idParts, routeIdentity)
		}
		idParts = appendCredentialOverrideIdentity(idParts, ck.DisableCooling, ck.RequestRetry)
		id, token := idGen.next("claude:apikey", idParts...)
		attrs := map[string]string{
			"source":                      fmt.Sprintf("config:claude[%s]", token),
			"fingerprint_profile":         profile,
			coreauth.AttributeConfigIndex: strconv.Itoa(i),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		addWeightToAttrs(ck.Weight, attrs)
		if base != "" {
			attrs["base_url"] = base
		}
		if ck.RebuildMidSystemMessage {
			attrs["rebuild_mid_system_message"] = "true"
		}
		if modelsHash != "" {
			attrs["models_hash"] = modelsHash
		}
		if err := addConfigHeadersToAttrs(headers, attrs); err != nil {
			return nil, err
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			Label:      "claude-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata: credentialMetadata(
				ck.DisableCooling, ck.RequestRetry, ck.RequestScopedErrors,
			),
			CreatedAt: now,
			UpdatedAt: now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		out = append(out, a)
	}
	return out, nil
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	return s.synthesizeCodexStyleKeys(ctx, ctx.Config.CodexKey, "codex")
}

// synthesizeXAIKeys creates Auth entries for xAI API keys.
func (s *ConfigSynthesizer) synthesizeXAIKeys(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	return s.synthesizeCodexStyleKeys(ctx, ctx.Config.XAIKey, "xai")
}

func (s *ConfigSynthesizer) synthesizeCodexStyleKeys(
	ctx *SynthesisContext,
	entries []config.CodexKey,
	provider string,
) ([]*coreauth.Auth, error) {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		key := strings.TrimSpace(entry.APIKey)
		base := strings.TrimSpace(entry.BaseURL)
		if key == "" && base == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		headers := config.NormalizeHeaders(entry.Headers)
		modelsHash := diff.ComputeCodexModelsHash(entry.Models)
		excludedModelsHash := diff.ComputeExcludedModelsHash(entry.ExcludedModels)
		idParts := []string{key, base}
		disableCooling := entry.DisableCooling != nil && *entry.DisableCooling
		if entry.Priority != 0 || entry.Weight != nil || prefix != "" || proxyURL != "" || modelsHash != "" ||
			len(headers) > 0 || excludedModelsHash != "" || entry.Websockets || disableCooling {
			routeIdentity, errIdentity := stableConfigAuthRouteIdentity(
				entry.Priority,
				entry.Weight,
				prefix,
				proxyURL,
				modelsHash,
				headers,
				excludedModelsHash,
				entry.Websockets,
				disableCooling,
			)
			if errIdentity != nil {
				return nil, errIdentity
			}
			idParts = append(idParts, routeIdentity)
		}
		// 旧 true 已编码在 route identity 中，缺失值仍编码为旧 false
		coolingIdentity := entry.DisableCooling
		if disableCooling {
			coolingIdentity = nil
		}
		idParts = appendCredentialOverrideIdentity(idParts, coolingIdentity, entry.RequestRetry)
		id, token := idGen.next(provider+":apikey", idParts...)
		attrs := map[string]string{
			"source":                      fmt.Sprintf("config:%s[%s]", provider, token),
			coreauth.AttributeConfigIndex: strconv.Itoa(i),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		addWeightToAttrs(entry.Weight, attrs)
		if base != "" {
			attrs["base_url"] = base
		}
		if entry.Websockets {
			attrs["websockets"] = "true"
		}
		if provider == "codex" && entry.AlphaSearch {
			attrs[coreauth.AttributeCodexAlphaSearch] = "true"
		}
		if modelsHash != "" {
			attrs["models_hash"] = modelsHash
		}
		if err := addConfigHeadersToAttrs(headers, attrs); err != nil {
			return nil, err
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   provider,
			Label:      provider + "-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata: credentialMetadata(
				entry.DisableCooling, entry.RequestRetry, entry.RequestScopedErrors,
			),
			CreatedAt: now,
			UpdatedAt: now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		out = append(out, a)
	}
	return out, nil
}

// synthesizeOpenAICompat creates Auth entries for OpenAI-compatible providers.
func (s *ConfigSynthesizer) synthesizeOpenAICompat(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0)
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		prefix := strings.TrimSpace(compat.Prefix)
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		base := strings.TrimSpace(compat.BaseURL)
		coolingIdentity := compat.DisableCooling
		if coolingIdentity != nil && *coolingIdentity {
			coolingIdentity = nil
		}

		// Handle new APIKeyEntries format (preferred)
		createdEntries := 0
		for j := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			idParts := appendCredentialOverrideIdentity([]string{key, base, proxyURL}, coolingIdentity, compat.RequestRetry)
			id, token := idGen.next(idKind, idParts...)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
				"config_index": strconv.Itoa(i),
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			addWeightToAttrs(entry.Weight, attrs)
			if key != "" {
				attrs["api_key"] = key
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			if err := addConfigHeadersToAttrs(compat.Headers, attrs); err != nil {
				return nil, err
			}
			a := &coreauth.Auth{
				ID:         id,
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				ProxyURL:   proxyURL,
				Attributes: attrs,
				Metadata: credentialMetadata(
					compat.DisableCooling, compat.RequestRetry, compat.RequestScopedErrors,
				),
				CreatedAt: now,
				UpdatedAt: now,
			}
			out = append(out, a)
			createdEntries++
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if createdEntries == 0 {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			idParts := appendCredentialOverrideIdentity([]string{base}, coolingIdentity, compat.RequestRetry)
			id, token := idGen.next(idKind, idParts...)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
				"config_index": strconv.Itoa(i),
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			if err := addConfigHeadersToAttrs(compat.Headers, attrs); err != nil {
				return nil, err
			}
			a := &coreauth.Auth{
				ID:         id,
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				Attributes: attrs,
				Metadata: credentialMetadata(
					compat.DisableCooling, compat.RequestRetry, compat.RequestScopedErrors,
				),
				CreatedAt: now,
				UpdatedAt: now,
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// synthesizeVertexCompat creates Auth entries for Vertex-compatible providers.
func (s *ConfigSynthesizer) synthesizeVertexCompat(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		compat := &cfg.VertexCompatAPIKey[i]
		providerName := "vertex"
		base := strings.TrimSpace(compat.BaseURL)

		key := strings.TrimSpace(compat.APIKey)
		prefix := strings.TrimSpace(compat.Prefix)
		proxyURL := strings.TrimSpace(compat.ProxyURL)
		idKind := "vertex:apikey"
		idParts := appendCredentialOverrideIdentity([]string{key, base, proxyURL}, compat.DisableCooling, compat.RequestRetry)
		id, token := idGen.next(idKind, idParts...)
		attrs := map[string]string{
			"source":                      fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":                    base,
			"provider_key":                providerName,
			coreauth.AttributeConfigIndex: strconv.Itoa(i),
		}
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
		addWeightToAttrs(compat.Weight, attrs)
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeVertexCompatModelsHash(compat.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		if err := addConfigHeadersToAttrs(compat.Headers, attrs); err != nil {
			return nil, err
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   providerName,
			Label:      "vertex-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   credentialOverrideMetadata(compat.DisableCooling, compat.RequestRetry),
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, nil, "apikey")
		out = append(out, a)
	}
	return out, nil
}
