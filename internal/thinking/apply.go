// Package thinking provides unified thinking configuration processing.
package thinking

import (
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type pluginProviderApplier struct {
	owner    string
	priority int
	applier  ProviderApplier
}

// providerAppliers maps native provider names to their ProviderApplier implementations.
var providerAppliers = map[string]ProviderApplier{
	"gemini":      nil,
	"gemini-cli":  nil,
	"claude":      nil,
	"openai":      nil,
	"codex":       nil,
	"xai":         nil,
	"iflow":       nil,
	"antigravity": nil,
	"kimi":        nil,
}

var (
	providerAppliersMu     sync.RWMutex
	pluginProviderAppliers = map[string]pluginProviderApplier{}
)

// getProviderApplier returns the ProviderApplier for the given provider name.
// Returns nil if the provider is not registered.
func getProviderApplier(provider string) ProviderApplier {
	providerAppliersMu.RLock()
	defer providerAppliersMu.RUnlock()
	if applier := providerAppliers[provider]; applier != nil {
		return applier
	}
	return pluginProviderAppliers[provider].applier
}

// RegisterProvider registers a provider applier by name.
func RegisterProvider(name string, applier ProviderApplier) {
	providerAppliersMu.Lock()
	defer providerAppliersMu.Unlock()
	providerAppliers[name] = applier
}

// RegisterPluginProvider registers a plugin-owned provider applier.
func RegisterPluginProvider(owner string, name string, priority int, applier ProviderApplier) bool {
	owner = strings.TrimSpace(owner)
	name = strings.ToLower(strings.TrimSpace(name))
	if owner == "" || name == "" || applier == nil {
		return false
	}
	providerAppliersMu.Lock()
	defer providerAppliersMu.Unlock()
	if _, native := providerAppliers[name]; native {
		return false
	}
	current, exists := pluginProviderAppliers[name]
	if exists && current.priority > priority {
		return false
	}
	pluginProviderAppliers[name] = pluginProviderApplier{owner: owner, priority: priority, applier: applier}
	return true
}

// UnregisterPluginProviders removes all thinking providers owned by a plugin.
func UnregisterPluginProviders(owner string) {
	owner = strings.TrimSpace(owner)
	providerAppliersMu.Lock()
	defer providerAppliersMu.Unlock()
	for provider, record := range pluginProviderAppliers {
		if record.owner == owner {
			delete(pluginProviderAppliers, provider)
		}
	}
}

// ClearPluginProviders removes all plugin-owned thinking providers.
func ClearPluginProviders() {
	providerAppliersMu.Lock()
	defer providerAppliersMu.Unlock()
	pluginProviderAppliers = map[string]pluginProviderApplier{}
}

// IsUserDefinedModel reports whether the model is a user-defined model that should
// have thinking configuration passed through without validation.
//
// User-defined models are configured via config file's models[] array
// (e.g., openai-compatibility.*.models[], *-api-key.models[]). These models
// are marked with UserDefined=true at registration time.
//
// User-defined models should have their thinking configuration applied directly,
// letting the upstream service validate the configuration.
func IsUserDefinedModel(modelInfo *registry.ModelInfo) bool {
	if modelInfo == nil {
		return true
	}
	return modelInfo.UserDefined
}

// ApplyThinking applies thinking configuration to a request body.
//
// This is the unified entry point for all providers. It follows the processing
// order defined in FR25: route check → model capability query → config extraction
// → validation → application.
//
// Suffix Priority: When the model name includes a thinking suffix (e.g., "gemini-2.5-pro(8192)"),
// the suffix configuration takes priority over any thinking parameters in the request body.
// This enables users to override thinking settings via the model name without modifying their
// request payload.
//
// Parameters:
//   - body: Original request body JSON
//   - model: Model name, optionally with thinking suffix (e.g., "claude-sonnet-4-5(16384)")
//   - fromFormat: Source request format, such as OpenAI, Codex, or Gemini
//   - toFormat: Target provider format for the request body
//   - providerKey: Provider identifier used for registry model lookups; it may differ from toFormat
//
// Returns:
//   - Modified request body JSON with thinking configuration applied
//   - Error if validation fails (Error). On error, the original body
//     is returned (not nil) to enable defensive programming patterns.
//
// Passthrough behavior (returns original body without error):
//   - Unknown provider (not in providerAppliers map)
//   - modelInfo.Thinking is nil (model doesn't support thinking)
//
// Note: Unknown models (modelInfo is nil) are treated as user-defined models: we skip
// validation and still apply the thinking config so the upstream can validate it.
//
// Example:
//
//	// With suffix - suffix config takes priority
//	result, err := thinking.ApplyThinking(body, "gemini-2.5-pro(8192)", "gemini", "gemini", "gemini")
//
//	// Without suffix - uses body config
//	result, err := thinking.ApplyThinking(body, "gemini-2.5-pro", "gemini", "gemini", "gemini")
func ApplyThinking(body []byte, model string, fromFormat string, toFormat string, providerKey string) ([]byte, error) {
	summaryConfig := ExtractSummaryConfig(body, toFormat)
	return applyThinking(body, nil, model, fromFormat, toFormat, providerKey, nil, false, summaryConfig, false)
}

// ApplyThinkingWithSummary applies canonical thinking effort while preserving
// summary visibility extracted from the original source request.
func ApplyThinkingWithSummary(
	body []byte,
	model string,
	fromFormat string,
	toFormat string,
	providerKey string,
	summaryConfig SummaryConfig,
) ([]byte, error) {
	return applyThinking(body, nil, model, fromFormat, toFormat, providerKey, nil, false, summaryConfig, false)
}

// ApplyThinkingWithModelInfoAndSummary applies the exact configured model
// definition selected for an API-key execution attempt.
func ApplyThinkingWithModelInfoAndSummary(
	body []byte,
	sourceBody []byte,
	model string,
	fromFormat string,
	toFormat string,
	providerKey string,
	modelInfo *registry.ModelInfo,
	summaryConfig SummaryConfig,
	normalizedUpdatesChanged ...bool,
) ([]byte, error) {
	updatesChanged := len(normalizedUpdatesChanged) > 0 && normalizedUpdatesChanged[0]
	return applyThinking(body, sourceBody, model, fromFormat, toFormat, providerKey, modelInfo, true, summaryConfig, updatesChanged)
}

func applyThinking(
	body []byte,
	sourceBody []byte,
	model string,
	fromFormat string,
	toFormat string,
	providerKey string,
	resolvedModelInfo *registry.ModelInfo,
	modelInfoResolved bool,
	summaryConfig SummaryConfig,
	updatesChanged bool,
) ([]byte, error) {
	providerFormat := strings.ToLower(strings.TrimSpace(toFormat))
	if providerFormat == "openai-response" {
		providerFormat = "codex"
	}
	providerKey = strings.ToLower(strings.TrimSpace(providerKey))
	if providerKey == "" {
		providerKey = providerFormat
	}
	fromFormat = strings.ToLower(strings.TrimSpace(fromFormat))
	if fromFormat == "" {
		fromFormat = providerFormat
	}
	// 1. Parse suffix and get modelInfo
	suffixResult := ParseSuffix(model)
	baseModel := suffixResult.ModelName
	// Use provider-specific lookup unless the auth scheduler bound the exact
	// configured API-key model selected for this execution attempt.
	modelInfo := resolvedModelInfo
	if !modelInfoResolved {
		modelInfo = registry.LookupModelInfo(baseModel, providerKey)
	}
	responsesSource := fromFormat == "codex" || fromFormat == "openai-response"
	responsesTarget := providerFormat == "codex" || providerFormat == "xai"
	supportsUpdates := modelInfo != nil && modelInfo.SupportConfigurationUpdate
	var sourceConfig Config
	if responsesSource && !suffixResult.HasSuffix {
		sourceRequest := body
		if !updatesChanged && len(sourceBody) > 0 {
			sourceRequest = sourceBody
		}
		if !updatesChanged || responsesTarget {
			sourceConfig = extractCodexUsageConfig(sourceRequest)
		}
	}
	if responsesTarget && !supportsUpdates {
		var err error
		body, err = stripConfigurationUpdates(body)
		if err != nil {
			return body, err
		}
	}
	nativeResponses := responsesTarget && responsesSource && supportsUpdates
	// 2. Route check: Get provider applier
	applier := getProviderApplier(providerFormat)
	if applier == nil {
		log.WithFields(log.Fields{"provider": providerFormat, "model": model}).Debug("thinking: unknown provider, passthrough |")
		return body, nil
	}
	if !suffixResult.HasSuffix && len(sourceBody) > 0 && responsesSource && !gjson.ValidBytes(body) && hasConfig(extractConfigurationUpdateConfig(sourceBody)) {
		return body, nil
	}
	if nativeResponses && !suffixResult.HasSuffix {
		if log.IsLevelEnabled(log.DebugLevel) && (modelInfo.Thinking != nil || modelInfo.UserDefined) {
			if config := extractCodexUsageConfig(body); hasConfig(config) {
				fields := log.Fields{
					"provider": providerFormat, "model": modelInfo.ID,
					"mode": config.Mode, "budget": config.Budget, "level": config.Level,
				}
				if baseline := extractCodexConfig(body); baseline.Mode == ModeLevel {
					fields["baseline_level"] = baseline.Level
				}
				entry := log.WithFields(fields)
				entry.Debug("thinking: original config from request |")
				entry.Debug("thinking: processed config to apply |")
			}
		}
		return body, nil
	}

	// 3. Model capability check
	// Unknown models are treated as user-defined so thinking config can still be applied.
	// The upstream service is responsible for validating the configuration.
	if IsUserDefinedModel(modelInfo) {
		return applyUserDefinedModel(
			body,
			modelInfo,
			fromFormat,
			providerFormat,
			providerKey,
			suffixResult,
			sourceConfig,
			nativeResponses,
			summaryConfig,
		)
	}
	// noinspection GoMaybeNil — modelInfo != nil guaranteed: IsUserDefinedModel returns true for nil.
	if modelInfo.Thinking == nil {
		config := sourceConfig
		if !hasConfig(config) {
			config = extractConfig(body, providerFormat)
		}
		if hasConfig(config) || summaryConfig.Mode != SummaryUnspecified {
			log.WithFields(
				log.Fields{
					"model":    baseModel,
					"provider": providerFormat,
				},
			).Debug("thinking: model does not support thinking, stripping config |")
			if responsesTarget {
				return stripResponsesEffort(body)
			}
			return stripThinkingConfig(body, providerFormat), nil
		}
		log.WithFields(
			log.Fields{
				"provider": providerFormat,
				"model":    baseModel,
			},
		).Debug("thinking: model does not support thinking, passthrough |")
		return body, nil
	}

	// 4. Get config: suffix priority over body
	var config Config
	if suffixResult.HasSuffix {
		config = parseSuffixToConfig(suffixResult.RawSuffix, providerFormat, model)
		log.WithFields(
			log.Fields{
				"provider": providerFormat,
				"model":    model,
				"mode":     config.Mode,
				"budget":   config.Budget,
				"level":    config.Level,
			},
		).Debug("thinking: config from model suffix |")
	} else {
		config = sourceConfig
		if !hasConfig(config) && !updatesChanged && modelInfoResolved && len(sourceBody) > 0 {
			config = extractSourceConfig(sourceBody, fromFormat)
		}
		if !hasConfig(config) {
			config = extractConfig(body, providerFormat)
		}
		if hasConfig(config) {
			log.WithFields(
				log.Fields{
					"provider": providerFormat,
					"model":    modelInfo.ID,
					"mode":     config.Mode,
					"budget":   config.Budget,
					"level":    config.Level,
				},
			).Debug("thinking: original config from request |")
		}
	}

	if !hasConfig(config) {
		log.WithFields(
			log.Fields{
				"provider": providerFormat,
				"model":    modelInfo.ID,
			},
		).Debug("thinking: no config found, passthrough |")
		if nativeResponses {
			return body, nil
		}
		if modelInfoResolved && providerFormat == "claude" && fromFormat != providerFormat &&
			ExtractSummaryConfig(sourceBody, fromFormat).Mode == SummaryEnabled {
			body = stripInferredClaudeSummaryActivation(body, modelInfo)
		}
		return applySummaryConfigForProvider(
			body,
			providerFormat,
			baseModel,
			providerKey,
			modelInfo,
			summaryConfig,
		), nil
	}

	if modelInfoResolved && config.Mode == ModeLevel {
		modelType := strings.ToLower(strings.TrimSpace(modelInfo.Type))
		if fromFormat != providerFormat || modelType != "" && !isSameProviderFamily(providerFormat, modelType) {
			level := Level(strings.ToLower(strings.TrimSpace(string(config.Level))))
			if level == LevelXHigh || level == LevelMax {
				candidates := []Level{level, LevelMax, LevelHigh}
				if level == LevelMax {
					candidates[1] = LevelXHigh
				}
				for _, candidate := range candidates {
					if isLevelSupported(string(candidate), modelInfo.Thinking.Levels) {
						config.Level = candidate
						break
					}
				}
			}
		}
	}

	// 5. Validate and normalize configuration
	validated, err := validateConfig(config, modelInfo, fromFormat, providerFormat, suffixResult.HasSuffix)
	if err != nil {
		log.WithFields(
			log.Fields{
				"provider": providerFormat,
				"model":    modelInfo.ID,
				"error":    err.Error(),
			},
		).Warn("thinking: validation failed |")
		// Return original body on validation failure (defensive programming).
		// This ensures callers who ignore the error won't receive nil body.
		// The upstream service will decide how to handle the unmodified request.
		return body, err
	}

	// Defensive check: ValidateConfig should never return (nil, nil)
	if validated == nil {
		log.WithFields(
			log.Fields{
				"provider": providerFormat,
				"model":    modelInfo.ID,
			},
		).Warn("thinking: ValidateConfig returned nil config without error, passthrough |")
		return body, nil
	}

	log.WithFields(
		log.Fields{
			"provider": providerFormat,
			"model":    modelInfo.ID,
			"mode":     validated.Mode,
			"budget":   validated.Budget,
			"level":    validated.Level,
		},
	).Debug("thinking: processed config to apply |")

	// 6. Apply the provider-specific thinking amount, then restore the explicit
	// summary visibility selected independently of effort
	applied, err := applier.Apply(body, *validated, modelInfo)
	if err != nil {
		return applied, err
	}
	if thinkingIsFullyDisabled(*validated) || nativeResponses {
		return applied, nil
	}
	return applySummaryConfigForProvider(
		applied,
		providerFormat,
		baseModel,
		providerKey,
		modelInfo,
		summaryConfig,
	), nil
}

func thinkingIsFullyDisabled(config Config) bool {
	return config.Mode == ModeNone && config.Budget == 0 && config.Level == ""
}

// parseSuffixToConfig converts a raw suffix string to Config.
//
// Parsing priority:
//  1. Special values: "none" → ModeNone, "auto"/"-1" → ModeAuto
//  2. Level names: "minimal", "low", "medium", "high", "xhigh" → ModeLevel
//  3. Numeric values: positive integers → ModeBudget, 0 → ModeNone
//
// If none of the above match, returns empty Config (treated as no config).
func parseSuffixToConfig(rawSuffix, provider, model string) Config {
	// 1. Try special values first (none, auto, -1)
	if mode, ok := parseSpecialSuffix(rawSuffix); ok {
		// noinspection GoSwitchMissingCasesForIotaConsts — parseSpecialSuffix only returns ModeNone or ModeAuto on success.
		switch mode {
		case ModeNone:
			return Config{Mode: ModeNone, Budget: 0}
		case ModeAuto:
			return Config{Mode: ModeAuto, Budget: -1}
		default:
			// parseSpecialSuffix only returns ModeNone or ModeAuto on success; unreachable.
		}
	}

	// 2. Try level parsing (minimal, low, medium, high, xhigh)
	if level, ok := parseLevelSuffix(rawSuffix); ok {
		return Config{Mode: ModeLevel, Level: level}
	}

	// 3. Try numeric parsing
	if budget, ok := parseNumericSuffix(rawSuffix); ok {
		if budget == 0 {
			return Config{Mode: ModeNone, Budget: 0}
		}
		return Config{Mode: ModeBudget, Budget: budget}
	}

	// Unknown suffix format - return empty config
	log.WithFields(
		log.Fields{
			"provider":   provider,
			"model":      model,
			"raw_suffix": rawSuffix,
		},
	).Debug("thinking: unknown suffix format, treating as no config |")
	return Config{}
}

// applyUserDefinedModel applies thinking configuration for user-defined models
// without ThinkingSupport validation.
func applyUserDefinedModel(
	body []byte,
	modelInfo *registry.ModelInfo,
	fromFormat, toFormat, providerKey string,
	suffixResult SuffixResult,
	sourceConfig Config,
	nativeResponses bool,
	summaryConfig SummaryConfig,
) ([]byte, error) {
	// Get model ID for logging
	modelID := ""
	if modelInfo != nil {
		modelID = modelInfo.ID
	} else {
		modelID = suffixResult.ModelName
	}

	// Get config: suffix priority over body
	var config Config
	if suffixResult.HasSuffix {
		config = parseSuffixToConfig(suffixResult.RawSuffix, toFormat, modelID)
	} else {
		config = sourceConfig
		if !hasConfig(config) {
			config = extractConfig(body, toFormat)
		}
	}

	if !hasConfig(config) {
		log.WithFields(
			log.Fields{
				"model":    modelID,
				"provider": toFormat,
			},
		).Debug("thinking: user-defined model, passthrough (no config) |")
		if nativeResponses {
			return body, nil
		}
		return applySummaryConfigForProvider(
			body,
			toFormat,
			modelID,
			providerKey,
			modelInfo,
			summaryConfig,
		), nil
	}

	applier := getProviderApplier(toFormat)
	if applier == nil {
		log.WithFields(
			log.Fields{
				"model":    modelID,
				"provider": toFormat,
			},
		).Debug("thinking: user-defined model, passthrough (unknown provider) |")
		return body, nil
	}

	log.WithFields(
		log.Fields{
			"provider": toFormat,
			"model":    modelID,
			"mode":     config.Mode,
			"budget":   config.Budget,
			"level":    config.Level,
		},
	).Debug("thinking: applying config for user-defined model (skip validation)")

	config = normalizeUserDefinedConfig(config, fromFormat, toFormat, suffixResult.HasSuffix)
	applied, err := applier.Apply(body, config, modelInfo)
	if err != nil {
		return applied, err
	}
	if thinkingIsFullyDisabled(config) || nativeResponses {
		return applied, nil
	}
	return applySummaryConfigForProvider(
		applied,
		toFormat,
		modelID,
		providerKey,
		modelInfo,
		summaryConfig,
	), nil
}

func normalizeUserDefinedConfig(config Config, fromFormat, toFormat string, fromSuffix bool) Config {
	if config.Mode != ModeLevel || toFormat == "claude" && !fromSuffix {
		return config
	}
	if !isBudgetCapableProvider(toFormat) || !isLevelBasedProvider(fromFormat) {
		return config
	}
	budget, ok := ConvertLevelToBudget(string(config.Level))
	if !ok {
		return config
	}
	config.Mode = ModeBudget
	config.Budget = budget
	config.Level = ""
	return config
}

// extractConfig extracts provider-specific thinking config from request body.
func extractSourceConfig(body []byte, provider string) Config {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "openai-response" {
		provider = "codex"
	}
	return extractConfig(body, provider)
}

func extractConfig(body []byte, provider string) Config {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return Config{}
	}

	switch provider {
	case "claude":
		return extractClaudeConfig(body)
	case "gemini", "gemini-cli", "antigravity":
		return extractGeminiConfig(body, provider)
	case "openai":
		return extractOpenAIConfig(body)
	case "codex", "xai":
		return extractCodexConfig(body)
	case "iflow":
		config := extractIFlowConfig(body)
		if hasConfig(config) {
			return config
		}
		return extractOpenAIConfig(body)
	case "kimi", "kimi-ai":
		return extractKimiConfig(body)
	default:
		return Config{}
	}
}

func hasConfig(config Config) bool {
	return config.Mode != ModeBudget || config.Budget != 0 || config.Level != ""
}

// ExtractReasoningEffort returns the request's thinking setting as a canonical reasoning_effort label.
func ExtractReasoningEffort(body []byte, provider, model string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "codex" || provider == "xai" || provider == "openai-response" {
		if effort := reasoningEffortFromConfig(extractConfigurationUpdateConfig(body)); effort != "" {
			return effort
		}
	}
	if effort := reasoningEffortFromSuffix(ParseSuffix(model)); effort != "" {
		return effort
	}

	return ExtractTranslatedReasoningEffort(body, provider)
}

// ExtractTranslatedReasoningEffort returns the final provider payload's thinking setting.
func ExtractTranslatedReasoningEffort(body []byte, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	config := extractConfigForUsage(body, provider)
	if !hasConfig(config) {
		switch provider {
		case "openai", "openai-response":
			config = extractCodexUsageConfig(body)
			if !hasConfig(config) {
				config = extractOpenAIConfig(body)
			}
		}
	}
	return reasoningEffortFromConfig(config)
}

func extractConfigForUsage(body []byte, provider string) Config {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "xai", "openai-response":
		return extractCodexUsageConfig(body)
	default:
		return extractConfig(body, provider)
	}
}

func reasoningEffortFromSuffix(suffix SuffixResult) string {
	if !suffix.HasSuffix {
		return ""
	}
	return reasoningEffortFromConfig(parseSuffixToConfig(suffix.RawSuffix, "", suffix.ModelName))
}

func reasoningEffortFromConfig(config Config) string {
	if !hasConfig(config) {
		return ""
	}
	switch config.Mode {
	case ModeNone:
		return string(LevelNone)
	case ModeAuto:
		return string(LevelAuto)
	case ModeLevel:
		return strings.ToLower(strings.TrimSpace(string(config.Level)))
	case ModeBudget:
		level, ok := ConvertBudgetToLevel(config.Budget)
		if !ok {
			return ""
		}
		return level
	default:
		return ""
	}
}

// extractClaudeConfig extracts thinking configuration from Claude format request body.
//
// Claude API format:
//   - thinking.type: "enabled", "adaptive", "auto", or "disabled"
//   - thinking.budget_tokens: integer (-1=auto, 0=disabled, >0=budget)
//   - output_config.effort: discrete effort for adaptive thinking
//
// 显式禁用优先于其他字段，enabled 的预算优先于 effort
// enabled 未给预算时保留显式 effort，未给 effort 时使用 ModeAuto
func extractClaudeConfig(body []byte) Config {
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	if thinkingType == "disabled" {
		return Config{Mode: ModeNone, Budget: 0}
	}
	if thinkingType == "adaptive" || thinkingType == "auto" ||
		thinkingType == "enabled" && !gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
		effort := gjson.GetBytes(body, "output_config.effort")
		if !effort.Exists() || effort.Type != gjson.String || strings.TrimSpace(effort.String()) == "" {
			if thinkingType == "enabled" {
				return Config{Mode: ModeAuto, Budget: -1}
			}
			return Config{}
		}
		switch value := strings.ToLower(strings.TrimSpace(effort.String())); value {
		case "none":
			return Config{Mode: ModeNone, Budget: 0}
		case "auto":
			return Config{Mode: ModeAuto, Budget: -1}
		default:
			return Config{Mode: ModeLevel, Level: Level(value)}
		}
	}

	// Check budget_tokens
	if budget := gjson.GetBytes(body, "thinking.budget_tokens"); budget.Exists() {
		value := int(budget.Int())
		switch value {
		case 0:
			return Config{Mode: ModeNone, Budget: 0}
		case -1:
			return Config{Mode: ModeAuto, Budget: -1}
		default:
			return Config{Mode: ModeBudget, Budget: value}
		}
	}

	return Config{}
}

// extractGeminiConfig extracts thinking configuration from Gemini format request body.
//
// Gemini API format:
//   - generationConfig.thinkingConfig.thinkingLevel: "none", "auto", or level name (Gemini 3)
//   - generationConfig.thinkingConfig.thinkingBudget: integer (Gemini 2.5)
//
// For gemini-cli and antigravity providers, the path is prefixed with `request.`.
//
// Priority: thinkingLevel is checked first (Gemini 3 format), then thinkingBudget (Gemini 2.5 format).
// This allows newer Gemini 3 level-based configs to take precedence.
func extractGeminiConfig(body []byte, provider string) Config {
	prefix := "generationConfig.thinkingConfig"
	if provider == "gemini-cli" || provider == "antigravity" {
		prefix = "request.generationConfig.thinkingConfig"
	}

	// Check thinkingLevel first (Gemini 3 format takes precedence)
	level := gjson.GetBytes(body, prefix+".thinkingLevel")
	if !level.Exists() {
		// Google official Gemini Python SDK sends snake_case field names
		level = gjson.GetBytes(body, prefix+".thinking_level")
	}
	if level.Exists() {
		value := level.String()
		switch value {
		case "none":
			return Config{Mode: ModeNone, Budget: 0}
		case "auto":
			return Config{Mode: ModeAuto, Budget: -1}
		default:
			return Config{Mode: ModeLevel, Level: Level(value)}
		}
	}

	// Check thinkingBudget (Gemini 2.5 format)
	budget := gjson.GetBytes(body, prefix+".thinkingBudget")
	if !budget.Exists() {
		// Google official Gemini Python SDK sends snake_case field names
		budget = gjson.GetBytes(body, prefix+".thinking_budget")
	}
	if budget.Exists() {
		value := int(budget.Int())
		switch value {
		case 0:
			return Config{Mode: ModeNone, Budget: 0}
		case -1:
			return Config{Mode: ModeAuto, Budget: -1}
		default:
			return Config{Mode: ModeBudget, Budget: value}
		}
	}

	return Config{}
}

// extractOpenAIConfig extracts thinking configuration from OpenAI format request body.
//
// OpenAI API format:
//   - reasoning_effort: "none", "low", "medium", "high" (discrete levels)
//
// OpenAI uses level-based thinking configuration only, no numeric budget support.
// The "none" value is treated specially to return ModeNone.
func extractOpenAIConfig(body []byte) Config {
	// Check reasoning_effort (OpenAI Chat Completions format)
	if effort := gjson.GetBytes(body, "reasoning_effort"); effort.Exists() {
		value := effort.String()
		if value == "none" {
			return Config{Mode: ModeNone, Budget: 0}
		}
		return Config{Mode: ModeLevel, Level: Level(value)}
	}

	return Config{}
}

// extractKimiConfig extracts Kimi's native thinking object while retaining
// reasoning_effort as a legacy input fallback. Native fields take precedence.
func extractKimiConfig(body []byte) Config {
	thinkingType := gjson.GetBytes(body, "thinking.type")
	if thinkingType.Exists() {
		switch strings.ToLower(strings.TrimSpace(thinkingType.String())) {
		case "disabled":
			return Config{Mode: ModeNone, Budget: 0}
		case "enabled":
			if !gjson.GetBytes(body, "thinking.effort").Exists() {
				return Config{}
			}
		}
	}

	if effort := gjson.GetBytes(body, "thinking.effort"); effort.Exists() {
		value := strings.ToLower(strings.TrimSpace(effort.String()))
		switch value {
		case "":
			return Config{}
		case "none":
			return Config{Mode: ModeNone, Budget: 0}
		case "auto":
			return Config{Mode: ModeAuto, Budget: -1}
		default:
			return Config{Mode: ModeLevel, Level: Level(value)}
		}
	}

	if thinkingType.Exists() {
		return Config{}
	}
	return extractOpenAIConfig(body)
}

// extractCodexConfig extracts thinking configuration from Codex format request body.
//
// Codex API format (OpenAI Responses API):
//   - reasoning.effort: "none", "low", "medium", "high"
//
// This is similar to OpenAI but uses nested field "reasoning.effort" instead of "reasoning_effort".
func extractCodexConfig(body []byte) Config {
	// Check reasoning.effort (Codex / OpenAI Responses API format)
	if effort := gjson.GetBytes(body, "reasoning.effort"); effort.Exists() {
		value := effort.String()
		if value == "none" {
			return Config{Mode: ModeNone, Budget: 0}
		}
		return Config{Mode: ModeLevel, Level: Level(value)}
	}

	return Config{}
}

// extractCodexUsageConfig reads the last applicable configuration_update
// reasoning effort, falling back to the request-level value when no update sets it.
func extractCodexUsageConfig(body []byte) Config {
	if !gjson.ValidBytes(body) {
		return Config{}
	}
	if config := extractConfigurationUpdateConfig(body); hasConfig(config) {
		return config
	}
	return extractCodexConfig(body)
}

func extractConfigurationUpdateConfig(body []byte) Config {
	if !gjson.ValidBytes(body) {
		return Config{}
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return Config{}
	}
	items := input.Array()
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Get("type").String() != "configuration_update" {
			continue
		}
		effort := items[i].Get("reasoning.effort")
		if effort.Type != gjson.String {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(effort.String()))
		switch value {
		case "none":
			return Config{Mode: ModeNone, Budget: 0}
		case "auto":
			return Config{Mode: ModeAuto, Budget: -1}
		case "max":
			return Config{Mode: ModeLevel, Level: LevelMax}
		default:
			if level, ok := parseLevelSuffix(value); ok {
				return Config{Mode: ModeLevel, Level: level}
			}
		}
	}
	return Config{}
}

// extractIFlowConfig extracts thinking configuration from iFlow format request body.
//
// iFlow API format (supports multiple model families):
//   - GLM format: chat_template_kwargs.enable_thinking (boolean)
//   - MiniMax format: reasoning_split (boolean)
//
// Returns ModeBudget with Budget=1 as a sentinel value indicating "enabled".
// The actual budget/configuration is determined by the iFlow applier based on model capabilities.
// Budget=1 is used because iFlow models don't use numeric budgets; they only support on/off.
func extractIFlowConfig(body []byte) Config {
	// GLM format: chat_template_kwargs.enable_thinking
	if enabled := gjson.GetBytes(body, "chat_template_kwargs.enable_thinking"); enabled.Exists() {
		if enabled.Bool() {
			// Budget=1 is a sentinel meaning "enabled" (iFlow doesn't use numeric budgets)
			return Config{Mode: ModeBudget, Budget: 1}
		}
		return Config{Mode: ModeNone, Budget: 0}
	}

	// MiniMax format: reasoning_split
	if split := gjson.GetBytes(body, "reasoning_split"); split.Exists() {
		if split.Bool() {
			// Budget=1 is a sentinel meaning "enabled" (iFlow doesn't use numeric budgets)
			return Config{Mode: ModeBudget, Budget: 1}
		}
		return Config{Mode: ModeNone, Budget: 0}
	}

	return Config{}
}
