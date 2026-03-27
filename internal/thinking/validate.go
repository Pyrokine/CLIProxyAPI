// Package thinking provides unified thinking configuration processing logic.
package thinking

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

// validateConfig validates a thinking configuration against model capabilities.
//
// This function performs comprehensive validation:
//   - Checks if the model supports thinking
//   - Auto-converts between Budget and Level formats based on model capability
//   - Validates that requested level is in the model's supported levels list
//   - Clamps budget values to model's allowed range
//   - When converting Budget -> Level for level-only models, clamps the derived standard level to the nearest supported level
//     (special values none/auto are preserved)
//   - When config comes from a model suffix, strict budget validation is disabled (we clamp instead of error)
//
// Parameters:
//   - config: The thinking configuration to validate
//   - support: Model's ThinkingSupport properties (nil means no thinking support)
//   - fromFormat: Source provider format (used to determine strict validation rules)
//   - toFormat: Target provider format
//   - fromSuffix: Whether config was sourced from model suffix
//
// Returns:
//   - Normalized Config with clamped values
//   - Error if validation fails (errThinkingNotSupported, errLevelNotSupported, etc.)
//
// Auto-conversion behavior:
//   - Budget-only model + Level config → Level converted to Budget
//   - Level-only model + Budget config → Budget converted to Level
//   - Hybrid model → preserve original format
func validateConfig(
	config Config,
	modelInfo *registry.ModelInfo,
	fromFormat, toFormat string,
	fromSuffix bool,
) (*Config, error) {
	fromFormat, toFormat = strings.ToLower(strings.TrimSpace(fromFormat)), strings.ToLower(strings.TrimSpace(toFormat))
	model := "unknown"
	support := (*registry.ThinkingSupport)(nil)
	if modelInfo != nil {
		if modelInfo.ID != "" {
			model = modelInfo.ID
		}
		support = modelInfo.Thinking
	}

	if support == nil {
		if config.Mode != ModeNone {
			return nil, newErrorWithModel(
				errThinkingNotSupported, "thinking not supported for this model", model,
			)
		}
		return &config, nil
	}

	allowClampUnsupported := isBudgetCapableProvider(fromFormat) && isLevelBasedProvider(toFormat)
	strictBudget := !fromSuffix && fromFormat != "" && isSameProviderFamily(fromFormat, toFormat)
	budgetDerivedFromLevel := false

	capability := detectModelCapability(modelInfo)
	// noinspection GoSwitchMissingCasesForIotaConsts — capabilityNone/capabilityUnknown are impossible here (support != nil guard above).
	switch capability {
	case capabilityBudgetOnly:
		if config.Mode == ModeLevel {
			if config.Level == LevelAuto {
				break
			}
			budget, ok := ConvertLevelToBudget(string(config.Level))
			if !ok {
				return nil, newError(errUnknownLevel, fmt.Sprintf("unknown level: %s", config.Level))
			}
			config.Mode = ModeBudget
			config.Budget = budget
			config.Level = ""
			budgetDerivedFromLevel = true
		}
	case capabilityLevelOnly:
		if config.Mode == ModeBudget {
			level, ok := ConvertBudgetToLevel(config.Budget)
			if !ok {
				return nil, newError(
					errUnknownLevel, fmt.Sprintf("budget %d cannot be converted to a valid level", config.Budget),
				)
			}
			// When converting Budget -> Level for level-only models, clamp the derived standard level
			// to the nearest supported level. Special values (none/auto) are preserved.
			config.Mode = ModeLevel
			config.Level = clampLevel(Level(level), modelInfo, toFormat)
			config.Budget = 0
		}
	case capabilityHybrid:
	default:
		// capabilityNone and capabilityUnknown are impossible here:
		// support != nil guard above ensures the model supports thinking,
		// and user-defined / nil modelInfo paths exit before reaching this point.
	}

	if config.Mode == ModeLevel && config.Level == LevelNone {
		config.Mode = ModeNone
		config.Budget = 0
		config.Level = ""
	}
	if config.Mode == ModeLevel && config.Level == LevelAuto {
		config.Mode = ModeAuto
		config.Budget = -1
		config.Level = ""
	}
	if config.Mode == ModeBudget && config.Budget == 0 {
		config.Mode = ModeNone
		config.Level = ""
	}

	if len(support.Levels) > 0 && config.Mode == ModeLevel {
		if !isLevelSupported(string(config.Level), support.Levels) {
			if allowClampUnsupported {
				config.Level = clampLevel(config.Level, modelInfo, toFormat)
			}
			if !isLevelSupported(string(config.Level), support.Levels) {
				// User explicitly specified an unsupported level - return error
				// (budget-derived levels may be clamped based on source format)
				validLevels := normalizeLevels(support.Levels)
				message := fmt.Sprintf(
					"level %q not supported, valid levels: %s", strings.ToLower(string(config.Level)),
					strings.Join(validLevels, ", "),
				)
				return nil, newError(errLevelNotSupported, message)
			}
		}
	}

	if strictBudget && config.Mode == ModeBudget && !budgetDerivedFromLevel {
		lo, hi := support.Min, support.Max
		if lo != 0 || hi != 0 {
			if config.Budget < lo || config.Budget > hi || (config.Budget == 0 && !support.ZeroAllowed) {
				message := fmt.Sprintf("budget %d out of range [%d,%d]", config.Budget, lo, hi)
				return nil, newError(errBudgetOutOfRange, message)
			}
		}
	}

	// Convert ModeAuto to mid-range if dynamic not allowed
	if config.Mode == ModeAuto && !support.DynamicAllowed {
		config = convertAutoToMidRange(config, support, toFormat, model)
	}

	if config.Mode == ModeNone && toFormat == "claude" {
		// Claude supports explicit disable via thinking.type="disabled".
		// Keep Budget=0 so applier can omit budget_tokens.
		config.Budget = 0
		config.Level = ""
	} else {
		// noinspection GoSwitchMissingCasesForIotaConsts — all Mode values are covered.
		switch config.Mode {
		case ModeBudget, ModeAuto, ModeNone:
			config.Budget = clampBudget(config.Budget, modelInfo, toFormat)
		case ModeLevel:
			// Level-based config: budget is irrelevant, no clamping needed.
		}

		// ModeNone with clamped Budget > 0: set Level to lowest for Level-only/Hybrid models
		// This ensures Apply layer doesn't need to access support.Levels
		if config.Mode == ModeNone && config.Budget > 0 && len(support.Levels) > 0 {
			config.Level = Level(support.Levels[0])
		}
	}

	return &config, nil
}

// convertAutoToMidRange converts ModeAuto to a mid-range value when dynamic is not allowed.
//
// This function handles the case where a model does not support dynamic/auto thinking.
// The auto mode is silently converted to a fixed value based on model capability:
//   - Level-only models: convert to ModeLevel with levelMedium
//   - Budget models: convert to ModeBudget with mid = (Min + Max) / 2
//
// Logging:
//   - Debug level when conversion occurs
//   - Fields: original_mode, clamped_to, reason
func convertAutoToMidRange(
	config Config,
	support *registry.ThinkingSupport,
	provider, model string,
) Config {
	// For level-only models (has Levels but no Min/Max range), use ModeLevel with medium
	if len(support.Levels) > 0 && support.Min == 0 && support.Max == 0 {
		config.Mode = ModeLevel
		config.Level = levelMedium
		config.Budget = 0
		log.WithFields(
			log.Fields{
				"provider":      provider,
				"model":         model,
				"original_mode": "auto",
				"clamped_to":    string(levelMedium),
			},
		).Debug("thinking: mode converted, dynamic not allowed, using medium level |")
		return config
	}

	// For budget models, use mid-range budget
	mid := (support.Min + support.Max) / 2
	if mid <= 0 && support.ZeroAllowed {
		config.Mode = ModeNone
		config.Budget = 0
	} else if mid <= 0 {
		config.Mode = ModeBudget
		config.Budget = support.Min
	} else {
		config.Mode = ModeBudget
		config.Budget = mid
	}
	log.WithFields(
		log.Fields{
			"provider":      provider,
			"model":         model,
			"original_mode": "auto",
			"clamped_to":    config.Budget,
		},
	).Debug("thinking: mode converted, dynamic not allowed |")
	return config
}

// standardLevelOrder defines the canonical ordering of thinking levels from lowest to highest.
var standardLevelOrder = []Level{levelMinimal, levelLow, levelMedium, LevelHigh, LevelXHigh}

// clampLevel clamps the given level to the nearest supported level.
// On tie, prefers the lower level.
func clampLevel(level Level, modelInfo *registry.ModelInfo, provider string) Level {
	model := "unknown"
	var supported []string
	if modelInfo != nil {
		if modelInfo.ID != "" {
			model = modelInfo.ID
		}
		if modelInfo.Thinking != nil {
			supported = modelInfo.Thinking.Levels
		}
	}

	if len(supported) == 0 || isLevelSupported(string(level), supported) {
		return level
	}

	pos := levelIndex(string(level))
	if pos == -1 {
		return level
	}
	bestIdx, bestDist := -1, len(standardLevelOrder)+1

	for _, s := range supported {
		if idx := levelIndex(strings.TrimSpace(s)); idx != -1 {
			if dist := abs(pos - idx); dist < bestDist || (dist == bestDist && idx < bestIdx) {
				bestIdx, bestDist = idx, dist
			}
		}
	}

	if bestIdx >= 0 {
		clamped := standardLevelOrder[bestIdx]
		log.WithFields(
			log.Fields{
				"provider":       provider,
				"model":          model,
				"original_value": string(level),
				"clamped_to":     string(clamped),
			},
		).Debug("thinking: level clamped |")
		return clamped
	}
	return level
}

// clampBudget clamps a budget value to the model's supported range.
func clampBudget(value int, modelInfo *registry.ModelInfo, provider string) int {
	model := "unknown"
	support := (*registry.ThinkingSupport)(nil)
	if modelInfo != nil {
		if modelInfo.ID != "" {
			model = modelInfo.ID
		}
		support = modelInfo.Thinking
	}
	if support == nil {
		return value
	}

	// Auto value (-1) passes through without clamping.
	if value == -1 {
		return value
	}

	lo, hi := support.Min, support.Max
	if value == 0 && !support.ZeroAllowed {
		log.WithFields(
			log.Fields{
				"provider":       provider,
				"model":          model,
				"original_value": value,
				"clamped_to":     lo,
				"min":            lo,
				"max":            hi,
			},
		).Warn("thinking: budget zero not allowed |")
		return lo
	}

	// Some models are level-only and do not define numeric budget ranges.
	if lo == 0 && hi == 0 {
		return value
	}

	if value < lo {
		if value == 0 && support.ZeroAllowed {
			return 0
		}
		logClamp(provider, model, value, lo, lo, hi)
		return lo
	}
	if value > hi {
		logClamp(provider, model, value, hi, lo, hi)
		return hi
	}
	return value
}

// isLevelSupported reports whether the given level is present in the supported levels list.
func isLevelSupported(level string, supported []string) bool {
	for _, s := range supported {
		if strings.EqualFold(level, strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

func levelIndex(level string) int {
	for i, l := range standardLevelOrder {
		if strings.EqualFold(level, string(l)) {
			return i
		}
	}
	return -1
}

func normalizeLevels(levels []string) []string {
	out := make([]string, len(levels))
	for i, l := range levels {
		out[i] = strings.ToLower(strings.TrimSpace(l))
	}
	return out
}

func isBudgetCapableProvider(provider string) bool {
	switch provider {
	case "gemini", "gemini-cli", "antigravity", "claude":
		return true
	default:
		return false
	}
}

func isLevelBasedProvider(provider string) bool {
	switch provider {
	case "openai", "openai-response", "codex":
		return true
	default:
		return false
	}
}

func isGeminiFamily(provider string) bool {
	switch provider {
	case "gemini", "gemini-cli", "antigravity":
		return true
	default:
		return false
	}
}

func isSameProviderFamily(from, to string) bool {
	if from == to {
		return true
	}
	return isGeminiFamily(from) && isGeminiFamily(to)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func logClamp(provider, model string, original, clampedTo, lo, hi int) {
	log.WithFields(
		log.Fields{
			"provider":       provider,
			"model":          model,
			"original_value": original,
			"min":            lo,
			"max":            hi,
			"clamped_to":     clampedTo,
		},
	).Debug("thinking: budget clamped |")
}
