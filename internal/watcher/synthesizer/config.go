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
	return s.synthesizeGeminiKeyEntries(ctx, ctx.Config.InteractionsKey, "gemini-interactions:apikey", "interactions", "interactions-apikey", "gemini-interactions")
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
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		id, token := idGen.next(idKind, key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:%s[%s]", sourceName, token),
			"api_key": key,
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		if err := addConfigHeadersToAttrs(entry.Headers, attrs); err != nil {
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
			CreatedAt:  now,
			UpdatedAt:  now,
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
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		headers := config.NormalizeHeaders(ck.Headers)
		modelsHash := diff.ComputeClaudeModelsHash(ck.Models)
		excludedModelsHash := diff.ComputeExcludedModelsHash(ck.ExcludedModels)
		idParts := []string{key, base}
		if ck.Priority != 0 || prefix != "" || proxyURL != "" || modelsHash != "" || len(headers) > 0 ||
			excludedModelsHash != "" || ck.RebuildMidSystemMessage || ck.Cloak != nil || ck.ExperimentalCCHSigning {
			routeIdentity, errIdentity := stableConfigAuthRouteIdentity(
				ck.Priority,
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
		id, token := idGen.next("claude:apikey", idParts...)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:claude[%s]", token),
			"api_key": key,
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
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
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		out = append(out, a)
	}
	return out, nil
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		ck := cfg.CodexKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		headers := config.NormalizeHeaders(ck.Headers)
		modelsHash := diff.ComputeCodexModelsHash(ck.Models)
		excludedModelsHash := diff.ComputeExcludedModelsHash(ck.ExcludedModels)
		idParts := []string{key, base}
		if ck.Priority != 0 || prefix != "" || proxyURL != "" || modelsHash != "" || len(headers) > 0 ||
			excludedModelsHash != "" || ck.Websockets {
			routeIdentity, errIdentity := stableConfigAuthRouteIdentity(
				ck.Priority,
				prefix,
				proxyURL,
				modelsHash,
				headers,
				excludedModelsHash,
				ck.Websockets,
			)
			if errIdentity != nil {
				return nil, errIdentity
			}
			idParts = append(idParts, routeIdentity)
		}
		id, token := idGen.next("codex:apikey", idParts...)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:codex[%s]", token),
			"api_key": key,
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if ck.Websockets {
			attrs["websockets"] = "true"
		}
		if modelsHash != "" {
			attrs["models_hash"] = modelsHash
		}
		if err := addConfigHeadersToAttrs(headers, attrs); err != nil {
			return nil, err
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "codex",
			Label:      "codex-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
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

		// Handle new APIKeyEntries format (preferred)
		createdEntries := 0
		for j := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.next(idKind, key, base, proxyURL)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
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
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			out = append(out, a)
			createdEntries++
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if createdEntries == 0 {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.next(idKind, base)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
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
				CreatedAt:  now,
				UpdatedAt:  now,
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
		id, token := idGen.next(idKind, key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":     base,
			"provider_key": providerName,
		}
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
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
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, nil, "apikey")
		out = append(out, a)
	}
	return out, nil
}
