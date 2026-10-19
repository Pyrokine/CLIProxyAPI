// Package claude implements thinking configuration scaffolding for Claude models.
//
// Claude models use manual thinking budgets or adaptive output_config.effort levels.
// Some models allow thinking to be disabled, while older models do not.
// See: _bmad-output/planning-artifacts/architecture.md#Epic-6
package claude

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applier implements thinking.ProviderApplier for Claude models.
// This applier is stateless and holds no configuration.
type applier struct{}

// newApplier creates a new Claude thinking applier.
func newApplier() *applier {
	return &applier{}
}

func init() {
	thinking.RegisterProvider("claude", newApplier())
}

// Apply applies thinking configuration to Claude request body.
//
// IMPORTANT: This method expects config to be pre-validated by thinking.ValidateConfig.
// ValidateConfig handles:
//   - Mode conversion (Level→Budget, Auto→Budget)
//   - Budget clamping to model range
//   - ZeroAllowed constraint enforcement
//
// Apply handles manual budgets, adaptive effort levels, automatic mode, and explicit disable.
//
// Expected output format when enabled:
//
//	{
//	  "thinking": {
//	    "type": "enabled",
//	    "budget_tokens": 16384
//	  }
//	}
//
// Expected output format when disabled:
//
//	{
//	  "thinking": {
//	    "type": "disabled"
//	  }
//	}
func (a *applier) Apply(body []byte, config thinking.Config, modelInfo *registry.ModelInfo) ([]byte, error) {
	if thinking.IsUserDefinedModel(modelInfo) {
		return applyCompatibleClaude(body, config)
	}
	if modelInfo.Thinking == nil {
		return body, nil
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	switch config.Mode {
	case thinking.ModeLevel:
		if config.Level == "" {
			return body, nil
		}
		result, _ := sjson.SetBytes(body, "thinking.type", "adaptive")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		result, _ = sjson.SetBytes(result, "output_config.effort", string(config.Level))
		return result, nil
	case thinking.ModeAuto:
		thinkingType := "enabled"
		if len(modelInfo.Thinking.Levels) > 0 {
			thinkingType = "adaptive"
		}
		result, _ := sjson.SetBytes(body, "thinking.type", thinkingType)
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		result, _ = sjson.DeleteBytes(result, "output_config.effort")
		return deleteEmptyClaudeOutputConfig(result), nil
	case thinking.ModeNone:
		result, _ := sjson.SetBytes(body, "thinking.type", "disabled")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		result, _ = sjson.DeleteBytes(result, "thinking.display")
		result, _ = sjson.DeleteBytes(result, "output_config.effort")
		return deleteEmptyClaudeOutputConfig(result), nil
	case thinking.ModeBudget:
		if config.Budget == 0 {
			result, _ := sjson.SetBytes(body, "thinking.type", "disabled")
			result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
			result, _ = sjson.DeleteBytes(result, "thinking.display")
			result, _ = sjson.DeleteBytes(result, "output_config.effort")
			return deleteEmptyClaudeOutputConfig(result), nil
		}

		result, _ := sjson.SetBytes(body, "thinking.type", "enabled")
		result, _ = sjson.SetBytes(result, "thinking.budget_tokens", config.Budget)
		result, _ = sjson.DeleteBytes(result, "output_config.effort")
		result = deleteEmptyClaudeOutputConfig(result)
		return a.normalizeClaudeBudget(result, config.Budget, modelInfo), nil
	default:
		return body, nil
	}
}

func deleteEmptyClaudeOutputConfig(body []byte) []byte {
	outputConfig := gjson.GetBytes(body, "output_config")
	if outputConfig.IsObject() && len(outputConfig.Map()) == 0 {
		body, _ = sjson.DeleteBytes(body, "output_config")
	}
	return body
}

// normalizeClaudeBudget applies Claude-specific constraints to ensure max_tokens > budget_tokens.
// Anthropic API requires this constraint; violating it returns a 400 error.
func (a *applier) normalizeClaudeBudget(body []byte, budgetTokens int, modelInfo *registry.ModelInfo) []byte {
	if budgetTokens <= 0 {
		return body
	}

	// Ensure the request satisfies Claude constraints:
	//  1) Determine effective max_tokens (request overrides model default)
	//  2) If budget_tokens >= max_tokens, reduce budget_tokens to max_tokens-1
	//  3) If the adjusted budget falls below the model minimum, leave the request unchanged
	//  4) If max_tokens came from model default, write it back into the request

	effectiveMax, setDefaultMax := a.effectiveMaxTokens(body, modelInfo)
	if setDefaultMax && effectiveMax > 0 {
		body, _ = sjson.SetBytes(body, "max_tokens", effectiveMax)
	}

	// Compute the budget we would apply after enforcing budget_tokens < max_tokens.
	adjustedBudget := budgetTokens
	if effectiveMax > 0 && adjustedBudget >= effectiveMax {
		adjustedBudget = effectiveMax - 1
	}

	minBudget := 0
	if modelInfo != nil && modelInfo.Thinking != nil {
		minBudget = modelInfo.Thinking.Min
	}
	if minBudget > 0 && adjustedBudget > 0 && adjustedBudget < minBudget {
		// If enforcing the max_tokens constraint would push the budget below the model minimum,
		// leave the request unchanged.
		return body
	}

	if adjustedBudget != budgetTokens {
		body, _ = sjson.SetBytes(body, "thinking.budget_tokens", adjustedBudget)
	}

	return body
}

// effectiveMaxTokens returns the max tokens to cap thinking:
// prefer request-provided max_tokens; otherwise fall back to model default.
// The boolean indicates whether the value came from the model default (and thus should be written back).
func (a *applier) effectiveMaxTokens(body []byte, modelInfo *registry.ModelInfo) (max int, fromModel bool) {
	if maxTok := gjson.GetBytes(body, "max_tokens"); maxTok.Exists() && maxTok.Int() > 0 {
		return int(maxTok.Int()), false
	}
	if modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
		return modelInfo.MaxCompletionTokens, true
	}
	return 0, false
}

func applyCompatibleClaude(body []byte, config thinking.Config) ([]byte, error) {
	if config.Mode == thinking.ModeLevel {
		if config.Level == "" {
			return body, nil
		}
		if gjson.GetBytes(body, "thinking.type").String() == "adaptive" {
			return sjson.SetBytes(body, "output_config.effort", string(config.Level))
		}
		budget, ok := thinking.ConvertLevelToBudget(string(config.Level))
		if !ok {
			return body, nil
		}
		config.Mode = thinking.ModeBudget
		config.Budget = budget
	}
	if config.Mode != thinking.ModeBudget && config.Mode != thinking.ModeNone && config.Mode != thinking.ModeAuto {
		return body, nil
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	switch config.Mode {
	case thinking.ModeNone:
		result, _ := sjson.SetBytes(body, "thinking.type", "disabled")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		result, _ = sjson.DeleteBytes(result, "thinking.display")
		return result, nil
	case thinking.ModeAuto:
		result, _ := sjson.SetBytes(body, "thinking.type", "enabled")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		return result, nil
	default:
		result, _ := sjson.SetBytes(body, "thinking.type", "enabled")
		result, _ = sjson.SetBytes(result, "thinking.budget_tokens", config.Budget)
		return result, nil
	}
}
