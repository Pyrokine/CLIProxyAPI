package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
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
	devinBuiltinSWE16SlowModelInfo(),
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

const devinBuiltinSWE16SlowID = "devin/swe-1-6-slow"

func devinBuiltinSWE16SlowModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:                         devinBuiltinSWE16SlowID,
		Object:                     "model",
		Type:                       "devin",
		OwnedBy:                    "cognition",
		DisplayName:                "SWE-1.6 Slow",
		ContextLength:              200000,
		MaxCompletionTokens:        64000,
		InputTokenLimit:            200000,
		OutputTokenLimit:           64000,
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		SupportedGenerationMethods: []string{"generateContent", "countTokens"},
	}
}

// WithDevinBuiltins preserves catalog entries while replacing built-in model definitions.
func WithDevinBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, devinBuiltinSWE16SlowModelInfo())
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
			return WithDevinBuiltins(validated)
		}
	}
	return WithDevinBuiltins(cloneModelInfos(staticDevinModels))
}

// LookupDevinModel returns a model from the active Devin catalog.
func LookupDevinModel(modelID string) *ModelInfo {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	modelID = strings.TrimPrefix(modelID, "devin/")
	if modelID == "" {
		return nil
	}
	models := GetDevinModels()
	for _, model := range models {
		if model != nil && strings.TrimPrefix(strings.ToLower(model.ID), "devin/") == modelID {
			return model
		}
	}
	baseID, _ := splitDevinModelID(modelID)
	if baseID != modelID {
		for _, model := range models {
			if model != nil && strings.TrimPrefix(strings.ToLower(model.ID), "devin/") == baseID {
				return model
			}
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

func retainDevinPublicAliases(models []*ModelInfo) ([]*ModelInfo, error) {
	embedded, err := ValidateDevinModelsJSON(embeddedDevinModelsJSON)
	if err != nil {
		return nil, fmt.Errorf("validate embedded Devin aliases: %w", err)
	}
	present := make(map[string]struct{}, len(models))
	for _, model := range models {
		present[model.ID] = struct{}{}
	}
	for _, model := range embedded {
		switch model.ID {
		case "devin/claude-haiku-4-5", "devin/claude-sonnet-4-5", "devin/gpt-4-1", "devin/gemini-3-flash":
			if _, exists := present[model.ID]; !exists {
				models = append(models, model)
			}
		}
	}
	return models, nil
}

func loadDevinModelsFromBytes(data []byte, source string) (bool, error) {
	models, err := ValidateDevinModelsJSON(data)
	if err != nil {
		return false, fmt.Errorf("%s: %w", source, err)
	}
	models, err = retainDevinPublicAliases(models)
	if err != nil {
		return false, fmt.Errorf("%s: %w", source, err)
	}
	models = WithDevinBuiltins(models)

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

var devinThinkingLevelOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "fast"}

var devinDisplayNameSuffixes = []string{
	" Low Thinking Fast", " Medium Thinking Fast", " High Thinking Fast", " XHigh Thinking Fast",
	" Max Thinking Fast", " No Thinking Fast", " Low Fast", " Medium Fast", " High Fast",
	" XHigh Fast", " Max Fast", " Low Thinking", " Medium Thinking", " High Thinking",
	" XHigh Thinking", " Max Thinking", " No Thinking", " Low", " Medium", " High",
	" XHigh", " Max", " None", " Minimal", " Thinking", " Fast",
}

func splitDevinModelID(id string) (string, string) {
	if id == "swe-1-6-slow" {
		return id, ""
	}
	if id == "swe-1-6-fast" {
		return "swe-1-6", ""
	}
	for _, level := range devinThinkingLevelOrder {
		if suffix := "_" + level; strings.HasSuffix(id, suffix) {
			return strings.TrimSuffix(id, suffix), level
		}
	}
	if strings.HasSuffix(id, "_thinking") {
		return strings.TrimSuffix(id, "_thinking"), "high"
	}
	for _, suffix := range []struct {
		tail   string
		readd  string
		effort string
	}{
		{tail: "-thinking-1m", readd: "-1m"},
		{tail: "-thinking"},
		{tail: "-max-1m", readd: "-1m", effort: "max"},
		{tail: "-none-1m", readd: "-1m", effort: "none"},
	} {
		if strings.HasSuffix(id, suffix.tail) {
			return strings.TrimSuffix(id, suffix.tail) + suffix.readd, suffix.effort
		}
	}
	for _, level := range devinThinkingLevelOrder {
		for _, suffix := range []string{"-fast", "-priority"} {
			if tail := "-" + level + suffix; strings.HasSuffix(id, tail) {
				return strings.TrimSuffix(id, tail), level
			}
		}
		if tail := "-" + level; strings.HasSuffix(id, tail) {
			return strings.TrimSuffix(id, tail), level
		}
	}
	return id, ""
}

func cleanDevinDisplayName(name string) string {
	name = strings.TrimSpace(name)
	for {
		cleaned := false
		for _, suffix := range devinDisplayNameSuffixes {
			if strings.HasSuffix(strings.ToLower(name), strings.ToLower(suffix)) {
				name = strings.TrimSpace(name[:len(name)-len(suffix)])
				cleaned = true
				break
			}
		}
		if !cleaned {
			return name
		}
	}
}

func aggregateDevinModels(models []*ModelInfo) []*ModelInfo {
	type modelGroup struct {
		model  *ModelInfo
		levels map[string]struct{}
	}
	groups := make(map[string]*modelGroup, len(models))
	order := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimPrefix(model.ID, "devin/")
		base, effort := splitDevinModelID(id)
		if base == "" {
			base = id
		}
		key := "devin/" + base
		group := groups[key]
		if group == nil {
			copyModel := cloneModelInfo(model)
			copyModel.ID = key
			if displayName := cleanDevinDisplayName(model.DisplayName); displayName != "" {
				copyModel.DisplayName = displayName
			}
			group = &modelGroup{model: copyModel, levels: make(map[string]struct{})}
			groups[key] = group
			order = append(order, key)
		}
		if base == id {
			if model.DisplayName != "" {
				group.model.DisplayName = cleanDevinDisplayName(model.DisplayName)
			}
			if model.OwnedBy != "" {
				group.model.OwnedBy = model.OwnedBy
			}
		}
		group.model.ContextLength = max(group.model.ContextLength, model.ContextLength)
		group.model.MaxCompletionTokens = max(group.model.MaxCompletionTokens, model.MaxCompletionTokens)
		group.model.InputTokenLimit = max(group.model.InputTokenLimit, model.InputTokenLimit)
		group.model.OutputTokenLimit = max(group.model.OutputTokenLimit, model.OutputTokenLimit)
		for _, modality := range model.SupportedInputModalities {
			if !slices.Contains(group.model.SupportedInputModalities, modality) {
				group.model.SupportedInputModalities = append(group.model.SupportedInputModalities, modality)
			}
		}
		for _, modality := range model.SupportedOutputModalities {
			if !slices.Contains(group.model.SupportedOutputModalities, modality) {
				group.model.SupportedOutputModalities = append(group.model.SupportedOutputModalities, modality)
			}
		}
		for _, method := range model.SupportedGenerationMethods {
			if !slices.Contains(group.model.SupportedGenerationMethods, method) {
				group.model.SupportedGenerationMethods = append(group.model.SupportedGenerationMethods, method)
			}
		}
		if model.Thinking != nil {
			for _, level := range model.Thinking.Levels {
				group.levels[level] = struct{}{}
			}
		}
		if effort != "" {
			group.levels[effort] = struct{}{}
		}
	}
	out := make([]*ModelInfo, 0, len(order))
	for _, key := range order {
		group := groups[key]
		if len(group.levels) > 0 {
			levels := make([]string, 0, len(group.levels))
			for level := range group.levels {
				levels = append(levels, level)
			}
			sort.Slice(levels, func(i, j int) bool {
				return slices.Index(devinThinkingLevelOrder, levels[i]) < slices.Index(devinThinkingLevelOrder, levels[j])
			})
			group.model.Thinking = &ThinkingSupport{Levels: levels}
		}
		out = append(out, group.model)
	}
	return out
}

func sanitizeAndValidateDevinModels(candidates []*ModelInfo) ([]*ModelInfo, error) {
	seen := make(map[string]struct{}, len(candidates))
	models := make([]*ModelInfo, 0, len(candidates))
	ignoredInternalUIDs := 0
	for index, candidate := range candidates {
		if candidate == nil {
			return nil, fmt.Errorf("model at index %d is null", index)
		}
		model := cloneModelInfo(candidate)
		modelID := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model.ID)), "devin/")
		if modelID == "" || strings.Contains(modelID, "/") {
			return nil, fmt.Errorf("model at index %d has invalid id %q", index, model.ID)
		}
		if strings.HasPrefix(modelID, "model_") {
			ignoredInternalUIDs++
			continue
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
	if ignoredInternalUIDs > 0 {
		log.Warnf("registry: skipped %d internal Devin UIDs from public model catalog", ignoredInternalUIDs)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("devin catalog contains no public models")
	}
	return aggregateDevinModels(models), nil
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
