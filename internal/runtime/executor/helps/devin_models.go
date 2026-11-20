package helps

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
)

type devinModelSuffix struct {
	suffix   string
	effort   string
	priority bool
}

var devinModelSuffixes = []devinModelSuffix{
	{suffix: "-medium-priority", effort: "medium", priority: true},
	{suffix: "-xhigh-priority", effort: "xhigh", priority: true},
	{suffix: "-high-priority", effort: "high", priority: true},
	{suffix: "-low-priority", effort: "low", priority: true},
	{suffix: "-max-priority", effort: "max", priority: true},
	{suffix: "-priority", priority: true},
	{suffix: "-minimal", effort: "minimal"},
	{suffix: "-medium", effort: "medium"},
	{suffix: "-xhigh", effort: "xhigh"},
	{suffix: "-high", effort: "high"},
	{suffix: "-none", effort: "none"},
	{suffix: "-fast", effort: "fast"},
	{suffix: "-low", effort: "low"},
	{suffix: "-max", effort: "max"},
}

var specialDevinAliases = map[string]string{
	"claude-haiku-4-5": "MODEL_PRIVATE_11",
	"gpt-4-1":          "MODEL_CHAT_GPT_4_1_2025_04_14",
}

var devinStandardLevelOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// hasDevinEffortSuffix reports whether model ends with a recognized Devin UID suffix.
func hasDevinEffortSuffix(model string) bool {
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), "-slow") {
		return true
	}
	_, _, _, found := splitDevinModelSuffix(model)
	return found
}

// normalizeThinkingLevel converts a request effort or token budget to a Devin effort.
func normalizeThinkingLevel(level string, budgetTokens int) string {
	normalized := strings.ToLower(strings.TrimSpace(level))
	switch normalized {
	case "minimal", "low", "medium", "high", "xhigh", "max", "fast":
		return normalized
	case "none", "off", "disabled", "0":
		return "none"
	case "auto", "adaptive", "-1":
		return "high"
	}
	if parsed, err := strconv.Atoi(normalized); err == nil && parsed > 0 {
		budgetTokens = parsed
	}
	if budgetTokens <= 0 {
		return ""
	}
	switch {
	case budgetTokens <= 4096:
		return "low"
	case budgetTokens <= 16384:
		return "medium"
	case budgetTokens <= 32768:
		return "high"
	default:
		return "max"
	}
}

// ResolveDevinChatModel resolves a namespaced public model and requested effort
// to an upstream chat_model_uid and its catalog metadata.
func ResolveDevinChatModel(rawModel string, thinkingLevel string, budgetTokens int) (
	string,
	*registry.ModelInfo,
	error,
) {
	model := strings.TrimSpace(rawModel)
	if model == "" {
		model = "devin/swe-2"
	}
	if strings.HasPrefix(strings.ToLower(model), "devin/") {
		model = model[len("devin/"):]
	}

	parsedSuffix := thinking.ParseSuffix(model)
	model = strings.TrimSpace(parsedSuffix.ModelName)
	hasRequestSuffix := parsedSuffix.HasSuffix
	if parsedSuffix.HasSuffix {
		thinkingLevel = parsedSuffix.RawSuffix
	} else if colon := strings.LastIndex(model, ":"); colon >= 0 {
		thinkingLevel = strings.TrimSpace(model[colon+1:])
		model = strings.TrimSpace(model[:colon])
		hasRequestSuffix = true
	}

	manualUID := model
	baseModel, uidEffort, priority, hasUIDSuffix := splitDevinModelSuffix(model)
	if hasUIDSuffix {
		lowerUID := strings.ToLower(model)
		encodedEffort := strings.HasPrefix(lowerUID, "model_") || strings.HasSuffix(
			lowerUID, "-thinking",
		) || strings.HasSuffix(lowerUID, "-thinking-1m") || lowerUID == "swe-1-6-fast"
		model = baseModel
		if !hasRequestSuffix && (thinkingLevel == "" || encodedEffort) && uidEffort != "" {
			thinkingLevel = uidEffort
		}
	}

	canonicalBase := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), ".", "-")
	if canonicalBase == "" {
		return "", nil, fmt.Errorf("empty Devin model")
	}
	if canonicalBase == "gemini-3-flash" {
		canonicalBase = "gemini-3-8-flash"
	}

	effort := normalizeThinkingLevel(thinkingLevel, budgetTokens)
	legacyID := strings.ReplaceAll(canonicalBase, "-", "_")
	switch legacyID {
	case "model_gpt_5_2":
		levels := []string{"none", "low", "medium", "high", "xhigh"}
		info := &registry.ModelInfo{
			ID: "devin/" + legacyID, Type: "devin", ContextLength: 384000,
			MaxCompletionTokens: 64000, Thinking: &registry.ThinkingSupport{Levels: levels},
		}
		level := clampDevinEffort(effort, levels, "low")
		return "MODEL_GPT_5_2_" + strings.ToUpper(level), info, nil
	case "model_google_gemini_3_0_flash":
		levels := []string{"minimal", "low", "medium", "high"}
		info := &registry.ModelInfo{
			ID: "devin/" + legacyID, Type: "devin", ContextLength: 1048576,
			MaxCompletionTokens: 64000, Thinking: &registry.ThinkingSupport{Levels: levels},
		}
		level := clampDevinEffort(effort, levels, "high")
		return "MODEL_GOOGLE_GEMINI_3_0_FLASH_" + strings.ToUpper(level), info, nil
	case "model_claude_4_5_opus":
		info := &registry.ModelInfo{
			ID: "devin/" + legacyID, Type: "devin", ContextLength: 200000,
			MaxCompletionTokens: 64000, Thinking: &registry.ThinkingSupport{Levels: []string{"high"}},
		}
		if effort != "" && effort != "none" {
			return "MODEL_CLAUDE_4_5_OPUS_THINKING", info, nil
		}
		return "MODEL_CLAUDE_4_5_OPUS", info, nil
	case "model_private_2", "model_private_3", "model_private_11", "model_private_12",
		"model_private_13", "model_private_14", "model_private_15", "model_chat_gpt_4_1_2025_04_14":
		contextLength := 200000
		switch legacyID {
		case "model_private_12", "model_private_13", "model_private_14", "model_private_15":
			contextLength = 272000
		case "model_chat_gpt_4_1_2025_04_14":
			contextLength = 1047576
		}
		info := &registry.ModelInfo{
			ID: "devin/" + legacyID, Type: "devin",
			ContextLength: contextLength, MaxCompletionTokens: 64000,
		}
		return strings.ToUpper(legacyID), info, nil
	}

	modelInfo := registry.LookupDevinModel(canonicalBase)
	if modelInfo == nil {
		if strings.HasSuffix(strings.ToLower(manualUID), "-slow") {
			return manualUID, nil, nil
		}
		return "", nil, fmt.Errorf("unknown Devin model %q", rawModel)
	}

	if alias, exists := specialDevinAliases[canonicalBase]; exists {
		return alias, modelInfo, nil
	}
	if canonicalBase == "claude-sonnet-4-5" {
		if effort != "" && effort != "none" {
			return "MODEL_PRIVATE_3", modelInfo, nil
		}
		return "MODEL_PRIVATE_2", modelInfo, nil
	}

	switch canonicalBase {
	case "swe-1-6":
		if effort == "fast" {
			return canonicalBase + "-fast", modelInfo, nil
		}
		return canonicalBase, modelInfo, nil
	case "glm-5-2", "glm-5-2-1m":
		uid := strings.TrimSuffix(canonicalBase, "-1m")
		if effort == "none" || effort == "max" {
			uid += "-" + effort
		}
		if strings.HasSuffix(canonicalBase, "-1m") {
			uid += "-1m"
		}
		return uid, modelInfo, nil
	case "claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-6-1m", "claude-sonnet-4-6-1m":
		uid := strings.TrimSuffix(canonicalBase, "-1m")
		if effort != "" && effort != "none" {
			uid += "-thinking"
		}
		if strings.HasSuffix(canonicalBase, "-1m") {
			uid += "-1m"
		}
		return uid, modelInfo, nil
	}

	allowedLevels := []string(nil)
	if modelInfo.Thinking != nil {
		allowedLevels = modelInfo.Thinking.Levels
	}
	if len(allowedLevels) == 0 {
		return canonicalBase, modelInfo, nil
	}

	uid := canonicalBase
	if effort != "" || !strings.HasPrefix(canonicalBase, "swe-") {
		defaultEffort := selectDefaultDevinEffort(canonicalBase, allowedLevels)
		resolvedEffort := clampDevinEffort(effort, allowedLevels, defaultEffort)
		uid += "-" + resolvedEffort
	}
	if priority {
		uid += "-priority"
	}
	return uid, modelInfo, nil
}

func splitDevinModelSuffix(model string) (string, string, bool, bool) {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	switch lower {
	case "glm-5-2-none-1m":
		return "glm-5-2-1m", "none", false, true
	case "glm-5-2-max-1m":
		return "glm-5-2-1m", "max", false, true
	}
	if strings.HasSuffix(lower, "-thinking-1m") {
		return trimmed[:len(trimmed)-len("-thinking-1m")] + "-1m", "high", false, true
	}
	for _, suffix := range []string{"-thinking", "_thinking"} {
		if strings.HasSuffix(lower, suffix) {
			return trimmed[:len(trimmed)-len(suffix)], "high", false, true
		}
	}
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if suffix := "_" + level; strings.HasSuffix(lower, suffix) {
			return trimmed[:len(trimmed)-len(suffix)], level, false, true
		}
	}
	for _, candidate := range devinModelSuffixes {
		if strings.HasSuffix(lower, candidate.suffix) && len(trimmed) > len(candidate.suffix) {
			return strings.TrimSpace(
				trimmed[:len(trimmed)-len(candidate.suffix)],
			), candidate.effort, candidate.priority, true
		}
	}
	return trimmed, "", false, false
}

func selectDefaultDevinEffort(baseModel string, levels []string) string {
	if strings.Contains(baseModel, "swe-2") && containsDevinLevel(levels, "high") {
		return "high"
	}
	if strings.HasPrefix(baseModel, "gpt-5") &&
		containsDevinLevel(levels, "none") && containsDevinLevel(levels, "low") {
		return "low"
	}
	if containsDevinLevel(levels, "high") &&
		(strings.Contains(baseModel, "gemini") ||
			strings.Contains(baseModel, "grok") ||
			strings.Contains(baseModel, "glm") ||
			strings.Contains(baseModel, "deepseek") ||
			strings.Contains(baseModel, "kimi") ||
			strings.Contains(baseModel, "nemotron")) {
		return "high"
	}
	for _, preferred := range []string{"medium", "high", "low", "none", "minimal", "xhigh", "max", "fast"} {
		if containsDevinLevel(levels, preferred) {
			return preferred
		}
	}
	return strings.ToLower(strings.TrimSpace(levels[0]))
}

func containsDevinLevel(levels []string, wanted string) bool {
	for _, level := range levels {
		if strings.EqualFold(strings.TrimSpace(level), wanted) {
			return true
		}
	}
	return false
}

func clampDevinEffort(requested string, allowed []string, defaultEffort string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return defaultEffort
	}
	for _, level := range allowed {
		if strings.EqualFold(strings.TrimSpace(level), requested) {
			return strings.ToLower(strings.TrimSpace(level))
		}
	}
	if requested == "none" {
		return defaultEffort
	}

	requestedIndex := devinLevelIndex(requested)
	if requestedIndex < 0 {
		return defaultEffort
	}
	bestMatch := defaultEffort
	bestDistance := len(devinStandardLevelOrder) + 1
	bestIndex := -1
	for _, level := range allowed {
		level = strings.ToLower(strings.TrimSpace(level))
		levelIndex := devinLevelIndex(level)
		if levelIndex < 0 {
			continue
		}
		distance := requestedIndex - levelIndex
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance || distance == bestDistance && levelIndex > bestIndex {
			bestMatch = level
			bestDistance = distance
			bestIndex = levelIndex
		}
	}
	return bestMatch
}

func devinLevelIndex(level string) int {
	for index, candidate := range devinStandardLevelOrder {
		if candidate == strings.ToLower(strings.TrimSpace(level)) {
			return index
		}
	}
	return -1
}
