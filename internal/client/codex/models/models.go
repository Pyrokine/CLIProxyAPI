// Package models builds model catalogs for official Codex clients.
package models

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

type codexClientModelsPayload struct {
	Models []map[string]any `json:"models"`
}

// ProvidersForModelFunc returns the providers registered for a model.
type ProvidersForModelFunc func(string) []string

// WebSearchCapabilityForModelFunc returns explicit conservative capability
// metadata for an exact public model ID. nil means unknown.
type WebSearchCapabilityForModelFunc func(string) *bool

var (
	codexClientModelTemplatesMu       sync.Mutex
	codexClientModelTemplatesLoaded   bool
	codexClientModelTemplatesRevision uint64
	codexClientModelTemplates         map[string]map[string]any
	codexClientDefaultTemplate        map[string]any
	codexClientModelTemplatesErr      error
)

var codexClientAllowedReasoningLevels = map[string]struct{}{
	"none":    {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
	"ultra":   {},
}

var codexClientLegacyAllowedReasoningLevels = map[string]struct{}{
	"none":    {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
}

// BuildResponse builds a Codex client model response from available models.
func BuildResponse(
	availableModels []map[string]any,
	providersForModel ProvidersForModelFunc,
	optimizeMultiAgentV2 bool,
) map[string]any {
	return BuildResponseForClient(availableModels, providersForModel, optimizeMultiAgentV2, "")
}

// BuildResponseForClient builds a Codex client model response from available models
// tailored for a specific client version.
func BuildResponseForClient(
	availableModels []map[string]any,
	providersForModel ProvidersForModelFunc,
	optimizeMultiAgentV2 bool,
	clientVersion string,
) map[string]any {
	return BuildResponseForClientWithCPACapabilities(
		availableModels,
		providersForModel,
		registry.GetGlobalRegistry().GetResponsesWebSearchCapability,
		optimizeMultiAgentV2,
		clientVersion,
	)
}

// BuildResponseForClientWithCPACapabilities builds a client response while
// allowing Home to supply capability metadata independent of the local registry.
func BuildResponseForClientWithCPACapabilities(
	availableModels []map[string]any,
	providersForModel ProvidersForModelFunc,
	webSearchCapabilityForModel WebSearchCapabilityForModelFunc,
	optimizeMultiAgentV2 bool,
	clientVersion string,
) map[string]any {
	return map[string]any{
		"models": buildCodexClientModels(
			availableModels,
			providersForModel,
			webSearchCapabilityForModel,
			optimizeMultiAgentV2,
			clientVersion,
		),
	}
}

func buildCodexClientModels(
	models []map[string]any,
	providersForModel ProvidersForModelFunc,
	webSearchCapabilityForModel WebSearchCapabilityForModelFunc,
	optimizeMultiAgentV2 bool,
	clientVersion string,
) []map[string]any {
	templates, defaultTemplate, err := loadCodexClientModelTemplates()
	if err != nil || defaultTemplate == nil {
		return nil
	}

	result := make([]map[string]any, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		id := strings.TrimSpace(stringModelValue(model, "id"))
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		metadataID := codexClientMetadataModelID(id)

		if template, ok := templates[metadataID]; ok {
			entry := cloneCodexClientModelMap(template)
			entry["slug"] = id
			info := registry.LookupModelInfo(id)
			applyCodexClientModelCapabilities(entry, id, metadataID, info, providersForModel, clientVersion)
			applyCodexClientDisplayName(entry, model)
			applyCodexClientDescription(entry, model)
			applyCodexClientBaseInstructions(entry, model)
			applyCodexClientMaxContextLengthOverride(entry, model)
			applyCodexClientMaxTokens(entry, model)
			if thinkingSupport := codexClientThinkingSupport(model); thinkingSupport != nil {
				applyCodexClientThinkingMetadata(entry, thinkingSupport, clientVersion)
			}
			if modalities, explicit := codexClientInputModalities(model); explicit {
				applyCodexClientExplicitInputModalities(entry, modalities)
			}
			applyCodexClientProviderCapabilities(entry, id, true, providersForModel)
			applyCPAWebSearchCapability(entry, id, webSearchCapabilityForModel, clientVersion)
			sanitizeCodexClientReasoningMetadata(entry, clientVersion)
			applyCodexClientVisibilityOverride(entry, id)
			if optimizeMultiAgentV2 {
				entry["multi_agent_version"] = "v2"
			}
			result = append(result, entry)
			continue
		}

		entry := cloneCodexClientModelMap(defaultTemplate)
		applyCodexClientModelMetadata(entry, id, model, optimizeMultiAgentV2, clientVersion)
		applyCodexClientMaxTokens(entry, model)
		applyCodexClientProviderCapabilities(entry, id, false, providersForModel)
		applyCPAWebSearchCapability(entry, id, webSearchCapabilityForModel, clientVersion)
		sanitizeCodexClientReasoningMetadata(entry, clientVersion)
		applyCodexClientVisibilityOverride(entry, id)
		result = append(result, entry)
	}

	applyCodexClientNonTemplatePriorities(result, templates)

	sort.SliceStable(
		result, func(i, j int) bool {
			return codexClientModelPriority(result[i]) < codexClientModelPriority(result[j])
		},
	)

	return result
}

func maxCodexClientTemplatePriority(templates map[string]map[string]any) int {
	maxPriority := 0
	for _, template := range templates {
		priority := codexClientModelPriority(template)
		if priority > maxPriority {
			maxPriority = priority
		}
	}
	return maxPriority
}

func applyCodexClientNonTemplatePriorities(result []map[string]any, templates map[string]map[string]any) {
	if len(result) == 0 {
		return
	}

	basePriority := maxCodexClientTemplatePriority(templates)
	type nonTemplateEntry struct {
		index       int
		displayName string
		slug        string
	}

	pending := make([]nonTemplateEntry, 0)
	for index, entry := range result {
		slug := stringModelValue(entry, "slug")
		if _, ok := templates[codexClientMetadataModelID(slug)]; ok {
			continue
		}
		displayName := stringModelValue(entry, "display_name")
		if displayName == "" {
			displayName = slug
		}
		pending = append(
			pending, nonTemplateEntry{
				index:       index,
				displayName: displayName,
				slug:        slug,
			},
		)
	}

	sort.SliceStable(
		pending, func(i, j int) bool {
			left := strings.ToLower(pending[i].displayName)
			right := strings.ToLower(pending[j].displayName)
			if left == right {
				return pending[i].slug < pending[j].slug
			}
			return left < right
		},
	)

	for rank, entry := range pending {
		result[entry.index]["priority"] = basePriority + 100*(rank+1)
	}
}

func loadCodexClientModelTemplates() (map[string]map[string]any, map[string]any, error) {
	raw, revision := registry.GetCodexClientModelsSnapshot()
	return loadCodexClientModelTemplatesSnapshot(raw, revision)
}

func loadCodexClientModelTemplatesSnapshot(
	raw []byte,
	revision uint64,
) (map[string]map[string]any, map[string]any, error) {
	codexClientModelTemplatesMu.Lock()
	defer codexClientModelTemplatesMu.Unlock()
	if codexClientModelTemplatesLoaded && codexClientModelTemplatesRevision == revision {
		return codexClientModelTemplates, codexClientDefaultTemplate, codexClientModelTemplatesErr
	}

	var payload codexClientModelsPayload
	err := json.Unmarshal(raw, &payload)
	var templates map[string]map[string]any
	var defaultTemplate map[string]any
	if err == nil {
		templates = make(map[string]map[string]any, len(payload.Models))
		for _, model := range payload.Models {
			slug := strings.TrimSpace(stringModelValue(model, "slug"))
			if slug == "" {
				continue
			}
			templates[slug] = cloneCodexClientModelMap(model)
			if slug == "gpt-5.5" {
				defaultTemplate = cloneCodexClientModelMap(model)
			}
		}
	}

	codexClientModelTemplatesLoaded = true
	codexClientModelTemplatesRevision = revision
	codexClientModelTemplates = templates
	codexClientDefaultTemplate = defaultTemplate
	codexClientModelTemplatesErr = err
	return codexClientModelTemplates, codexClientDefaultTemplate, codexClientModelTemplatesErr
}

func codexClientMetadataModelID(id string) string {
	id = strings.TrimSpace(id)
	if info := registry.LookupModelInfo(id); info != nil {
		if metadataID := strings.TrimSpace(info.CatalogName); metadataID != "" {
			return metadataID
		}
	}
	if index := strings.Index(id, "/"); index != -1 {
		base := strings.TrimSpace(id[index+1:])
		if info := registry.LookupModelInfo(base); info != nil {
			if metadataID := strings.TrimSpace(info.CatalogName); metadataID != "" {
				return metadataID
			}
		}
		return base
	}
	return id
}

func applyCodexClientDisplayName(entry map[string]any, model map[string]any) {
	if displayName := stringModelValue(model, "display_name"); displayName != "" {
		entry["display_name"] = displayName
	}
}

func applyCodexClientDescription(entry map[string]any, model map[string]any) {
	if description := stringModelValue(model, "description"); description != "" {
		entry["description"] = description
	}
}

func applyCodexClientBaseInstructions(entry map[string]any, model map[string]any) {
	if baseInstructions := stringModelValue(model, "base_instructions"); baseInstructions != "" {
		entry["base_instructions"] = baseInstructions
	}
}

func applyCodexClientModelCapabilities(
	entry map[string]any,
	id string,
	metadataID string,
	info *registry.ModelInfo,
	providersForModel ProvidersForModelFunc,
	clientVersion string,
) {
	if info != nil && info.Type == registry.OpenAIImageModelType {
		entry["visibility"] = "hide"
		delete(entry, "input_modalities")
		delete(entry, "supports_image_detail_original")
		return
	}

	providers := codexClientProviders(id, providersForModel)
	isAlias := metadataID != "" && !strings.EqualFold(id, metadataID)

	var constrainedModalities []string
	hasModalitiesConstraint := false
	for _, provider := range providers {
		providerInfo := codexClientProviderModelInfo(id, provider)
		if providerInfo == nil {
			continue
		}
		isCodex := strings.EqualFold(strings.TrimSpace(provider), "codex")
		if (!isCodex && isAlias) || providerInfo.ExplicitInputModalities {
			modalities := providerInfo.SupportedInputModalities
			if modalities == nil {
				modalities = []string{}
			}
			if !hasModalitiesConstraint {
				constrainedModalities = append([]string(nil), modalities...)
				hasModalitiesConstraint = true
			} else {
				constrainedModalities = intersectStringSlices(constrainedModalities, modalities)
			}
		}
	}
	if !hasModalitiesConstraint && info != nil && info.ExplicitInputModalities {
		constrainedModalities = append([]string(nil), info.SupportedInputModalities...)
		hasModalitiesConstraint = true
	}
	if hasModalitiesConstraint {
		applyCodexClientExplicitInputModalities(entry, constrainedModalities)
	}

	var constrainedThinking *registry.ThinkingSupport
	hasThinkingConstraint := false
	for _, provider := range providers {
		providerInfo := codexClientProviderModelInfo(id, provider)
		if providerInfo == nil {
			continue
		}
		isCodex := strings.EqualFold(strings.TrimSpace(provider), "codex")
		if (!isCodex && isAlias) || providerInfo.ExplicitThinking {
			hasThinkingConstraint = true
			providerThinking := providerInfo.Thinking
			if providerThinking == nil {
				providerThinking = &registry.ThinkingSupport{Levels: []string{}}
			}
			if constrainedThinking == nil {
				constrainedThinking = providerThinking
			} else {
				constrainedThinking = intersectThinkingSupport(constrainedThinking, providerThinking)
			}
		}
	}
	if !hasThinkingConstraint && info != nil && info.ExplicitThinking {
		constrainedThinking = info.Thinking
		if constrainedThinking == nil {
			constrainedThinking = &registry.ThinkingSupport{Levels: []string{}}
		}
		hasThinkingConstraint = true
	}
	if hasThinkingConstraint {
		applyCodexClientThinkingMetadata(entry, constrainedThinking, clientVersion)
	}
}

func codexClientProviders(id string, providersForModel ProvidersForModelFunc) []string {
	if providersForModel == nil {
		return nil
	}
	providers := providersForModel(id)
	if len(providers) == 0 {
		if index := strings.Index(id, "/"); index != -1 {
			providers = providersForModel(strings.TrimSpace(id[index+1:]))
		}
	}
	return providers
}

func codexClientProviderModelInfo(id, provider string) *registry.ModelInfo {
	if info := registry.LookupModelInfo(id, provider); info != nil {
		return info
	}
	if index := strings.Index(id, "/"); index != -1 {
		return registry.LookupModelInfo(strings.TrimSpace(id[index+1:]), provider)
	}
	return nil
}

func intersectThinkingSupport(left, right *registry.ThinkingSupport) *registry.ThinkingSupport {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	minimum := left.Min
	if right.Min > minimum {
		minimum = right.Min
	}
	maximum := left.Max
	if right.Max > 0 && (maximum == 0 || right.Max < maximum) {
		maximum = right.Max
	}
	return &registry.ThinkingSupport{
		Min:            minimum,
		Max:            maximum,
		ZeroAllowed:    left.ZeroAllowed && right.ZeroAllowed,
		DynamicAllowed: left.DynamicAllowed && right.DynamicAllowed,
		Levels:         intersectStringSlices(left.Levels, right.Levels),
	}
}

func intersectStringSlices(left, right []string) []string {
	if len(left) == 0 || len(right) == 0 {
		return []string{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, item := range right {
		rightSet[strings.ToLower(strings.TrimSpace(item))] = struct{}{}
	}
	result := make([]string, 0, len(left))
	seen := make(map[string]struct{}, len(left))
	for _, item := range left {
		key := strings.ToLower(strings.TrimSpace(item))
		if _, present := rightSet[key]; !present {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func applyCodexClientMaxContextLengthOverride(entry map[string]any, model map[string]any) {
	maxContextLength := intModelValue(model, "max_context_length")
	explicitMaxContextLength := maxContextLength > 0
	if maxContextLength <= 0 {
		maxContextLength = intModelValue(model, "context_length")
	}
	if maxContextLength <= 0 {
		return
	}
	entry["context_window"] = maxContextLength
	entry["max_context_window"] = maxContextLength
	if explicitMaxContextLength {
		entry["max_context_length"] = maxContextLength
	}
}

func applyCodexClientMaxTokens(entry map[string]any, model map[string]any) {
	if maxCompletionTokens := intModelValue(model, "max_completion_tokens"); maxCompletionTokens > 0 {
		entry["max_tokens"] = maxCompletionTokens
	}
}

func applyCPAWebSearchCapability(
	entry map[string]any,
	id string,
	capabilityForModel WebSearchCapabilityForModelFunc,
	clientVersion string,
) {
	delete(entry, "cpa_capabilities")
	if clientVersion != "cpa" || capabilityForModel == nil {
		return
	}
	if supported := capabilityForModel(strings.TrimSpace(id)); supported != nil {
		entry["cpa_capabilities"] = map[string]any{"web_search": *supported}
	}
}

func applyCodexClientProviderCapabilities(
	entry map[string]any,
	id string,
	templateModel bool,
	providersForModel ProvidersForModelFunc,
) {
	if !templateModel {
		applyCodexClientSearchToolSupport(entry, id, false, providersForModel)
		return
	}
	if providersForModel != nil && !isPureCodexProvider(id, providersForModel) {
		entry["supports_search_tool"] = false
		entry["prefer_websockets"] = false
		delete(entry, "apply_patch_tool_type")
		entry["service_tiers"] = []any{}
		delete(entry, "upgrade")
		delete(entry, "availability_nux")
		return
	}
	applyCodexClientSearchToolSupport(entry, id, true, providersForModel)
}

func isPureCodexProvider(id string, providersForModel ProvidersForModelFunc) bool {
	providers := codexClientProviders(id, providersForModel)
	if len(providers) == 0 {
		return false
	}
	for _, provider := range providers {
		if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return false
		}
	}
	return true
}

func applyCodexClientSearchToolSupport(
	entry map[string]any,
	id string,
	templateModel bool,
	providersForModel ProvidersForModelFunc,
) {
	supportsSearch, _ := entry["supports_search_tool"].(bool)
	if !supportsSearch {
		return
	}
	if !templateModel {
		entry["supports_search_tool"] = false
		return
	}
	if providersForModel == nil {
		return
	}

	providers := codexClientProviders(id, providersForModel)
	if len(providers) == 0 {
		entry["supports_search_tool"] = false
		return
	}
	for _, provider := range providers {
		if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
			entry["supports_search_tool"] = false
			return
		}
	}
}

func applyCodexClientModelMetadata(
	entry map[string]any,
	id string,
	model map[string]any,
	optimizeMultiAgentV2 bool,
	clientVersion string,
) {
	info := registry.LookupModelInfo(id)

	displayName := stringModelValue(model, "display_name")
	description := stringModelValue(model, "description")
	contextWindow := intModelValue(model, "context_length")
	thinkingSupport := codexClientThinkingSupport(model)
	inputModalities, explicitInputModalities := codexClientInputModalities(model)

	if info != nil {
		if displayName == "" && info.DisplayName != "" {
			displayName = info.DisplayName
		}
		if description == "" && info.Description != "" {
			description = info.Description
		}
		if contextWindow <= 0 && info.ContextLength > 0 {
			contextWindow = info.ContextLength
		}
		if info.Type == registry.OpenAIImageModelType {
			entry["visibility"] = "hide"
			delete(entry, "input_modalities")
			delete(entry, "supports_image_detail_original")
		} else if explicitInputModalities {
			applyCodexClientExplicitInputModalities(entry, inputModalities)
		} else {
			applyCodexClientExplicitInputModalities(entry, info.SupportedInputModalities)
		}
		if thinkingSupport == nil {
			thinkingSupport = info.Thinking
		}
	} else {
		applyCodexClientExplicitInputModalities(entry, inputModalities)
	}
	if thinkingSupport == nil {
		thinkingSupport = &registry.ThinkingSupport{Levels: []string{}}
	}
	applyCodexClientThinkingMetadata(entry, thinkingSupport, clientVersion)

	if maxContextLength := intModelValue(model, "max_context_length"); maxContextLength > 0 {
		contextWindow = maxContextLength
	}

	if displayName == "" {
		displayName = id
	}
	if description == "" {
		description = id
	}

	entry["slug"] = id
	entry["display_name"] = displayName
	entry["description"] = description
	entry["prefer_websockets"] = false
	if optimizeMultiAgentV2 {
		entry["multi_agent_version"] = "v2"
	}
	entry["service_tiers"] = []any{}
	delete(entry, "apply_patch_tool_type")
	delete(entry, "upgrade")
	delete(entry, "availability_nux")

	if contextWindow > 0 {
		entry["context_window"] = contextWindow
		entry["max_context_window"] = contextWindow
		if intModelValue(model, "max_context_length") > 0 {
			entry["max_context_length"] = contextWindow
		}
	}

	applyCodexClientBaseInstructions(entry, model)
	if plans, ok := model["available_in_plans"]; ok {
		entry["available_in_plans"] = cloneCodexClientModelValue(plans)
	}
}

func codexClientThinkingSupport(model map[string]any) *registry.ThinkingSupport {
	raw, ok := model["thinking"]
	if !ok || raw == nil {
		return nil
	}
	switch thinking := raw.(type) {
	case *registry.ThinkingSupport:
		return thinking
	case registry.ThinkingSupport:
		return &thinking
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var thinking registry.ThinkingSupport
	if err = json.Unmarshal(data, &thinking); err != nil {
		return nil
	}
	return &thinking
}

func codexClientInputModalities(model map[string]any) ([]string, bool) {
	for _, key := range []string{"supportedInputModalities", "input_modalities"} {
		raw, exists := model[key]
		if !exists {
			continue
		}
		switch modalities := raw.(type) {
		case []string:
			return append([]string(nil), modalities...), true
		case []any:
			result := make([]string, 0, len(modalities))
			for _, modality := range modalities {
				if value, ok := modality.(string); ok {
					result = append(result, value)
				}
			}
			return result, true
		default:
			return []string{}, true
		}
	}
	return nil, false
}

func applyCodexClientVisibilityOverride(entry map[string]any, id string) {
	target := strings.TrimSpace(id)
	if index := strings.Index(target, "/"); index != -1 {
		target = strings.TrimSpace(target[index+1:])
	}
	if registry.IsCodexImageModel(target) {
		entry["visibility"] = "hide"
		return
	}
	switch target {
	case "grok-imagine-image-quality",
		"grok-imagine-image",
		"grok-imagine-image-2.0",
		"grok-imagine-video",
		"grok-imagine-video-1.5",
		"grok-imagine-video-1.5-preview":
		entry["visibility"] = "hide"
	}
}

func applyCodexClientExplicitInputModalities(entry map[string]any, modalities []string) {
	applyCodexClientFilteredInputModalities(entry, filterCodexClientInputModalities(modalities))
}

func filterCodexClientInputModalities(modalities []string) []any {
	codexModalities := make([]any, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, raw := range modalities {
		switch modality := strings.ToLower(strings.TrimSpace(raw)); modality {
		case "text", "image":
			if _, duplicate := seen[modality]; duplicate {
				continue
			}
			seen[modality] = struct{}{}
			codexModalities = append(codexModalities, modality)
		}
	}
	return codexModalities
}

func applyCodexClientFilteredInputModalities(entry map[string]any, modalities []any) {
	entry["input_modalities"] = modalities
	for _, modality := range modalities {
		if modality == "image" {
			entry["supports_image_detail_original"] = true
			return
		}
	}
	delete(entry, "supports_image_detail_original")
}

func applyCodexClientThinkingMetadata(entry map[string]any, thinking *registry.ThinkingSupport, clientVersion string) {
	if thinking == nil {
		return
	}

	levels := make([]any, 0, len(thinking.Levels))
	defaultLevel := ""
	firstLevel := ""
	for _, rawLevel := range thinking.Levels {
		level := normalizeCodexClientReasoningLevel(rawLevel, clientVersion)
		if level == "" {
			continue
		}
		if firstLevel == "" {
			firstLevel = level
		}
		if (defaultLevel == "" && level != "none") || level == "medium" {
			defaultLevel = level
		}
		levels = append(
			levels, map[string]any{
				"effort":      level,
				"description": codexClientReasoningDescription(level),
			},
		)
	}
	if len(levels) == 0 {
		entry["supported_reasoning_levels"] = levels
		delete(entry, "default_reasoning_level")
		return
	}
	if defaultLevel == "" {
		defaultLevel = firstLevel
	}

	entry["supported_reasoning_levels"] = levels
	entry["default_reasoning_level"] = defaultLevel
}

func sanitizeCodexClientReasoningMetadata(entry map[string]any, clientVersion string) {
	rawLevels, ok := entry["supported_reasoning_levels"].([]any)
	if !ok {
		return
	}

	levels := make([]any, 0, len(rawLevels))
	allowedDefaults := make(map[string]struct{}, len(rawLevels))
	for _, rawLevelEntry := range rawLevels {
		levelEntry, ok := rawLevelEntry.(map[string]any)
		if !ok {
			continue
		}
		level := normalizeCodexClientReasoningLevel(stringModelValue(levelEntry, "effort"), clientVersion)
		if level == "" {
			continue
		}
		clonedEntry := cloneCodexClientModelMap(levelEntry)
		clonedEntry["effort"] = level
		levels = append(levels, clonedEntry)
		allowedDefaults[level] = struct{}{}
	}

	if len(levels) == 0 {
		entry["supported_reasoning_levels"] = levels
		delete(entry, "default_reasoning_level")
		return
	}

	defaultLevel := normalizeCodexClientReasoningLevel(stringModelValue(entry, "default_reasoning_level"), clientVersion)
	if _, ok := allowedDefaults[defaultLevel]; !ok {
		defaultLevel = stringModelValue(levels[0].(map[string]any), "effort")
	}

	entry["supported_reasoning_levels"] = levels
	entry["default_reasoning_level"] = defaultLevel
}

func normalizeCodexClientReasoningLevel(rawLevel string, clientVersion string) string {
	level := strings.ToLower(strings.TrimSpace(rawLevel))
	if !supportsExtendedReasoningLevels(clientVersion) {
		if _, ok := codexClientLegacyAllowedReasoningLevels[level]; !ok {
			return ""
		}
		return level
	}
	if _, ok := codexClientAllowedReasoningLevels[level]; !ok {
		return ""
	}
	return level
}

// supportsExtendedReasoningLevels reports whether the client supports the max and ultra levels.
// Empty or unparseable versions preserve modern capabilities.
func supportsExtendedReasoningLevels(clientVersion string) bool {
	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		return true
	}
	cmp, ok := compareDottedVersions(clientVersion, "0.144.0")
	if !ok {
		return true
	}
	return cmp >= 0
}

func parseDottedVersion(version string) []int64 {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(version, "v") || strings.HasPrefix(version, "V") {
		version = version[1:]
	}
	if index := strings.IndexAny(version, "-+"); index != -1 {
		version = version[:index]
	}
	parts := strings.Split(version, ".")
	numbers := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		number, errParse := strconv.ParseInt(part, 10, 64)
		if errParse != nil || number < 0 {
			return nil
		}
		numbers = append(numbers, number)
	}
	return numbers
}

func compareDottedVersions(left, right string) (int, bool) {
	leftNumbers := parseDottedVersion(left)
	rightNumbers := parseDottedVersion(right)
	if len(leftNumbers) == 0 || len(rightNumbers) == 0 {
		return 0, false
	}
	maxLength := len(leftNumbers)
	if len(rightNumbers) > maxLength {
		maxLength = len(rightNumbers)
	}
	for index := 0; index < maxLength; index++ {
		var leftValue, rightValue int64
		if index < len(leftNumbers) {
			leftValue = leftNumbers[index]
		}
		if index < len(rightNumbers) {
			rightValue = rightNumbers[index]
		}
		if leftValue < rightValue {
			return -1, true
		}
		if leftValue > rightValue {
			return 1, true
		}
	}
	return 0, true
}

func codexClientReasoningDescription(level string) string {
	switch level {
	case "none":
		return "No reasoning"
	case "minimal":
		return "Fastest responses with minimal reasoning"
	case "low":
		return "Fast responses with lighter reasoning"
	case "medium":
		return "Balances speed and reasoning depth for everyday tasks"
	case "high":
		return "Greater reasoning depth for complex problems"
	case "xhigh":
		return "Extra high reasoning depth for complex problems"
	case "max":
		return "Maximum available reasoning depth for complex problems"
	default:
		return level
	}
}

func codexClientModelPriority(model map[string]any) int {
	if priority, ok := model["priority"].(int); ok {
		return priority
	}
	if priority, ok := model["priority"].(float64); ok {
		return int(priority)
	}
	return 100
}

func stringModelValue(model map[string]any, key string) string {
	if model == nil {
		return ""
	}
	value, ok := model[key]
	if !ok {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func intModelValue(model map[string]any, key string) int {
	if model == nil {
		return 0
	}
	switch value := model[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func cloneCodexClientModelMap(model map[string]any) map[string]any {
	if model == nil {
		return nil
	}
	cloned := make(map[string]any, len(model))
	for key, value := range model {
		cloned[key] = cloneCodexClientModelValue(value)
	}
	return cloned
}

func cloneCodexClientModelValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneCodexClientModelMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i, entry := range typed {
			cloned[i] = cloneCodexClientModelValue(entry)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
