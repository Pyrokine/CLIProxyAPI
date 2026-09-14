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

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - gemini-cli
//   - aistudio
//   - codex
//   - qwen
//   - iflow
//   - kimi
//   - antigravity (returns static overrides only)
//   - xai
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
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
	case "kimi":
		return GetKimiModels()
	case "antigravity":
		return getAntigravityModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	default:
		return nil
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
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
		GetDeepSeekModels(),
		GetXAIModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return m
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
