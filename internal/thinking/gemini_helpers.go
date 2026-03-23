package thinking

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Gemini-family path prefixes for thinkingConfig.
const (
	GeminiDirectPrefix = "generationConfig.thinkingConfig"
	GeminiCLIPrefix    = "request.generationConfig.thinkingConfig"
)

// ApplyGeminiLevelFormat applies level-based thinking configuration using the given JSON path prefix.
// Used by gemini, gemini-cli, and antigravity providers which share identical logic but differ in JSON paths.
func ApplyGeminiLevelFormat(body []byte, config Config, prefix string) ([]byte, error) {
	// Remove conflicting fields to avoid both thinkingLevel and thinkingBudget in output
	result, _ := sjson.DeleteBytes(body, prefix+".thinkingBudget")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_budget")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_level")
	// Normalize includeThoughts field name to avoid oneof conflicts in upstream JSON parsing.
	result, _ = sjson.DeleteBytes(result, prefix+".include_thoughts")

	if config.Mode == ModeNone {
		result, _ = sjson.SetBytes(result, prefix+".includeThoughts", false)
		if config.Level != "" {
			result, _ = sjson.SetBytes(result, prefix+".thinkingLevel", string(config.Level))
		}
		return result, nil
	}

	// Only handle ModeLevel - budget conversion should be done by upper layer
	if config.Mode != ModeLevel {
		return body, nil
	}

	level := string(config.Level)
	result, _ = sjson.SetBytes(result, prefix+".thinkingLevel", level)

	// Respect user's explicit includeThoughts setting from original body; default to true if not set
	// Support both camelCase and snake_case variants
	includeThoughts := true
	if inc := gjson.GetBytes(body, prefix+".includeThoughts"); inc.Exists() {
		includeThoughts = inc.Bool()
	} else if inc := gjson.GetBytes(body, prefix+".include_thoughts"); inc.Exists() {
		includeThoughts = inc.Bool()
	}
	result, _ = sjson.SetBytes(result, prefix+".includeThoughts", includeThoughts)
	return result, nil
}

// ApplyGeminiBudgetFormat applies budget-based thinking configuration using the given JSON path prefix.
// Used by gemini, gemini-cli, and antigravity providers which share identical logic but differ in JSON paths.
// The budget parameter allows callers to pre-process the budget (e.g., antigravity's Claude normalization).
func ApplyGeminiBudgetFormat(body []byte, config Config, budget int, prefix string) ([]byte, error) {
	// Remove conflicting fields to avoid both thinkingLevel and thinkingBudget in output
	result, _ := sjson.DeleteBytes(body, prefix+".thinkingLevel")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_level")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_budget")
	// Normalize includeThoughts field name to avoid oneof conflicts in upstream JSON parsing.
	result, _ = sjson.DeleteBytes(result, prefix+".include_thoughts")

	// For ModeNone, always set includeThoughts to false regardless of user setting.
	// This ensures that when user requests budget=0 (disable thinking output),
	// the includeThoughts is correctly set to false even if budget is clamped to min.
	if config.Mode == ModeNone {
		result, _ = sjson.SetBytes(result, prefix+".thinkingBudget", budget)
		result, _ = sjson.SetBytes(result, prefix+".includeThoughts", false)
		return result, nil
	}

	// Determine includeThoughts: respect user's explicit setting from original body if provided
	// Support both camelCase and snake_case variants
	var includeThoughts bool
	var userSetIncludeThoughts bool
	if inc := gjson.GetBytes(body, prefix+".includeThoughts"); inc.Exists() {
		includeThoughts = inc.Bool()
		userSetIncludeThoughts = true
	} else if inc := gjson.GetBytes(body, prefix+".include_thoughts"); inc.Exists() {
		includeThoughts = inc.Bool()
		userSetIncludeThoughts = true
	}

	if !userSetIncludeThoughts {
		// No explicit setting, use default logic based on mode
		switch config.Mode {
		case ModeAuto:
			includeThoughts = true
		default:
			includeThoughts = budget > 0
		}
	}

	result, _ = sjson.SetBytes(result, prefix+".thinkingBudget", budget)
	result, _ = sjson.SetBytes(result, prefix+".includeThoughts", includeThoughts)
	return result, nil
}

// ApplyGeminiCompatible applies thinking configuration for user-defined models using the given
// JSON path prefix and format applier functions. This handles the mode routing logic shared across
// gemini-family providers.
func ApplyGeminiCompatible(
	body []byte,
	config Config,
	levelFn func([]byte, Config) ([]byte, error),
	budgetFn func([]byte, Config) ([]byte, error),
) ([]byte, error) {
	body, ok := ValidateGeminiPreconditions(body, config)
	if !ok {
		return body, nil
	}

	if config.Mode == ModeAuto {
		return budgetFn(body, config)
	}

	if config.Mode == ModeLevel || (config.Mode == ModeNone && config.Level != "") {
		return levelFn(body, config)
	}

	return budgetFn(body, config)
}

// ValidateGeminiPreconditions validates the mode and body for Gemini-family thinking apply.
// Returns the (possibly sanitized) body and whether to continue.
func ValidateGeminiPreconditions(body []byte, config Config) ([]byte, bool) {
	if config.Mode != ModeBudget && config.Mode != ModeLevel && config.Mode != ModeNone && config.Mode != ModeAuto {
		return body, false
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}
	return body, true
}
