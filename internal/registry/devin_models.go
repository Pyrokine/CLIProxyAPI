package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

//go:embed models/devin_models.json
var embeddedDevinModelsJSON []byte

type devinModelsFilePayload struct {
	Devin  []*ModelInfo `json:"devin,omitempty"`
	Models []*ModelInfo `json:"models,omitempty"`
}

type devinModelsStore struct {
	mu       sync.RWMutex
	models   []*ModelInfo
	rawJSON  []byte
	revision uint64
}

var devinCatalogStore = &devinModelsStore{}

var staticDevinModels = []*ModelInfo{
	{
		ID: "devin/swe-2", Object: "model", Type: "devin", OwnedBy: "cognition", DisplayName: "SWE-2",
		ContextLength: 262000, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"medium", "high", "max"}},
	},
	{
		ID: "devin/claude-fable-5-1", Object: "model", Type: "devin", OwnedBy: "anthropic", DisplayName: "Claude Fable 5.1",
		ContextLength: 1000000, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}},
	},
	{
		ID: "devin/gpt-6-astra", Object: "model", Type: "devin", OwnedBy: "openai", DisplayName: "GPT-6 Astra",
		ContextLength: 1000000, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}},
	},
	{
		ID: "devin/glm-5-2", Object: "model", Type: "devin", OwnedBy: "zhipu", DisplayName: "GLM-5.2",
		ContextLength: 200000, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"none", "high"}},
	},
	{
		ID: "devin/glm-5-3", Object: "model", Type: "devin", OwnedBy: "zhipu", DisplayName: "GLM-5.3",
		ContextLength: 1048576, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"low", "high", "max"}},
	},
	{
		ID: "devin/glm-5-3-flash", Object: "model", Type: "devin", OwnedBy: "zhipu", DisplayName: "GLM-5.3 Flash",
		ContextLength: 1000000, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"low", "high", "max"}},
	},
	{
		ID: "devin/gpt-5-6-sol", Object: "model", Type: "devin", OwnedBy: "openai", DisplayName: "GPT-5.6 Sol",
		ContextLength: 1000000, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh", "max"}},
	},
	{
		ID: "devin/gemini-3-8-flash", Object: "model", Type: "devin", OwnedBy: "google", DisplayName: "Gemini 3.8 Flash",
		ContextLength: 1048576, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
	},
	{
		ID: "devin/grok-4-6", Object: "model", Type: "devin", OwnedBy: "xai", DisplayName: "Grok 4.6",
		ContextLength: 500000, MaxCompletionTokens: 131072,
		Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
	},
	{
		ID: "devin/deepseek-v4-flash", Object: "model", Type: "devin", OwnedBy: "deepseek", DisplayName: "DeepSeek V4 Flash",
		ContextLength: 1048576, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"high", "max"}},
	},
	{
		ID: "devin/deepseek-v4-1-flash", Object: "model", Type: "devin", OwnedBy: "deepseek", DisplayName: "DeepSeek V4.1 Flash",
		ContextLength: 1048576, MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{Levels: []string{"high", "max"}},
	},
}

func init() {
	if _, err := loadDevinModelsFromBytes(embeddedDevinModelsJSON, "embedded catalog"); err != nil {
		log.Warnf("registry: failed to parse embedded Devin model catalog: %v", err)
	}
}

// GetDevinModels returns the active namespaced Devin model catalog.
func GetDevinModels() []*ModelInfo {
	devinCatalogStore.mu.RLock()
	models := cloneModelInfos(devinCatalogStore.models)
	devinCatalogStore.mu.RUnlock()
	if len(models) > 0 {
		return models
	}
	if catalog := getModels(); catalog != nil && len(catalog.Devin) > 0 {
		validated, err := sanitizeAndValidateDevinModels(catalog.Devin)
		if err == nil {
			return validated
		}
	}
	return cloneModelInfos(staticDevinModels)
}

// LookupDevinModel returns a model from the active Devin catalog.
func LookupDevinModel(modelID string) *ModelInfo {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	modelID = strings.TrimPrefix(modelID, "devin/")
	if modelID == "" {
		return nil
	}
	for _, model := range GetDevinModels() {
		if model == nil {
			continue
		}
		if strings.TrimPrefix(strings.ToLower(model.ID), "devin/") == modelID {
			return model
		}
	}
	return nil
}

// GetDevinModelsSnapshot returns the raw catalog and its in-process revision.
func GetDevinModelsSnapshot() ([]byte, uint64) {
	devinCatalogStore.mu.RLock()
	defer devinCatalogStore.mu.RUnlock()
	return append([]byte(nil), devinCatalogStore.rawJSON...), devinCatalogStore.revision
}

// GetDevinModelsJSON returns the current raw catalog JSON.
func GetDevinModelsJSON() []byte {
	data, _ := GetDevinModelsSnapshot()
	return data
}

// GetDevinModelsRevision returns the current catalog revision.
func GetDevinModelsRevision() uint64 {
	_, revision := GetDevinModelsSnapshot()
	return revision
}

func loadDevinModelsFromBytes(data []byte, source string) (bool, error) {
	models, err := ValidateDevinModelsJSON(data)
	if err != nil {
		return false, fmt.Errorf("%s: %w", source, err)
	}

	rawJSON := append([]byte(nil), data...)
	devinCatalogStore.mu.Lock()
	defer devinCatalogStore.mu.Unlock()
	if bytes.Equal(devinCatalogStore.rawJSON, rawJSON) {
		return false, nil
	}
	devinCatalogStore.models = models
	devinCatalogStore.rawJSON = rawJSON
	devinCatalogStore.revision++
	return true, nil
}

// ValidateDevinModelsJSON validates and normalizes a Devin model catalog.
func ValidateDevinModelsJSON(data []byte) ([]*ModelInfo, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("empty Devin models payload")
	}

	var payload devinModelsFilePayload
	if err := json.Unmarshal(data, &payload); err == nil {
		models := payload.Devin
		if len(models) == 0 {
			models = payload.Models
		}
		if len(models) > 0 {
			return sanitizeAndValidateDevinModels(models)
		}
	}

	var models []*ModelInfo
	if err := json.Unmarshal(data, &models); err == nil && len(models) > 0 {
		return sanitizeAndValidateDevinModels(models)
	}
	return nil, fmt.Errorf("expected a non-empty devin, models, or direct model array")
}

func sanitizeAndValidateDevinModels(candidates []*ModelInfo) ([]*ModelInfo, error) {
	seen := make(map[string]struct{}, len(candidates))
	models := make([]*ModelInfo, 0, len(candidates))
	for index, candidate := range candidates {
		if candidate == nil {
			return nil, fmt.Errorf("model at index %d is null", index)
		}
		model := cloneModelInfo(candidate)
		modelID := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model.ID)), "devin/")
		if modelID == "" || strings.Contains(modelID, "/") {
			return nil, fmt.Errorf("model at index %d has invalid id %q", index, model.ID)
		}
		model.ID = "devin/" + modelID
		if _, exists := seen[model.ID]; exists {
			return nil, fmt.Errorf("duplicate model id %q", model.ID)
		}
		seen[model.ID] = struct{}{}

		if model.ContextLength <= 0 {
			return nil, fmt.Errorf("model %q has invalid context length %d", model.ID, model.ContextLength)
		}
		if model.MaxCompletionTokens <= 0 {
			return nil, fmt.Errorf("model %q has invalid max completion tokens %d", model.ID, model.MaxCompletionTokens)
		}
		if err := normalizeDevinThinkingLevels(model); err != nil {
			return nil, err
		}

		model.Type = "devin"
		model.Object = "model"
		model.InputTokenLimit = model.ContextLength
		model.OutputTokenLimit = model.MaxCompletionTokens
		if len(model.SupportedInputModalities) == 0 {
			model.SupportedInputModalities = []string{"text"}
		}
		if len(model.SupportedOutputModalities) == 0 {
			model.SupportedOutputModalities = []string{"text"}
		}
		if len(model.SupportedGenerationMethods) == 0 {
			model.SupportedGenerationMethods = []string{"chat.completions"}
		}
		models = append(models, model)
	}
	return models, nil
}

func normalizeDevinThinkingLevels(model *ModelInfo) error {
	if model.Thinking == nil || len(model.Thinking.Levels) == 0 {
		return nil
	}
	allowed := map[string]struct{}{
		"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {}, "fast": {},
	}
	seen := make(map[string]struct{}, len(model.Thinking.Levels))
	levels := make([]string, 0, len(model.Thinking.Levels))
	for _, rawLevel := range model.Thinking.Levels {
		level := strings.ToLower(strings.TrimSpace(rawLevel))
		if _, valid := allowed[level]; !valid {
			return fmt.Errorf("model %q has invalid thinking level %q", model.ID, rawLevel)
		}
		if _, duplicate := seen[level]; duplicate {
			return fmt.Errorf("model %q has duplicate thinking level %q", model.ID, level)
		}
		seen[level] = struct{}{}
		levels = append(levels, level)
	}
	model.Thinking.Levels = levels
	return nil
}
