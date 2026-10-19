// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is stored in model_definitions_static_data.go.
package registry

import "strings"

const (
	codexBuiltinImage15ModelID                       = "gpt-image-1.5"
	codexBuiltinImageModelID                         = "gpt-image-2"
	codexBuiltinImage25FlareModelID                  = "gpt-image-2.5-flare"
	codexBuiltinImage25FlareSnapshotModelID          = "gpt-image-2.5-flare-2026-09-08"
	codexBuiltinImage25SunburstModelID               = "gpt-image-2.5-sunburst"
	codexBuiltinImage25SunburstSnapshotModelID       = "gpt-image-2.5-sunburst-2026-09-08"
	xaiBuiltinImageModelID                           = "grok-imagine-image"
	xaiBuiltinImage20ModelID                         = "grok-imagine-image-2.0"
	xaiBuiltinImageQualityModelID                    = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID                           = "grok-imagine-video"
	xaiBuiltinVideo15ModelID                         = "grok-imagine-video-1.5"
	codexBuiltinImage25ReleaseTimestamp        int64 = 1788825600 // 2026-09-08
)

var codexBuiltinImageModelIDs = []string{
	codexBuiltinImage15ModelID,
	codexBuiltinImageModelID,
	codexBuiltinImage25FlareModelID,
	codexBuiltinImage25FlareSnapshotModelID,
	codexBuiltinImage25SunburstModelID,
	codexBuiltinImage25SunburstSnapshotModelID,
}

// withCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates.
func withCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, codexBuiltinImageModelInfos()...)
}

// CanonicalCodexImageModel returns the canonical ID for a built-in Codex image model.
func CanonicalCodexImageModel(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	for _, candidate := range codexBuiltinImageModelIDs {
		if modelID == candidate {
			return candidate
		}
	}
	return ""
}

// IsCodexImageModel reports whether modelID is a built-in Codex image model.
func IsCodexImageModel(modelID string) bool {
	return CanonicalCodexImageModel(modelID) != ""
}

// CodexImageModelIDs returns all built-in Codex image model IDs.
func CodexImageModelIDs() []string {
	return append([]string(nil), codexBuiltinImageModelIDs...)
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// withXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func withXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(
		models,
		xaiBuiltinImageModelInfo(),
		xaiBuiltinImage20ModelInfo(),
		xaiBuiltinImageQualityModelInfo(),
		xaiBuiltinVideoModelInfo(),
		xaiBuiltinVideo15ModelInfo(),
	)
}

func AntigravityWebSearchModelFor(modelID string) string {
	modelID = normalizeAntigravityCapabilityModelID(modelID)
	if modelID == "" {
		return ""
	}
	for _, model := range GetGlobalRegistry().GetAvailableModelsByProvider("antigravity") {
		if model == nil {
			continue
		}
		currentModelID := normalizeAntigravityCapabilityModelID(model.ID)
		if currentModelID == "" {
			continue
		}
		if currentModelID == modelID {
			if model.SupportsWebSearch {
				return currentModelID
			}
			return ""
		}
	}
	return ""
}

func normalizeAntigravityCapabilityModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if open := strings.LastIndex(modelID, "("); open >= 0 && strings.HasSuffix(modelID, ")") {
		modelID = strings.TrimSpace(modelID[:open])
	}
	return modelID
}

func codexBuiltinImageModelInfos() []*ModelInfo {
	return []*ModelInfo{
		codexBuiltinImageModelInfo(codexBuiltinImage15ModelID, "GPT Image 1.5", 1704067200),
		codexBuiltinImageModelInfo(codexBuiltinImageModelID, "GPT Image 2", 1704067200),
		codexBuiltinImageModelInfo(
			codexBuiltinImage25FlareModelID,
			"GPT Image 2.5 Flare",
			codexBuiltinImage25ReleaseTimestamp,
		),
		codexBuiltinImageModelInfo(
			codexBuiltinImage25FlareSnapshotModelID,
			"GPT Image 2.5 Flare",
			codexBuiltinImage25ReleaseTimestamp,
		),
		codexBuiltinImageModelInfo(
			codexBuiltinImage25SunburstModelID,
			"GPT Image 2.5 Sunburst",
			codexBuiltinImage25ReleaseTimestamp,
		),
		codexBuiltinImageModelInfo(
			codexBuiltinImage25SunburstSnapshotModelID,
			"GPT Image 2.5 Sunburst",
			codexBuiltinImage25ReleaseTimestamp,
		),
	}
}

func codexBuiltinImageModelInfo(id, displayName string, created int64) *ModelInfo {
	return &ModelInfo{
		ID:          id,
		Object:      "model",
		Created:     created,
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: displayName,
		Version:     id,
	}
}

func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

func xaiBuiltinImage20ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:                        xaiBuiltinImage20ModelID,
		Object:                    "model",
		Created:                   1790035200, // 2026-09-22
		OwnedBy:                   "xai",
		Type:                      "xai",
		DisplayName:               "Grok Imagine Image 2.0",
		Name:                      xaiBuiltinImage20ModelID,
		Description:               "xAI image generation and editing model with 1K and 2K output.",
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"image"},
	}
}

func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

func xaiBuiltinVideo15ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15ModelID,
		Object:      "model",
		Created:     1735689600,
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5",
		Name:        xaiBuiltinVideo15ModelID,
		Description: "xAI Grok video generation model.",
	}
}

func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

func legacyStaticLookupModelsByChannel(channel string) []*ModelInfo {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "codex":
		return []*ModelInfo{
			{
				ID:                  "gpt-5.3-codex-spark",
				Object:              "model",
				Created:             1770912000,
				OwnedBy:             "openai",
				Type:                "openai",
				DisplayName:         "GPT 5.3 Codex Spark",
				Version:             "gpt-5.3",
				Description:         "Retired Codex model retained for historical request lookup.",
				ContextLength:       128000,
				MaxCompletionTokens: 128000,
				SupportedParameters: []string{"tools"},
				Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
			},
		}
	case "kimi":
		return []*ModelInfo{
			{
				ID:                        "kimi-k3-256k",
				Object:                    "model",
				Created:                   1785110400,
				OwnedBy:                   "moonshot",
				Type:                      "kimi",
				DisplayName:               "Kimi K3 256K",
				Description:               "Historical Kimi K3 256K route retained for exact-ID lookup.",
				ContextLength:             262144,
				MaxCompletionTokens:       65536,
				SupportedInputModalities:  []string{"text", "image"},
				SupportedOutputModalities: []string{"text"},
				Thinking:                  &ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "high", "max"}},
			},
		}
	default:
		return nil
	}
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - gemini-interactions
//   - vertex
//   - gemini-cli
//   - aistudio
//   - codex
//   - qwen
//   - iflow
//   - kimi
//   - antigravity (returns static overrides only)
//   - xai
//   - devin
//   - meta
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini", "gemini-interactions":
		return GetGeminiModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "gemini-cli":
		return GetGeminiCLIModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return getOpenAIModels()
	case "qwen":
		return GetQwenModels()
	case "iflow":
		return GetIFlowModels()
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		return GetKimiModels()
	case "antigravity":
		return getAntigravityModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	case "devin":
		return GetDevinModels()
	case "meta", "muse":
		return GetMetaModels()
	default:
		return nil
	}
}

// GetEmbeddedStaticModelDefinitionsByChannel returns the versioned embedded
// catalog without applying mutable remote refresh data.
func GetEmbeddedStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	catalog := embeddedModelsCatalog()
	if catalog == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return cloneModelInfos(catalog.Claude)
	case "gemini", "gemini-interactions":
		return cloneModelInfos(catalog.Gemini)
	case "vertex":
		return cloneModelInfos(catalog.Vertex)
	case "gemini-cli":
		return cloneModelInfos(catalog.GeminiCLI)
	case "aistudio":
		return cloneModelInfos(catalog.AIStudio)
	case "codex":
		return GetEmbeddedOpenAIModels()
	case "qwen":
		return cloneModelInfos(catalog.Qwen)
	case "iflow":
		return cloneModelInfos(catalog.IFlow)
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		return cloneModelInfos(catalog.Kimi)
	case "antigravity":
		return cloneModelInfos(catalog.Antigravity)
	case "xai", "x-ai", "grok":
		return GetEmbeddedXAIModels()
	case "devin":
		return cloneModelInfos(catalog.Devin)
	case "meta", "muse":
		return cloneModelInfos(catalog.Meta)
	default:
		return nil
	}
}

// LookupStaticModelInfoByChannel searches one provider-specific static section.
// It does not fall back across providers, so callers can preserve provenance.
func LookupStaticModelInfoByChannel(modelID, channel string) *ModelInfo {
	modelID = canonicalStaticModelID(modelID)
	if modelID == "" {
		return nil
	}
	for _, model := range GetStaticModelDefinitionsByChannel(channel) {
		if model != nil && model.ID == modelID {
			return cloneModelInfo(model)
		}
	}
	for _, model := range legacyStaticLookupModelsByChannel(channel) {
		if model != nil && model.ID == modelID {
			return cloneModelInfo(model)
		}
	}
	return nil
}

func canonicalStaticModelID(modelID string) string {
	return strings.TrimSpace(modelID)
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	modelID = canonicalStaticModelID(modelID)
	if modelID == "" {
		return nil
	}

	allModels := [][]*ModelInfo{
		GetClaudeModels(),
		GetGeminiModels(),
		GetGeminiVertexModels(),
		GetGeminiCLIModels(),
		GetAIStudioModels(),
		getOpenAIModels(),
		GetQwenModels(),
		GetIFlowModels(),
		GetKimiModels(),
		legacyStaticLookupModelsByChannel("kimi"),
		GetDeepSeekModels(),
		GetXAIModels(),
		legacyStaticLookupModelsByChannel("codex"),
		GetDevinModels(),
		GetMetaModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	// Check Antigravity static config
	if cfg := GetAntigravityModelConfig()[modelID]; cfg != nil {
		return &ModelInfo{
			ID:                  modelID,
			Thinking:            cfg.Thinking,
			MaxCompletionTokens: cfg.MaxCompletionTokens,
		}
	}

	return nil
}
