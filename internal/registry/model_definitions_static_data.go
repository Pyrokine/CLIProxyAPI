// Package registry provides model definitions for various AI service providers.
// This file stores the static model metadata catalog.
package registry

import (
	"sort"
	"strings"
)

func getCatalogModels(selectModels func(*staticModelsJSON) []*ModelInfo) []*ModelInfo {
	for _, data := range []*staticModelsJSON{getModels(), embeddedModelsCatalog()} {
		if data == nil {
			continue
		}
		if models := selectModels(data); len(models) > 0 {
			return cloneModelInfos(models)
		}
	}
	return nil
}

// getCodexModels returns models from the selected Codex catalog section, if available.
func getCodexModels(selectModels func(*staticModelsJSON) []*ModelInfo) []*ModelInfo {
	return getCatalogModels(selectModels)
}

func withoutModelIDs(models []*ModelInfo, ids ...string) []*ModelInfo {
	excluded := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		excluded[id] = struct{}{}
	}
	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		if _, remove := excluded[model.ID]; !remove {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// GetCodexFreeModels returns the Codex free-plan model definitions.
func GetCodexFreeModels() []*ModelInfo {
	if dynamic := getCodexModels(func(data *staticModelsJSON) []*ModelInfo { return data.CodexFree }); len(dynamic) > 0 {
		return withCodexBuiltins(dynamic)
	}
	return GetEmbeddedOpenAIModels()
}

// GetCodexTeamModels returns the Codex team-plan model definitions.
func GetCodexTeamModels() []*ModelInfo {
	if dynamic := getCodexModels(func(data *staticModelsJSON) []*ModelInfo { return data.CodexTeam }); len(dynamic) > 0 {
		return withCodexBuiltins(dynamic)
	}
	return GetEmbeddedOpenAIModels()
}

// GetCodexPlusModels returns the Codex plus-plan model definitions.
func GetCodexPlusModels() []*ModelInfo {
	if dynamic := getCodexModels(func(data *staticModelsJSON) []*ModelInfo { return data.CodexPlus }); len(dynamic) > 0 {
		return withCodexBuiltins(dynamic)
	}
	return GetEmbeddedOpenAIModels()
}

// GetCodexProModels returns the Codex pro-plan model definitions.
func GetCodexProModels() []*ModelInfo {
	if dynamic := getCodexModels(func(data *staticModelsJSON) []*ModelInfo { return data.CodexPro }); len(dynamic) > 0 {
		return withCodexBuiltins(dynamic)
	}
	return GetEmbeddedOpenAIModels()
}

// GetClaudeModels returns the standard Claude model definitions
func GetClaudeModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.Claude }); len(models) > 0 {
		return models
	}
	return []*ModelInfo{
		{
			ID:                  "claude-opus-5",
			Object:              "model",
			Created:             1784038800,
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Opus 5",
			Description:         "Latest premium model combining maximum intelligence with practical performance",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking: &ThinkingSupport{
				ZeroAllowed: true, DynamicAllowed: true,
				Levels: []string{"low", "medium", "high", "xhigh", "max"},
			},
		},
		{
			ID:                  "claude-haiku-4-5-20251001",
			Object:              "model",
			Created:             1759276800, // 2025-10-01
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.5 Haiku",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-sonnet-4-5-20250929",
			Object:              "model",
			Created:             1759104000, // 2025-09-29
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.5 Sonnet",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-sonnet-4-6",
			Object:              "model",
			Created:             1771372800, // 2026-02-17
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.6 Sonnet",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking: &ThinkingSupport{
				Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false,
				Levels: []string{"low", "medium", "high", "max"},
			},
		},
		{
			ID:                  "claude-opus-4-7",
			Object:              "model",
			Created:             1776297600, // 2026-04-16
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.7 Opus",
			Description:         "Premium model combining maximum intelligence with practical performance",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-opus-4-8",
			Object:              "model",
			Created:             1779984000, // 2026-05-29
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Opus 4.8",
			Description:         "Premium model combining maximum intelligence with practical performance",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-sonnet-5",
			Object:              "model",
			Created:             1782777600, // 2026-06-30
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Sonnet 5",
			Description:         "Anthropic's agentic Sonnet model for coding, tool use, and enterprise workflows",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking: &ThinkingSupport{
				ZeroAllowed: true, DynamicAllowed: true,
				Levels: []string{"low", "medium", "high", "xhigh", "max"},
			},
		},
		{
			ID:                        "claude-fable-5-1",
			Object:                    "model",
			Created:                   1788220800,
			OwnedBy:                   "anthropic",
			Type:                      "claude",
			DisplayName:               "Claude Fable 5.1",
			Description:               "Claude Fable 5.1 extends Claude Fable 5 at the same input and output prices, with cache reads at a quarter of the cost, and brings stronger long-running agentic coding, multistep research, and document, spreadsheet, and slide work.",
			ContextLength:             1000000,
			MaxCompletionTokens:       128000,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
			Thinking: &ThinkingSupport{
				ZeroAllowed: false, DynamicAllowed: true,
				Levels: []string{"low", "medium", "high", "xhigh", "max"},
			},
		},
		{
			ID:                        "claude-opus-5-5",
			Object:                    "model",
			Created:                   1790035200,
			OwnedBy:                   "anthropic",
			Type:                      "claude",
			DisplayName:               "Claude Opus 5.5",
			Description:               "Latest premium model combining maximum intelligence with practical performance",
			ContextLength:             1000000,
			MaxCompletionTokens:       128000,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
			Thinking: &ThinkingSupport{
				ZeroAllowed: false, DynamicAllowed: true,
				Levels: []string{"low", "medium", "high", "xhigh", "max"},
			},
		},
		{
			ID:                  "claude-fable-5",
			Object:              "model",
			Created:             1781049600, // 2026-06-10
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Fable 5",
			Description:         "Anthropic's most capable widely released model, for the most demanding reasoning and long-horizon agentic work",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-opus-4-6",
			Object:              "model",
			Created:             1770318000, // 2026-02-05
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.6 Opus",
			Description:         "Premium model combining maximum intelligence with practical performance",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-opus-4-5-20251101",
			Object:              "model",
			Created:             1761955200, // 2025-11-01
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.5 Opus",
			Description:         "Premium model combining maximum intelligence with practical performance",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false},
		},
		{
			ID:                  "claude-opus-4-1-20250805",
			Object:              "model",
			Created:             1722945600, // 2025-08-05
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.1 Opus",
			ContextLength:       200000,
			MaxCompletionTokens: 32000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: false, DynamicAllowed: false},
		},
		{
			ID:                  "claude-opus-4-20250514",
			Object:              "model",
			Created:             1715644800, // 2025-05-14
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4 Opus",
			ContextLength:       200000,
			MaxCompletionTokens: 32000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: false, DynamicAllowed: false},
		},
		{
			ID:                  "claude-sonnet-4-20250514",
			Object:              "model",
			Created:             1715644800, // 2025-05-14
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4 Sonnet",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: false, DynamicAllowed: false},
		},
		{
			ID:                  "claude-3-7-sonnet-20250219",
			Object:              "model",
			Created:             1708300800, // 2025-02-19
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 3.7 Sonnet",
			ContextLength:       128000,
			MaxCompletionTokens: 8192,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: false, DynamicAllowed: false},
		},
		{
			ID:                  "claude-3-5-haiku-20241022",
			Object:              "model",
			Created:             1729555200, // 2024-10-22
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 3.5 Haiku",
			ContextLength:       128000,
			MaxCompletionTokens: 8192,
			// Thinking: not supported for Haiku models
		},
	}
}

// geminiStableModels returns the Gemini 2.5 stable models shared across all Gemini providers.
// Returns a fresh slice each call to prevent cross-caller mutation.
func geminiStableModels() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:               "gemini-2.5-pro",
			Object:           "model",
			Created:          1750118400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-2.5-pro",
			Version:          "2.5",
			DisplayName:      "Gemini 2.5 Pro",
			Description:      "Stable release (June 17th, 2025) of Gemini 2.5 Pro",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true,
			},
		},
		{
			ID:               "gemini-2.5-flash",
			Object:           "model",
			Created:          1750118400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-2.5-flash",
			Version:          "001",
			DisplayName:      "Gemini 2.5 Flash",
			Description:      "Stable version of Gemini 2.5 Flash, our mid-size multimodal model that supports up to 1 million tokens, released in June of 2025.",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{Min: 0, Max: 24576, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:               "gemini-2.5-flash-lite",
			Object:           "model",
			Created:          1753142400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-2.5-flash-lite",
			Version:          "2.5",
			DisplayName:      "Gemini 2.5 Flash Lite",
			Description:      "Our smallest and most cost effective model, built for at scale usage.",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{Min: 0, Max: 24576, ZeroAllowed: true, DynamicAllowed: true},
		},
	}
}

// geminiPreviewParams holds per-provider differences for the Gemini 3.x preview models.
type geminiPreviewParams struct {
	proDescription   string
	proLevels        []string
	flashDescription string
	flashLevels      []string
}

// geminiPreviewModels returns the Gemini 3.x preview models shared across all Gemini providers.
// Each caller provides the per-model config via the params argument.
func geminiPreviewModels(params geminiPreviewParams) []*ModelInfo {
	return []*ModelInfo{
		{
			ID:               "gemini-3-pro-preview",
			Object:           "model",
			Created:          1737158400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-3-pro-preview",
			Version:          "3.0",
			DisplayName:      "Gemini 3 Pro Preview",
			Description:      params.proDescription,
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true, Levels: params.proLevels,
			},
		},
		{
			ID:               "gemini-3.1-pro-preview",
			Object:           "model",
			Created:          1771459200,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-3.1-pro-preview",
			Version:          "3.1",
			DisplayName:      "Gemini 3.1 Pro Preview",
			Description:      "Gemini 3.1 Pro Preview",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true, Levels: params.proLevels,
			},
		},
		{
			ID:               "gemini-3-flash-preview",
			Object:           "model",
			Created:          1765929600,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-3-flash-preview",
			Version:          "3.0",
			DisplayName:      "Gemini 3 Flash Preview",
			Description:      params.flashDescription,
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true, Levels: params.flashLevels,
			},
		},
	}
}

// geminiProImagePreview returns the Gemini 3 Pro Image Preview model definition.
func gemini35FlashLite() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3.5-flash-lite",
		Object:           "model",
		Created:          1782864000,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3.5-flash-lite",
		Version:          "3.5",
		DisplayName:      "Gemini 3.5 Flash Lite",
		Description:      "Gemini 3.5 Flash Lite",
		InputTokenLimit:  1048576,
		OutputTokenLimit: 65536,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 32768, DynamicAllowed: true,
			Levels: []string{"minimal", "low", "medium", "high"},
		},
	}
}

func gemini36Flash() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3.6-flash",
		Object:           "model",
		Created:          1782864000,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3.6-flash",
		Version:          "3.6",
		DisplayName:      "Gemini 3.6 Flash",
		Description:      "Gemini 3.6 Flash",
		InputTokenLimit:  1048576,
		OutputTokenLimit: 65536,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 32768, DynamicAllowed: true,
			Levels: []string{"minimal", "low", "medium", "high"},
		},
	}
}

func gemini37Flash() *ModelInfo {
	return &ModelInfo{
		ID:                        "gemini-3.7-flash",
		Object:                    "model",
		OwnedBy:                   "google",
		Type:                      "gemini",
		Name:                      "models/gemini-3.7-flash",
		Version:                   "3.7",
		DisplayName:               "Gemini 3.7 Flash",
		Description:               "Gemini 3.7 Flash",
		ContextLength:             1048576,
		MaxCompletionTokens:       65536,
		SupportedInputModalities:  []string{"text", "image", "audio", "video"},
		SupportedOutputModalities: []string{"text"},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 65535, DynamicAllowed: true,
			Levels: []string{"low", "medium", "high"},
		},
	}
}

func gemini38Flash(name string) *ModelInfo {
	return &ModelInfo{
		ID:                        "gemini-3.8-flash",
		Object:                    "model",
		OwnedBy:                   "google",
		Type:                      "gemini",
		Name:                      name,
		Version:                   "3.8",
		DisplayName:               "Gemini 3.8 Flash",
		Description:               "Gemini 3.8 Flash",
		ContextLength:             1048576,
		MaxCompletionTokens:       65536,
		SupportedInputModalities:  []string{"text", "image", "audio", "video"},
		SupportedOutputModalities: []string{"text"},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 65535, DynamicAllowed: true,
			Levels: []string{"low", "medium", "high"},
		},
	}
}

func gemini31FlashLite() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3.1-flash-lite",
		Object:           "model",
		Created:          1776288000,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3.1-flash-lite",
		Version:          "3.1",
		DisplayName:      "Gemini 3.1 Flash Lite",
		Description:      "Our smallest and most cost effective model, built for at scale usage.",
		InputTokenLimit:  1048576,
		OutputTokenLimit: 65536,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 32768, DynamicAllowed: true,
			Levels: []string{"minimal", "low", "medium", "high"},
		},
	}
}

func gemini31Pro() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3.1-pro",
		Object:           "model",
		Created:          1771459200,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3.1-pro",
		Version:          "3.1",
		DisplayName:      "Gemini 3.1 Pro",
		Description:      "Gemini 3.1 Pro",
		InputTokenLimit:  1048576,
		OutputTokenLimit: 65536,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 32768, DynamicAllowed: true,
			Levels: []string{"low", "medium", "high"},
		},
	}
}

func geminiProImagePreview() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3-pro-image-preview",
		Object:           "model",
		Created:          1737158400,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3-pro-image-preview",
		Version:          "3.0",
		DisplayName:      "Gemini 3 Pro Image Preview",
		Description:      "Gemini 3 Pro Image Preview",
		InputTokenLimit:  1048576,
		OutputTokenLimit: 65536,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking: &ThinkingSupport{
			Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true, Levels: []string{"low", "high"},
		},
	}
}

func geminiVertexProductionModels() []*ModelInfo {
	models := geminiPreviewModels(
		geminiPreviewParams{
			proDescription:   "Gemini 3 Pro",
			proLevels:        []string{"low", "high"},
			flashDescription: "Our most intelligent model built for speed, combining frontier intelligence with superior search and grounding.",
			flashLevels:      []string{"minimal", "low", "medium", "high"},
		},
	)
	for _, model := range models {
		switch model.ID {
		case "gemini-3-pro-preview":
			model.ID = "gemini-3-pro"
			model.Name = "models/gemini-3-pro"
			model.DisplayName = "Gemini 3 Pro"
		case "gemini-3-flash-preview":
			model.ID = "gemini-3-flash"
			model.Name = "models/gemini-3-flash"
			model.DisplayName = "Gemini 3 Flash"
		}
	}
	return models
}

func geminiProImage() *ModelInfo {
	model := geminiProImagePreview()
	model.ID = "gemini-3-pro-image"
	model.Name = "models/gemini-3-pro-image"
	model.DisplayName = "Gemini 3 Pro Image"
	model.Description = "Gemini 3 Pro Image"
	return model
}

func gemini31FlashImage() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3.1-flash-image",
		Object:           "model",
		Created:          1771459200,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3.1-flash-image",
		Version:          "3.1",
		DisplayName:      "Gemini 3.1 Flash Image",
		Description:      "Stable image generation and editing model with text and image output.",
		InputTokenLimit:  131072,
		OutputTokenLimit: 32768,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking:                  &ThinkingSupport{Levels: []string{"minimal", "high"}},
		SupportedInputModalities:  []string{"text", "image", "video", "pdf"},
		SupportedOutputModalities: []string{"text", "image"},
		NativeCapabilities:        &NativeCapabilities{WebSearch: boolPointer(true)},
	}
}

func gemini31FlashLiteImage() *ModelInfo {
	return &ModelInfo{
		ID:               "gemini-3.1-flash-lite-image",
		Object:           "model",
		Created:          1782172800,
		OwnedBy:          "google",
		Type:             "gemini",
		Name:             "models/gemini-3.1-flash-lite-image",
		Version:          "3.1",
		DisplayName:      "Gemini 3.1 Flash-Lite Image",
		Description:      "Low-latency stable image generation and editing model with 1K image output.",
		InputTokenLimit:  65536,
		OutputTokenLimit: 4096,
		SupportedGenerationMethods: []string{
			"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
		},
		Thinking:                  &ThinkingSupport{Levels: []string{"minimal", "high"}},
		SupportedInputModalities:  []string{"text", "image", "video", "pdf"},
		SupportedOutputModalities: []string{"text", "image"},
	}
}

// GetGeminiModels returns the standard Gemini model definitions
func GetGeminiModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.Gemini }); len(models) > 0 {
		return models
	}
	models := geminiStableModels()
	models = append(
		models, geminiPreviewModels(
			geminiPreviewParams{
				proDescription:   "Gemini 3 Pro Preview",
				proLevels:        []string{"low", "high"},
				flashDescription: "Gemini 3 Flash Preview",
				flashLevels:      []string{"minimal", "low", "medium", "high"},
			},
		)...,
	)
	models = append(
		models,
		gemini35FlashLite(),
		gemini36Flash(),
		gemini37Flash(),
		gemini38Flash("models/gemini-3.8-flash"),
		geminiProImagePreview(),
		gemini31FlashImage(),
		gemini31FlashLiteImage(),
	)
	return withoutModelIDs(models, "gemini-3-pro-preview")
}

func GetGeminiVertexModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.Vertex }); len(models) > 0 {
		return models
	}
	models := geminiStableModels()
	models = append(models, geminiVertexProductionModels()...)
	models = append(
		models,
		gemini31Pro(),
		gemini31FlashImage(),
		gemini31FlashLiteImage(),
		gemini31FlashLite(),
		gemini35FlashLite(),
		gemini36Flash(),
		gemini37Flash(),
		gemini38Flash("gemini-3.8-flash"),
		geminiProImage(),
		// Imagen image generation models - use :predict action
		&ModelInfo{
			ID:                         "imagen-4.0-generate-001",
			Object:                     "model",
			Created:                    1750000000,
			OwnedBy:                    "google",
			Type:                       "gemini",
			Name:                       "models/imagen-4.0-generate-001",
			Version:                    "4.0",
			DisplayName:                "Imagen 4.0 Generate",
			Description:                "Imagen 4.0 image generation model",
			SupportedGenerationMethods: []string{"predict"},
		},
		&ModelInfo{
			ID:                         "imagen-4.0-ultra-generate-001",
			Object:                     "model",
			Created:                    1750000000,
			OwnedBy:                    "google",
			Type:                       "gemini",
			Name:                       "models/imagen-4.0-ultra-generate-001",
			Version:                    "4.0",
			DisplayName:                "Imagen 4.0 Ultra Generate",
			Description:                "Imagen 4.0 ultra image generation model",
			SupportedGenerationMethods: []string{"predict"},
		},
		&ModelInfo{
			ID:                         "imagen-4.0-fast-generate-001",
			Object:                     "model",
			Created:                    1750000000,
			OwnedBy:                    "google",
			Type:                       "gemini",
			Name:                       "models/imagen-4.0-fast-generate-001",
			Version:                    "4.0",
			DisplayName:                "Imagen 4.0 Fast Generate",
			Description:                "Imagen 4.0 fast image generation model",
			SupportedGenerationMethods: []string{"predict"},
		},
	)
	return models
}

// GetGeminiCLIModels returns the standard Gemini model definitions
func GetGeminiCLIModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.GeminiCLI }); len(models) > 0 {
		return models
	}
	models := geminiStableModels()
	models = append(
		models, geminiPreviewModels(
			geminiPreviewParams{
				proDescription:   "Our most intelligent model with SOTA reasoning and multimodal understanding, and powerful agentic and vibe coding capabilities",
				proLevels:        []string{"low", "high"},
				flashDescription: "Our most intelligent model built for speed, combining frontier intelligence with superior search and grounding.",
				flashLevels:      []string{"minimal", "low", "medium", "high"},
			},
		)...,
	)
	return withoutModelIDs(models, "gemini-3-pro-preview")
}

// GetAIStudioModels returns the Gemini model definitions for AI Studio integrations
func GetAIStudioModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.AIStudio }); len(models) > 0 {
		return models
	}
	models := geminiStableModels()
	models = append(
		models, geminiPreviewModels(
			geminiPreviewParams{
				proDescription:   "Gemini 3 Pro Preview",
				flashDescription: "Our most intelligent model built for speed, combining frontier intelligence with superior search and grounding.",
			},
		)...,
	)
	models = append(
		models,
		gemini35FlashLite(),
		gemini36Flash(),
		gemini37Flash(),
		gemini38Flash("models/gemini-3.8-flash"),
		gemini31FlashImage(),
		gemini31FlashLiteImage(),
		&ModelInfo{
			ID:               "gemini-pro-latest",
			Object:           "model",
			Created:          1750118400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-pro-latest",
			Version:          "2.5",
			DisplayName:      "Gemini Pro Latest",
			Description:      "Latest release of Gemini Pro",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true,
			},
		},
		&ModelInfo{
			ID:               "gemini-flash-latest",
			Object:           "model",
			Created:          1750118400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-flash-latest",
			Version:          "2.5",
			DisplayName:      "Gemini Flash Latest",
			Description:      "Latest release of Gemini Flash",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{Min: 0, Max: 24576, ZeroAllowed: true, DynamicAllowed: true},
		},
		&ModelInfo{
			ID:               "gemini-flash-lite-latest",
			Object:           "model",
			Created:          1753142400,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-flash-lite-latest",
			Version:          "2.5",
			DisplayName:      "Gemini Flash-Lite Latest",
			Description:      "Latest release of Gemini Flash-Lite",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			Thinking: &ThinkingSupport{Min: 512, Max: 24576, ZeroAllowed: true, DynamicAllowed: true},
		},
		// {
		// 	ID:                         "gemini-2.5-flash-image-preview",
		// 	Object:                     "model",
		// 	Created:                    1756166400,
		// 	OwnedBy:                    "google",
		// 	Type:                       "gemini",
		// 	Name:                       "models/gemini-2.5-flash-image-preview",
		// 	Version:                    "2.5",
		// 	DisplayName:                "Gemini 2.5 Flash Image Preview",
		// 	Description:                "State-of-the-art image generation and editing model.",
		// 	InputTokenLimit:            1048576,
		// 	OutputTokenLimit:           8192,
		// 	SupportedGenerationMethods: []string{"generateContent", "countTokens", "createCachedContent", "batchGenerateContent"},
		// 	// image models don't support thinkingConfig; leave Thinking nil
		// },
		&ModelInfo{
			ID:               "gemini-2.5-flash-image",
			Object:           "model",
			Created:          1759363200,
			OwnedBy:          "google",
			Type:             "gemini",
			Name:             "models/gemini-2.5-flash-image",
			Version:          "2.5",
			DisplayName:      "Gemini 2.5 Flash Image",
			Description:      "State-of-the-art image generation and editing model.",
			InputTokenLimit:  1048576,
			OutputTokenLimit: 8192,
			SupportedGenerationMethods: []string{
				"generateContent", "countTokens", "createCachedContent", "batchGenerateContent",
			},
			// image models don't support thinkingConfig; leave Thinking nil
		},
	)
	return withoutModelIDs(models, "gemini-3-pro-preview")
}

// getOpenAIModels returns the default Codex/OpenAI model definitions.
func getOpenAIModels() []*ModelInfo {
	return GetCodexProModels()
}

func GetEmbeddedOpenAIModels() []*ModelInfo {
	if catalog := embeddedModelsCatalog(); catalog != nil && len(catalog.CodexPro) > 0 {
		return withCodexBuiltins(cloneModelInfos(catalog.CodexPro))
	}
	return []*ModelInfo{
		{
			ID:                  "gpt-5",
			Object:              "model",
			Created:             1754524800,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5-2025-08-07",
			DisplayName:         "GPT 5",
			Description:         "Stable version of GPT 5, The best model for coding and agentic tasks across domains.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"minimal", "low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5-codex",
			Object:              "model",
			Created:             1757894400,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5-2025-09-15",
			DisplayName:         "GPT 5 Codex",
			Description:         "Stable version of GPT 5 Codex, The best model for coding and agentic tasks across domains.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5-codex-mini",
			Object:              "model",
			Created:             1762473600,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5-2025-11-07",
			DisplayName:         "GPT 5 Codex Mini",
			Description:         "Stable version of GPT 5 Codex Mini: cheaper, faster, but less capable version of GPT 5 Codex.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1",
			Object:              "model",
			Created:             1762905600,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5.1-2025-11-12",
			DisplayName:         "GPT 5",
			Description:         "Stable version of GPT 5, The best model for coding and agentic tasks across domains.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1-codex",
			Object:              "model",
			Created:             1762905600,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5.1-2025-11-12",
			DisplayName:         "GPT 5.1 Codex",
			Description:         "Stable version of GPT 5.1 Codex, The best model for coding and agentic tasks across domains.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1-codex-mini",
			Object:              "model",
			Created:             1762905600,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5.1-2025-11-12",
			DisplayName:         "GPT 5.1 Codex Mini",
			Description:         "Stable version of GPT 5.1 Codex Mini: cheaper, faster, but less capable version of GPT 5.1 Codex.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1-codex-max",
			Object:              "model",
			Created:             1763424000,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5.1-max",
			DisplayName:         "GPT 5.1 Codex Max",
			Description:         "Stable version of GPT 5.1 Codex Max",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
		},
		{
			ID:                  "gpt-5.2-codex",
			Object:              "model",
			Created:             1765440000,
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-5.2",
			DisplayName:         "GPT 5.2 Codex",
			Description:         "Stable version of GPT 5.2 Codex, The best model for coding and agentic tasks across domains.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
		},
	}
}

// GetQwenModels returns the standard Qwen model definitions
func GetQwenModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.Qwen }); len(models) > 0 {
		return models
	}
	return []*ModelInfo{
		{
			ID:                  "qwen3-coder-plus",
			Object:              "model",
			Created:             1753228800,
			OwnedBy:             "qwen",
			Type:                "qwen",
			Version:             "3.0",
			DisplayName:         "Qwen3 Coder Plus",
			Description:         "Advanced code generation and understanding model",
			ContextLength:       32768,
			MaxCompletionTokens: 8192,
			SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
		},
		{
			ID:                  "qwen3-coder-flash",
			Object:              "model",
			Created:             1753228800,
			OwnedBy:             "qwen",
			Type:                "qwen",
			Version:             "3.0",
			DisplayName:         "Qwen3 Coder Flash",
			Description:         "Fast code generation model",
			ContextLength:       8192,
			MaxCompletionTokens: 2048,
			SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
		},
		{
			ID:                  "coder-model",
			Object:              "model",
			Created:             1771171200,
			OwnedBy:             "qwen",
			Type:                "qwen",
			Version:             "3.5",
			DisplayName:         "Qwen 3.5 Plus",
			Description:         "efficient hybrid model with leading coding performance",
			ContextLength:       1048576,
			MaxCompletionTokens: 65536,
			SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
		},
		{
			ID:                  "vision-model",
			Object:              "model",
			Created:             1758672000,
			OwnedBy:             "qwen",
			Type:                "qwen",
			Version:             "3.0",
			DisplayName:         "Qwen3 Vision Model",
			Description:         "Vision model model",
			ContextLength:       32768,
			MaxCompletionTokens: 2048,
			SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
		},
	}
}

// iFlowThinkingSupport is a shared ThinkingSupport configuration for iFlow models
// that support thinking mode via chat_template_kwargs.enable_thinking (boolean toggle).
// Uses level-based configuration so standard normalization flows apply before conversion.
var iFlowThinkingSupport = &ThinkingSupport{
	Levels: []string{"none", "auto", "minimal", "low", "medium", "high", "xhigh"},
}

// GetIFlowModels returns supported models for iFlow OAuth accounts.
func GetIFlowModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.IFlow }); len(models) > 0 {
		return models
	}
	entries := []struct {
		ID                  string
		DisplayName         string
		Description         string
		Created             int64
		ContextLength       int
		MaxCompletionTokens int
		Thinking            *ThinkingSupport
	}{
		{
			ID: "qwen3-coder-plus", DisplayName: "Qwen3-Coder-Plus", Description: "Qwen3 Coder Plus code generation",
			Created: 1753228800,
		},
		{ID: "qwen3-max", DisplayName: "Qwen3-Max", Description: "Qwen3 flagship model", Created: 1758672000},
		{
			ID: "qwen3-vl-plus", DisplayName: "Qwen3-VL-Plus", Description: "Qwen3 multimodal vision-language",
			Created: 1758672000,
		},
		{
			ID: "qwen3-max-preview", DisplayName: "Qwen3-Max-Preview", Description: "Qwen3 Max preview build",
			Created: 1757030400, Thinking: iFlowThinkingSupport,
		},
		{
			ID: "glm-4.6", DisplayName: "GLM-4.6", Description: "Zhipu GLM 4.6 general model", Created: 1759190400,
			Thinking: iFlowThinkingSupport,
		},
		{
			ID: "glm-5", DisplayName: "GLM-5", Description: "Zhipu GLM 5 general model", Created: 1770854400,
			ContextLength: 170000, MaxCompletionTokens: 128000, Thinking: iFlowThinkingSupport,
		},
		{ID: "kimi-k2", DisplayName: "Kimi-K2", Description: "Moonshot Kimi K2 general model", Created: 1752192000},
		{
			ID: "kimi-k2.5", DisplayName: "Kimi-K2.5", Description: "Moonshot Kimi K2.5 multimodal model",
			Created: 1769472000, ContextLength: 262144, MaxCompletionTokens: 32768, Thinking: iFlowThinkingSupport,
		},
		{
			ID: "minimax-m2.5", DisplayName: "MiniMax-M2.5", Description: "MiniMax M2.5 general model",
			Created: 1770854400, ContextLength: 204800, Thinking: iFlowThinkingSupport,
		},
		{
			ID: "deepseek-v3.2", DisplayName: "DeepSeek-V3.2-Exp", Description: "DeepSeek V3.2 experimental",
			Created: 1759104000, Thinking: iFlowThinkingSupport,
		},
		{
			ID: "deepseek-v3.1", DisplayName: "DeepSeek-V3.1-Terminus", Description: "DeepSeek V3.1 Terminus",
			Created: 1756339200, Thinking: iFlowThinkingSupport,
		},
		{
			ID: "deepseek-r1", DisplayName: "DeepSeek-R1", Description: "DeepSeek reasoning model R1",
			Created: 1737331200,
		},
		{ID: "deepseek-v3", DisplayName: "DeepSeek-V3-671B", Description: "DeepSeek V3 671B", Created: 1734307200},
		{ID: "qwen3-32b", DisplayName: "Qwen3-32B", Description: "Qwen3 32B", Created: 1747094400},
		{
			ID: "qwen3-235b-a22b-thinking-2507", DisplayName: "Qwen3-235B-A22B-Thinking",
			Description: "Qwen3 235B A22B Thinking (2507)", Created: 1753401600,
		},
		{
			ID: "qwen3-235b-a22b-instruct", DisplayName: "Qwen3-235B-A22B-Instruct",
			Description: "Qwen3 235B A22B Instruct", Created: 1753401600,
		},
		{ID: "qwen3-235b", DisplayName: "Qwen3-235B-A22B", Description: "Qwen3 235B A22B", Created: 1753401600},
		{
			ID: "iflow-rome-30ba3b", DisplayName: "iFlow-ROME", Description: "iFlow Rome 30BA3B model",
			Created: 1736899200,
		},
	}
	models := make([]*ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(
			models, &ModelInfo{
				ID:                  entry.ID,
				Object:              "model",
				Created:             entry.Created,
				OwnedBy:             "iflow",
				Type:                "iflow",
				DisplayName:         entry.DisplayName,
				Description:         entry.Description,
				ContextLength:       entry.ContextLength,
				MaxCompletionTokens: entry.MaxCompletionTokens,
				Thinking:            entry.Thinking,
			},
		)
	}
	return models
}

// antigravityModelConfig captures static antigravity model overrides, including
// Thinking budget limits and provider max completion tokens.
type antigravityModelConfig struct {
	Thinking            *ThinkingSupport
	MaxCompletionTokens int
}

func getAntigravityModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.Antigravity }); len(models) > 0 {
		return models
	}
	cfg := GetAntigravityModelConfig()
	models := make([]*ModelInfo, 0, len(cfg))
	for modelID, entry := range cfg {
		if modelID == "" || entry == nil {
			continue
		}
		models = append(
			models, &ModelInfo{
				ID:                  modelID,
				Object:              "model",
				OwnedBy:             "antigravity",
				Type:                "antigravity",
				Thinking:            entry.Thinking,
				MaxCompletionTokens: entry.MaxCompletionTokens,
			},
		)
	}
	sort.Slice(
		models, func(i, j int) bool {
			return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID)
		},
	)
	return models
}

// GetAntigravityModelConfig returns static configuration for antigravity models.
// Keys use upstream model names returned by the Antigravity models' endpoint.
func GetAntigravityModelConfig() map[string]*antigravityModelConfig {
	return map[string]*antigravityModelConfig{
		"gemini-2.5-flash": {
			Thinking: &ThinkingSupport{
				Min: 0, Max: 24576, ZeroAllowed: true, DynamicAllowed: true,
			},
		},
		"gemini-2.5-flash-lite": {
			Thinking: &ThinkingSupport{
				Min: 0, Max: 24576, ZeroAllowed: true, DynamicAllowed: true,
			},
		},
		"gemini-3.1-pro-high": {
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true,
				Levels: []string{"low", "high"},
			},
		},
		"gemini-3.1-pro-low": {
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true,
				Levels: []string{"low", "high"},
			},
		},
		"gemini-3.8-flash-high": {
			Thinking: &ThinkingSupport{
				Min: 1, Max: 65535, DynamicAllowed: true,
				Levels: []string{"low", "medium", "high"},
			},
			MaxCompletionTokens: 65536,
		},
		"gemini-3.7-flash-high": {
			Thinking: &ThinkingSupport{
				Min: 1, Max: 65535, DynamicAllowed: true,
				Levels: []string{"low", "medium", "high"},
			},
			MaxCompletionTokens: 65536,
		},
		"gemini-3.6-flash-high": {
			Thinking: &ThinkingSupport{
				Min: 1, Max: 65535, DynamicAllowed: true,
				Levels: []string{"minimal", "low", "medium", "high"},
			},
			MaxCompletionTokens: 65536,
		},
		"gemini-3.1-flash-image": {
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true,
				Levels: []string{"minimal", "high"},
			},
		},
		"gemini-3-flash": {
			Thinking: &ThinkingSupport{
				Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true,
				Levels: []string{"minimal", "low", "medium", "high"},
			},
		},
		"claude-opus-4-6-thinking": {
			Thinking: &ThinkingSupport{
				Min: 1024, Max: 64000, ZeroAllowed: true, DynamicAllowed: true,
			}, MaxCompletionTokens: 64000,
		},
		"claude-sonnet-4-6": {
			Thinking: &ThinkingSupport{
				Min: 1024, Max: 64000, ZeroAllowed: true, DynamicAllowed: true,
			}, MaxCompletionTokens: 64000,
		},
		"gpt-oss-120b-medium": {},
	}
}

// GetDeepSeekModels returns the DeepSeek models supported by its Anthropic-compatible API.
func GetDeepSeekModels() []*ModelInfo {
	models := []*ModelInfo{
		{
			ID:                  "deepseek-flash",
			Object:              "model",
			OwnedBy:             "deepseek",
			Type:                "claude",
			DisplayName:         "DeepSeek V4.1 Flash",
			ContextLength:       1048576,
			MaxCompletionTokens: 393216,
			IsCompat:            true,
		},
		{
			ID:                  "deepseek-v4-pro",
			Object:              "model",
			OwnedBy:             "deepseek",
			Type:                "claude",
			DisplayName:         "DeepSeek V4 Pro",
			ContextLength:       1048576,
			MaxCompletionTokens: 393216,
			IsCompat:            true,
		},
		{
			ID:                  "deepseek-v4-flash",
			Object:              "model",
			OwnedBy:             "deepseek",
			Type:                "claude",
			DisplayName:         "DeepSeek V4 Flash (legacy alias)",
			ContextLength:       1048576,
			MaxCompletionTokens: 393216,
			IsCompat:            true,
		},
		{
			ID:                  "deepseek-v4-flash-vision-exp",
			Object:              "model",
			OwnedBy:             "deepseek",
			Type:                "claude",
			DisplayName:         "DeepSeek V4 Flash Vision (legacy alias)",
			ContextLength:       1048576,
			MaxCompletionTokens: 393216,
			IsCompat:            true,
		},
	}
	for _, model := range models {
		model.SupportedParameters = []string{"tools"}
		model.SupportedInputModalities = []string{"text", "image"}
		model.SupportedOutputModalities = []string{"text"}
		model.Thinking = &ThinkingSupport{DynamicAllowed: true}
	}
	return models
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions
func GetKimiModels() []*ModelInfo {
	if models := getCatalogModels(func(data *staticModelsJSON) []*ModelInfo { return data.Kimi }); len(models) > 0 {
		return models
	}
	return []*ModelInfo{
		{
			ID:                        "kimi-k2",
			Object:                    "model",
			Created:                   1752192000, // 2025-07-11
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K2",
			Description:               "Kimi K2 - Moonshot AI's flagship coding model",
			ContextLength:             131072,
			MaxCompletionTokens:       32768,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		},
		{
			ID:                        "kimi-k2-thinking",
			Object:                    "model",
			Created:                   1762387200, // 2025-11-06
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K2 Thinking",
			Description:               "Kimi K2 Thinking - Extended reasoning model",
			ContextLength:             131072,
			MaxCompletionTokens:       32768,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                        "kimi-k2.5",
			Object:                    "model",
			Created:                   1769472000, // 2026-01-26
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K2.5",
			Description:               "Kimi K2.5 - Latest Moonshot AI coding model with improved capabilities",
			ContextLength:             262144,
			MaxCompletionTokens:       32768,
			SupportedInputModalities:  []string{"text", "image", "video"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "high"}},
		},
		{
			ID:                        "kimi-k2.6",
			Object:                    "model",
			Created:                   1776729600, // 2026-04-21
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K2.6",
			Description:               "Kimi K2.6 - Latest Moonshot AI coding model with improved capabilities",
			ContextLength:             262144,
			MaxCompletionTokens:       65536,
			SupportedInputModalities:  []string{"text", "image", "video"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "high"}},
		},
		{
			ID:                        "kimi-k2.7-code",
			Object:                    "model",
			Created:                   1780396800, // 2026-06-02
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K2.7 Code",
			Description:               "Kimi K2.7 Code - Moonshot AI's latest coding-focused model",
			ContextLength:             262144,
			MaxCompletionTokens:       65536,
			SupportedInputModalities:  []string{"text", "image", "video"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &ThinkingSupport{ZeroAllowed: false, Levels: []string{"low", "high"}},
		},
		{
			ID:                        "kimi-k2.7-code-highspeed",
			Object:                    "model",
			Created:                   1780396800, // 2026-06-02
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K2.7 Code HighSpeed",
			Description:               "Kimi K2.7 Code HighSpeed - Same capabilities as Kimi K2.7 Code with higher output speed (~180 tokens/s)",
			ContextLength:             262144,
			MaxCompletionTokens:       65536,
			SupportedInputModalities:  []string{"text", "image", "video"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &ThinkingSupport{ZeroAllowed: false, Levels: []string{"low", "high"}},
		},
		{
			ID:                        "kimi-k3",
			Object:                    "model",
			Created:                   1784073600, // 2026-07-15
			OwnedBy:                   "moonshot",
			Type:                      "kimi",
			DisplayName:               "Kimi K3",
			Description:               "Kimi K3 - Moonshot AI's next-generation flagship model (~2.8T MoE) with multimodal input",
			ContextLength:             1048576,
			MaxCompletionTokens:       65536,
			SupportedInputModalities:  []string{"text", "image", "video"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "high", "max"}},
		},
	}
}

func xaiModelsFromCatalog(data *staticModelsJSON) []*ModelInfo {
	if data != nil && len(data.XAI) > 0 {
		return cloneModelInfos(data.XAI)
	}
	fallback := embeddedModelsCatalog()
	if fallback == nil {
		return nil
	}
	return cloneModelInfos(fallback.XAI)
}

// GetXAIModels returns xAI Grok model definitions including builtin image/video models.
func GetXAIModels() []*ModelInfo {
	return withXAIBuiltins(xaiModelsFromCatalog(getModels()))
}

func GetEmbeddedXAIModels() []*ModelInfo {
	return withXAIBuiltins(xaiModelsFromCatalog(nil))
}
