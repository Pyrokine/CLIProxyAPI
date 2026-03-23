// Package antigravity implements thinking configuration for Antigravity API format.
//
// Antigravity uses request.generationConfig.thinkingConfig.* path (same as gemini-cli)
// but requires additional normalization for Claude models:
//   - Ensure thinking budget < max_tokens
//   - Remove thinkingConfig if budget < minimum allowed
package antigravity

import (
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Applier applies thinking configuration for Antigravity API format.
type Applier struct{}

var _ thinking.ProviderApplier = (*Applier)(nil)

// newApplier creates a new Antigravity thinking applier.
func newApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("antigravity", newApplier())
}

// Apply applies thinking configuration to Antigravity request body.
//
// For Claude models, additional constraints are applied:
//   - Ensure thinking budget < max_tokens
//   - Remove thinkingConfig if budget < minimum allowed
func (a *Applier) Apply(body []byte, config thinking.Config, modelInfo *registry.ModelInfo) ([]byte, error) {
	if thinking.IsUserDefinedModel(modelInfo) {
		return a.applyCompatible(body, config, modelInfo)
	}
	if modelInfo.Thinking == nil {
		return body, nil
	}

	var ok bool
	if body, ok = thinking.ValidateGeminiPreconditions(body, config); !ok {
		return body, nil
	}

	isClaude := strings.Contains(strings.ToLower(modelInfo.ID), "claude")

	// ModeAuto/ModeBudget: Always use Budget format
	if config.Mode == thinking.ModeAuto || config.Mode == thinking.ModeBudget {
		return a.applyBudgetFormat(body, config, modelInfo, isClaude)
	}

	// For non-auto modes, choose format based on model capabilities
	support := modelInfo.Thinking
	if len(support.Levels) > 0 {
		return thinking.ApplyGeminiLevelFormat(body, config, thinking.GeminiCLIPrefix)
	}
	return a.applyBudgetFormat(body, config, modelInfo, isClaude)
}

func (a *Applier) applyCompatible(body []byte, config thinking.Config, modelInfo *registry.ModelInfo) (
	[]byte,
	error,
) {
	var ok bool
	if body, ok = thinking.ValidateGeminiPreconditions(body, config); !ok {
		return body, nil
	}

	isClaude := false
	if modelInfo != nil {
		isClaude = strings.Contains(strings.ToLower(modelInfo.ID), "claude")
	}

	if config.Mode == thinking.ModeAuto {
		return a.applyBudgetFormat(body, config, modelInfo, isClaude)
	}

	if config.Mode == thinking.ModeLevel || (config.Mode == thinking.ModeNone && config.Level != "") {
		return thinking.ApplyGeminiLevelFormat(body, config, thinking.GeminiCLIPrefix)
	}

	return a.applyBudgetFormat(body, config, modelInfo, isClaude)
}

// applyBudgetFormat applies Claude-specific budget normalization then delegates to shared logic.
func (a *Applier) applyBudgetFormat(
	body []byte,
	config thinking.Config,
	modelInfo *registry.ModelInfo,
	isClaude bool,
) ([]byte, error) {
	budget := config.Budget

	// Apply Claude-specific constraints before shared budget logic
	if isClaude && modelInfo != nil {
		budget, body = a.normalizeClaudeBudget(budget, body, modelInfo)
		// Sentinel: thinkingConfig was removed entirely
		if budget == -2 {
			return body, nil
		}
	}

	return thinking.ApplyGeminiBudgetFormat(body, config, budget, thinking.GeminiCLIPrefix)
}

// normalizeClaudeBudget applies Claude-specific constraints to thinking budget.
//
// It handles:
//   - Ensuring thinking budget < max_tokens
//   - Removing thinkingConfig if budget < minimum allowed
//
// Returns the normalized budget and updated payload.
// Returns budget=-2 as a sentinel indicating thinkingConfig was removed entirely.
func (a *Applier) normalizeClaudeBudget(budget int, payload []byte, modelInfo *registry.ModelInfo) (int, []byte) {
	if modelInfo == nil {
		return budget, payload
	}

	// Get effective max tokens
	effectiveMax, setDefaultMax := a.effectiveMaxTokens(payload, modelInfo)
	if effectiveMax > 0 && budget >= effectiveMax {
		budget = effectiveMax - 1
	}

	// Check minimum budget
	minBudget := 0
	if modelInfo.Thinking != nil {
		minBudget = modelInfo.Thinking.Min
	}
	if minBudget > 0 && budget >= 0 && budget < minBudget {
		// Budget is below minimum, remove thinking config entirely
		payload, _ = sjson.DeleteBytes(payload, "request.generationConfig.thinkingConfig")
		return -2, payload
	}

	// Set default max tokens if needed
	if setDefaultMax && effectiveMax > 0 {
		payload, _ = sjson.SetBytes(payload, "request.generationConfig.maxOutputTokens", effectiveMax)
	}

	return budget, payload
}

// effectiveMaxTokens returns the max tokens to cap thinking:
// prefer request-provided maxOutputTokens; otherwise fall back to model default.
// The boolean indicates whether the value came from the model default (and thus should be written back).
func (a *Applier) effectiveMaxTokens(payload []byte, modelInfo *registry.ModelInfo) (max int, fromModel bool) {
	if maxTok := gjson.GetBytes(
		payload, "request.generationConfig.maxOutputTokens",
	); maxTok.Exists() && maxTok.Int() > 0 {
		return int(maxTok.Int()), false
	}
	if modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
		return modelInfo.MaxCompletionTokens, true
	}
	return 0, false
}
