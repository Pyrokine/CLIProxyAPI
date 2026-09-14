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
	result, _ := sjson.DeleteBytes(body, prefix+".thinkingBudget")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_budget")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_level")
	result, _ = sjson.DeleteBytes(result, prefix+".includeThoughts")
	result, _ = sjson.DeleteBytes(result, prefix+".include_thoughts")

	if config.Mode == ModeNone {
		if config.Budget == 0 && config.Level == "" {
			result, _ = sjson.DeleteBytes(result, prefix)
			return result, nil
		}
		if config.Level != "" {
			result, _ = sjson.SetBytes(result, prefix+".thinkingLevel", string(config.Level))
		}
		return applyGeminiSummaryBool(result, body, prefix), nil
	}

	if config.Mode != ModeLevel {
		return body, nil
	}

	result, _ = sjson.SetBytes(result, prefix+".thinkingLevel", string(config.Level))
	return applyGeminiSummaryBool(result, body, prefix), nil
}

// ApplyGeminiBudgetFormat applies budget-based thinking configuration using the given JSON path prefix.
// The budget parameter allows callers to pre-process the budget before shared field normalization.
func ApplyGeminiBudgetFormat(body []byte, config Config, budget int, prefix string) ([]byte, error) {
	result, _ := sjson.DeleteBytes(body, prefix+".thinkingLevel")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_level")
	result, _ = sjson.DeleteBytes(result, prefix+".thinking_budget")
	result, _ = sjson.DeleteBytes(result, prefix+".includeThoughts")
	result, _ = sjson.DeleteBytes(result, prefix+".include_thoughts")

	if config.Mode == ModeNone && config.Budget == 0 && config.Level == "" {
		result, _ = sjson.DeleteBytes(result, prefix)
		return result, nil
	}

	result, _ = sjson.SetBytes(result, prefix+".thinkingBudget", budget)
	return applyGeminiSummaryBool(result, body, prefix), nil
}

func applyGeminiSummaryBool(result, original []byte, prefix string) []byte {
	for _, path := range []string{
		prefix + ".includeThoughts",
		prefix + ".include_thoughts",
	} {
		// Only JSON booleans have meaning for includeThoughts.
		// noinspection GoSwitchMissingCasesForIotaConsts
		switch value := gjson.GetBytes(original, path); value.Type {
		case gjson.True:
			result, _ = sjson.SetBytes(result, prefix+".includeThoughts", true)
			return result
		case gjson.False:
			result, _ = sjson.SetBytes(result, prefix+".includeThoughts", false)
			return result
		}
	}
	return result
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
// Returns the possibly sanitized body and whether to continue.
func ValidateGeminiPreconditions(body []byte, config Config) ([]byte, bool) {
	if config.Mode != ModeBudget && config.Mode != ModeLevel && config.Mode != ModeNone && config.Mode != ModeAuto {
		return body, false
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}
	return body, true
}
