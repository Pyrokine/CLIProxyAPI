package cliproxy

import (
	"context"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

// NativeCapabilities contains tri-state native capability metadata for one model.
type NativeCapabilities struct {
	WebSearch *bool `json:"web_search,omitempty"`
}

// ModelInfo describes a model exposed through the embedded proxy registry.
type ModelInfo struct {
	ID                         string              `json:"id"`
	Object                     string              `json:"object"`
	Created                    int64               `json:"created"`
	OwnedBy                    string              `json:"owned_by"`
	Type                       string              `json:"type"`
	DisplayName                string              `json:"display_name,omitempty"`
	CatalogName                string              `json:"-"`
	ExplicitThinking           bool                `json:"-"`
	ExplicitInputModalities    bool                `json:"-"`
	Name                       string              `json:"name,omitempty"`
	Version                    string              `json:"version,omitempty"`
	Description                string              `json:"description,omitempty"`
	InputTokenLimit            int                 `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int                 `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods []string            `json:"supportedGenerationMethods,omitempty"`
	ContextLength              int                 `json:"context_length,omitempty"`
	MaxContextLength           int                 `json:"-"`
	MaxCompletionTokens        int                 `json:"max_completion_tokens,omitempty"`
	SupportedParameters        []string            `json:"supported_parameters,omitempty"`
	SupportedInputModalities   []string            `json:"supportedInputModalities,omitempty"`
	SupportedOutputModalities  []string            `json:"supportedOutputModalities,omitempty"`
	SupportsWebSearch          bool                `json:"supports_web_search,omitempty"`
	SupportConfigurationUpdate bool                `json:"-"`
	NativeCapabilities         *NativeCapabilities `json:"-"`
	Thinking                   *ThinkingSupport    `json:"thinking,omitempty"`
	IsCompat                   bool                `json:"-"`
	UserDefined                bool                `json:"-"`
}

// ThinkingSupport describes supported reasoning budget settings for a model.
type ThinkingSupport struct {
	Min            int      `json:"min,omitempty"`
	Max            int      `json:"max,omitempty"`
	ZeroAllowed    bool     `json:"zero_allowed,omitempty"`
	DynamicAllowed bool     `json:"dynamic_allowed,omitempty"`
	Levels         []string `json:"levels,omitempty"`
}

// ClientModelProjection describes the desired availability state for one client model.
type ClientModelProjection struct {
	ModelID       string
	Suspended     bool
	SuspendReason string
	QuotaExceeded bool
}

// ModelRegistryHook receives asynchronous model registry updates.
type ModelRegistryHook interface {
	OnModelsRegistered(ctx context.Context, provider, clientID string, models []*ModelInfo)
	OnModelsUnregistered(ctx context.Context, provider, clientID string)
}

// ModelRegistry describes registry operations consumed by external callers.
type ModelRegistry interface {
	RegisterClient(clientID, clientProvider string, models []*ModelInfo)
	UnregisterClient(clientID string)
	SetModelQuotaExceeded(clientID, modelID string)
	ClearModelQuotaExceeded(clientID, modelID string)
	ClientSupportsModel(clientID, modelID string) bool
	GetAvailableModels(handlerType string) []map[string]any
	GetAvailableModelsByProvider(provider string) []*ModelInfo
}

// VersionedModelRegistry adds ordered model-state publication without widening
// the interface implemented by existing registry consumers.
type VersionedModelRegistry interface {
	ModelRegistry
	ReplaceClientModels(clientID, clientProvider string, expectedEpoch uint64, models []*ModelInfo) (uint64, bool)
	ApplyClientModelProjections(
		clientID string,
		epoch uint64,
		generation uint64,
		projections []ClientModelProjection,
	) bool
	GetModelsForClient(clientID string) []*ModelInfo
	GetModelsAndEpochForClient(clientID string) ([]*ModelInfo, uint64)
	ClientRegistrationEpoch(clientID string) uint64
}

type modelRegistryAdapter struct {
	inner *registry.ModelRegistry
}

func (r *modelRegistryAdapter) RegisterClient(clientID, clientProvider string, models []*ModelInfo) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.RegisterClient(clientID, clientProvider, toRegistryModelInfos(models))
}

func (r *modelRegistryAdapter) ReplaceClientModels(clientID, clientProvider string, expectedEpoch uint64, models []*ModelInfo) (uint64, bool) {
	if r == nil || r.inner == nil {
		return 0, false
	}
	return r.inner.ReplaceClientModels(clientID, clientProvider, expectedEpoch, toRegistryModelInfos(models))
}

func (r *modelRegistryAdapter) UnregisterClient(clientID string) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.UnregisterClient(clientID)
}

func (r *modelRegistryAdapter) SetModelQuotaExceeded(clientID, modelID string) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.SetModelQuotaExceeded(clientID, modelID)
}

func (r *modelRegistryAdapter) ClearModelQuotaExceeded(clientID, modelID string) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.ClearModelQuotaExceeded(clientID, modelID)
}

func (r *modelRegistryAdapter) ApplyClientModelProjections(
	clientID string,
	epoch uint64,
	generation uint64,
	projections []ClientModelProjection,
) bool {
	if r == nil || r.inner == nil {
		return false
	}
	converted := make([]registry.ClientModelProjection, 0, len(projections))
	for _, projection := range projections {
		converted = append(converted, registry.ClientModelProjection{
			ModelID:       projection.ModelID,
			Suspended:     projection.Suspended,
			SuspendReason: projection.SuspendReason,
			QuotaExceeded: projection.QuotaExceeded,
		})
	}
	return r.inner.ApplyClientModelProjections(clientID, epoch, generation, converted)
}

func (r *modelRegistryAdapter) ClientSupportsModel(clientID, modelID string) bool {
	if r == nil || r.inner == nil {
		return false
	}
	return r.inner.ClientSupportsModel(clientID, modelID)
}

func (r *modelRegistryAdapter) GetAvailableModels(handlerType string) []map[string]any {
	if r == nil || r.inner == nil {
		return nil
	}
	return r.inner.GetAvailableModels(handlerType)
}

func (r *modelRegistryAdapter) GetAvailableModelsByProvider(provider string) []*ModelInfo {
	if r == nil || r.inner == nil {
		return nil
	}
	return fromRegistryModelInfos(r.inner.GetAvailableModelsByProvider(provider))
}

func (r *modelRegistryAdapter) GetModelsForClient(clientID string) []*ModelInfo {
	if r == nil || r.inner == nil {
		return nil
	}
	return fromRegistryModelInfos(r.inner.GetModelsForClient(clientID))
}

func (r *modelRegistryAdapter) GetModelsAndEpochForClient(clientID string) ([]*ModelInfo, uint64) {
	if r == nil || r.inner == nil {
		return nil, 0
	}
	models, epoch := r.inner.GetModelsAndEpochForClient(clientID)
	return fromRegistryModelInfos(models), epoch
}

func (r *modelRegistryAdapter) ClientRegistrationEpoch(clientID string) uint64 {
	if r == nil || r.inner == nil {
		return 0
	}
	return r.inner.ClientRegistrationEpoch(clientID)
}

// GlobalModelRegistry returns the shared registry instance.
func GlobalModelRegistry() VersionedModelRegistry {
	return &modelRegistryAdapter{inner: registry.GetGlobalRegistry()}
}

// SetGlobalModelRegistryHook registers an optional hook on the shared global registry instance.
// noinspection GoUnusedExportedFunction
func SetGlobalModelRegistryHook(hook ModelRegistryHook) {
	if hook == nil {
		registry.GetGlobalRegistry().SetHook(nil)
		return
	}
	registry.GetGlobalRegistry().SetHook(modelRegistryHookAdapter{hook: hook})
}

type modelRegistryHookAdapter struct {
	hook ModelRegistryHook
}

func (h modelRegistryHookAdapter) OnModelsRegistered(
	ctx context.Context,
	provider string,
	clientID string,
	models []*registry.ModelInfo,
) {
	if h.hook == nil {
		return
	}
	h.hook.OnModelsRegistered(ctx, provider, clientID, fromRegistryModelInfos(models))
}

func (h modelRegistryHookAdapter) OnModelsUnregistered(ctx context.Context, provider string, clientID string) {
	if h.hook == nil {
		return
	}
	h.hook.OnModelsUnregistered(ctx, provider, clientID)
}

func toRegistryModelInfos(models []*ModelInfo) []*registry.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if converted := toRegistryModelInfo(model); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

func toRegistryModelInfo(model *ModelInfo) *registry.ModelInfo {
	if model == nil {
		return nil
	}
	return &registry.ModelInfo{
		ID:                         model.ID,
		Object:                     model.Object,
		Created:                    model.Created,
		OwnedBy:                    model.OwnedBy,
		Type:                       model.Type,
		DisplayName:                model.DisplayName,
		CatalogName:                model.CatalogName,
		ExplicitThinking:           model.ExplicitThinking,
		ExplicitInputModalities:    model.ExplicitInputModalities,
		Name:                       model.Name,
		Version:                    model.Version,
		Description:                model.Description,
		InputTokenLimit:            model.InputTokenLimit,
		OutputTokenLimit:           model.OutputTokenLimit,
		SupportedGenerationMethods: append([]string(nil), model.SupportedGenerationMethods...),
		ContextLength:              model.ContextLength,
		MaxContextLength:           model.MaxContextLength,
		MaxCompletionTokens:        model.MaxCompletionTokens,
		SupportedParameters:        append([]string(nil), model.SupportedParameters...),
		SupportedInputModalities:   append([]string(nil), model.SupportedInputModalities...),
		SupportedOutputModalities:  append([]string(nil), model.SupportedOutputModalities...),
		SupportsWebSearch:          model.SupportsWebSearch,
		SupportConfigurationUpdate: model.SupportConfigurationUpdate,
		NativeCapabilities:         toRegistryNativeCapabilities(model.NativeCapabilities),
		Thinking:                   toRegistryThinkingSupport(model.Thinking),
		IsCompat:                   model.IsCompat,
		UserDefined:                model.UserDefined,
	}
}

func toRegistryNativeCapabilities(capabilities *NativeCapabilities) *registry.NativeCapabilities {
	if capabilities == nil {
		return nil
	}
	converted := &registry.NativeCapabilities{}
	if capabilities.WebSearch != nil {
		webSearch := *capabilities.WebSearch
		converted.WebSearch = &webSearch
	}
	return converted
}

func toRegistryThinkingSupport(thinking *ThinkingSupport) *registry.ThinkingSupport {
	if thinking == nil {
		return nil
	}
	return &registry.ThinkingSupport{
		Min:            thinking.Min,
		Max:            thinking.Max,
		ZeroAllowed:    thinking.ZeroAllowed,
		DynamicAllowed: thinking.DynamicAllowed,
		Levels:         append([]string(nil), thinking.Levels...),
	}
}

func fromRegistryModelInfos(models []*registry.ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if converted := fromRegistryModelInfo(model); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

func fromRegistryModelInfo(model *registry.ModelInfo) *ModelInfo {
	if model == nil {
		return nil
	}
	return &ModelInfo{
		ID:                         model.ID,
		Object:                     model.Object,
		Created:                    model.Created,
		OwnedBy:                    model.OwnedBy,
		Type:                       model.Type,
		DisplayName:                model.DisplayName,
		CatalogName:                model.CatalogName,
		ExplicitThinking:           model.ExplicitThinking,
		ExplicitInputModalities:    model.ExplicitInputModalities,
		Name:                       model.Name,
		Version:                    model.Version,
		Description:                model.Description,
		InputTokenLimit:            model.InputTokenLimit,
		OutputTokenLimit:           model.OutputTokenLimit,
		SupportedGenerationMethods: append([]string(nil), model.SupportedGenerationMethods...),
		ContextLength:              model.ContextLength,
		MaxContextLength:           model.MaxContextLength,
		MaxCompletionTokens:        model.MaxCompletionTokens,
		SupportedParameters:        append([]string(nil), model.SupportedParameters...),
		SupportedInputModalities:   append([]string(nil), model.SupportedInputModalities...),
		SupportedOutputModalities:  append([]string(nil), model.SupportedOutputModalities...),
		SupportsWebSearch:          model.SupportsWebSearch,
		SupportConfigurationUpdate: model.SupportConfigurationUpdate,
		NativeCapabilities:         fromRegistryNativeCapabilities(model.NativeCapabilities),
		Thinking:                   fromRegistryThinkingSupport(model.Thinking),
		IsCompat:                   model.IsCompat,
		UserDefined:                model.UserDefined,
	}
}

func fromRegistryNativeCapabilities(capabilities *registry.NativeCapabilities) *NativeCapabilities {
	if capabilities == nil {
		return nil
	}
	converted := &NativeCapabilities{}
	if capabilities.WebSearch != nil {
		webSearch := *capabilities.WebSearch
		converted.WebSearch = &webSearch
	}
	return converted
}

func fromRegistryThinkingSupport(thinking *registry.ThinkingSupport) *ThinkingSupport {
	if thinking == nil {
		return nil
	}
	return &ThinkingSupport{
		Min:            thinking.Min,
		Max:            thinking.Max,
		ZeroAllowed:    thinking.ZeroAllowed,
		DynamicAllowed: thinking.DynamicAllowed,
		Levels:         append([]string(nil), thinking.Levels...),
	}
}
