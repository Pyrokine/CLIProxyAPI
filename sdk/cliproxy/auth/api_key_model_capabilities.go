package auth

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const resolvedAPIKeyModelInfoMetadataKey = "cliproxy.resolved_api_key_model_info"

const context1MMinimumTokens = 1_000_000

// ResolvedAPIKeyModelInfo returns the exact configured model definition bound to
// this API-key execution attempt.
func ResolvedAPIKeyModelInfo(req cliproxyexecutor.Request) (*registry.ModelInfo, bool) {
	modelInfo, ok := req.Metadata[resolvedAPIKeyModelInfoMetadataKey].(*registry.ModelInfo)
	if !ok || modelInfo == nil {
		return nil, false
	}
	return modelInfo, true
}

const (
	resolvedCodexOAuthModelInfoMetadataKey = "cliproxy.resolved_codex_oauth_model_info"
	resolvedOAuthModelInfoMetadataKey      = "cliproxy.resolved_oauth_model_info"
	resolvedHomeModelInfoMetadataKey       = "cliproxy.resolved_home_model_info"
	resolvedHomeModelOptionsMetadataKey    = "cliproxy.resolved_home_model_options"
)

// ResolvedModelInfo returns the model capabilities selected for this execution attempt.
func ResolvedModelInfo(req cliproxyexecutor.Request) (*registry.ModelInfo, bool) {
	if modelInfo, ok := req.Metadata[resolvedHomeModelInfoMetadataKey].(*registry.ModelInfo); ok && modelInfo != nil {
		return modelInfo, true
	}
	if modelInfo, ok := req.Metadata[resolvedCodexOAuthModelInfoMetadataKey].(*registry.ModelInfo); ok &&
		modelInfo != nil {
		return modelInfo, true
	}
	if modelInfo, ok := req.Metadata[resolvedOAuthModelInfoMetadataKey].(*registry.ModelInfo); ok && modelInfo != nil {
		return modelInfo, true
	}
	return ResolvedAPIKeyModelInfo(req)
}

// ResolvedHomeModelOptions returns the selected credential's model options for this attempt.
func ResolvedHomeModelOptions(req cliproxyexecutor.Request) (internalconfig.OpenAICompatibilityModel, bool) {
	options, ok := req.Metadata[resolvedHomeModelOptionsMetadataKey].(internalconfig.OpenAICompatibilityModel)
	return options, ok
}

func homeAPIKeyModelOptions(auth *Auth, model, routeModel string) (internalconfig.OpenAICompatibilityModel, bool) {
	var empty internalconfig.OpenAICompatibilityModel
	if auth == nil {
		return empty, false
	}
	raw, exists := auth.Metadata["credential_options"]
	if !exists {
		return empty, false
	}
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		log.Warn("home credential options cannot be decoded")
		return empty, false
	}
	var options struct {
		Models json.RawMessage `json:"models"`
	}
	if errDecode := json.Unmarshal(data, &options); errDecode != nil {
		log.Warn("home credential options cannot be decoded")
		return empty, false
	}
	if len(options.Models) == 0 {
		return empty, false
	}
	var models []internalconfig.OpenAICompatibilityModel
	if errDecode := json.Unmarshal(options.Models, &models); errDecode != nil {
		log.Warn("home credential model options have invalid models field")
		return empty, true
	}
	requested := strings.TrimSpace(model)
	if requested == "" {
		return empty, true
	}
	baseModel := strings.TrimSpace(thinking.ParseSuffix(requested).ModelName)
	_, routeCandidates := modelAliasLookupCandidates(rewriteModelForAuth(strings.TrimSpace(routeModel), auth))
	for _, route := range routeCandidates {
		for _, candidate := range []string{requested, baseModel} {
			for _, configured := range models {
				name := strings.TrimSpace(configured.Name)
				alias := strings.TrimSpace(configured.Alias)
				if name == "" {
					name = alias
				}
				if strings.EqualFold(name, candidate) && (strings.EqualFold(alias, route) || strings.EqualFold(
					name, route,
				)) {
					return configured, true
				}
			}
		}
	}
	for _, useAlias := range []bool{false, true} {
		for _, candidate := range []string{requested, baseModel} {
			if candidate == "" {
				continue
			}
			for _, configured := range models {
				name := strings.TrimSpace(configured.Name)
				if useAlias || name == "" {
					name = strings.TrimSpace(configured.Alias)
				}
				if strings.EqualFold(name, candidate) {
					return configured, true
				}
			}
		}
	}
	return empty, true
}

func attachResolvedHomeModelInfo(
	req cliproxyexecutor.Request, auth *Auth, routeModel string,
	modelInfo *registry.ModelInfo, support *bool,
) cliproxyexecutor.Request {
	upstreamModel := req.Model
	if modelInfo != nil {
		upstreamModel = modelInfo.ID
	}
	if auth != nil {
		if dispatched := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); dispatched != "" {
			upstreamModel = dispatched
		}
	}
	options, configured := homeAPIKeyModelOptions(auth, upstreamModel, routeModel)
	if modelInfo == nil && !configured {
		return req
	}
	metadata := make(map[string]any, len(req.Metadata)+2)
	maps.Copy(metadata, req.Metadata)
	delete(metadata, resolvedHomeModelInfoMetadataKey)
	delete(metadata, resolvedHomeModelOptionsMetadataKey)
	if configured {
		metadata[resolvedHomeModelOptionsMetadataKey] = options
	}
	req.Metadata = metadata
	if modelInfo == nil {
		return req
	}
	selected := *modelInfo
	local, ok := ResolvedAPIKeyModelInfo(req)
	if !ok {
		local, ok = req.Metadata[resolvedCodexOAuthModelInfoMetadataKey].(*registry.ModelInfo)
	}
	if support != nil {
		selected.SupportConfigurationUpdate = *support
	} else {
		selected.SupportConfigurationUpdate = ok && local != nil && strings.EqualFold(
			strings.TrimSpace(thinking.ParseSuffix(local.ID).ModelName),
			strings.TrimSpace(thinking.ParseSuffix(selected.ID).ModelName),
		) && local.SupportConfigurationUpdate
	}
	if configured {
		selected.IsCompat = options.IsCompat
	}
	metadata[resolvedHomeModelInfoMetadataKey] = &selected
	return req
}

func (m *Manager) configuredPoolModelInfo(auth *Auth, routeModel, upstreamModel string) (*registry.ModelInfo, bool) {
	if auth == nil || m.HomeEnabled() {
		return nil, false
	}
	if kind, _ := auth.AccountInfo(); !strings.EqualFold(kind, "api_key") {
		return nil, false
	}
	requested := rewriteModelForAuth(routeModel, auth)
	models := m.apiKeyModelAliasEntries(auth)
	pool := resolveModelAliasPoolFromConfigModels(requested, models)
	if len(pool) < 2 {
		return nil, false
	}
	_, candidates := modelAliasLookupCandidates(requested)
	for _, candidate := range candidates {
		for _, model := range models {
			if !strings.EqualFold(strings.TrimSpace(model.GetAlias()), candidate) ||
				!strings.EqualFold(
					thinking.PreserveRequestModelDecorations(model.GetName(), requested), upstreamModel,
				) {
				continue
			}
			base := strings.TrimSpace(thinking.ParseSuffix(upstreamModel).ModelName)
			info := registry.LookupStaticModelInfo(base)
			if info == nil {
				info = &registry.ModelInfo{}
			}
			info.ID, info.CatalogName, info.UserDefined = base, strings.TrimSpace(model.GetName()), false
			if configured, ok := model.(interface {
				GetThinking() *registry.ThinkingSupport
				GetMaxContextLength() int
				GetIsCompat() bool
			}); ok {
				info.IsCompat = configured.GetIsCompat()
				if limit := configured.GetMaxContextLength(); limit > 0 {
					info.ContextLength, info.MaxContextLength = limit, limit
				}
				if support := configured.GetThinking(); support != nil {
					cloned := *support
					cloned.Levels = append([]string(nil), support.Levels...)
					info.Thinking = &cloned
				}
			}
			if configured, ok := model.(internalconfig.CodexModel); ok {
				info.SupportConfigurationUpdate = configured.SupportConfigurationUpdate
			}
			if configured, ok := model.(internalconfig.OpenAICompatibilityModel); ok {
				if configured.Image {
					info.Type = registry.OpenAIImageModelType
				}
				info.SupportedInputModalities = append([]string(nil), configured.InputModalities...)
				info.SupportedOutputModalities = append([]string(nil), configured.OutputModalities...)
			}
			return info, true
		}
	}
	return nil, false
}

func (m *Manager) executionModelSupportsContext1M(auth *Auth, routeModel, upstreamModel string) bool {
	if info, configured := m.configuredPoolModelInfo(auth, routeModel, upstreamModel); configured {
		return ModelSupportsContext1M(info)
	}
	return authSupportsContext1MModel(auth, upstreamModel)
}

func (m *Manager) attachExecutionModelInfo(
	req cliproxyexecutor.Request,
	auth *Auth,
	routeModel string,
) (cliproxyexecutor.Request, error) {
	if info, configured := m.configuredPoolModelInfo(auth, routeModel, req.Model); configured {
		if thinking.HasContext1MTag(routeModel) && !ModelSupportsContext1M(info) {
			return req, context1MModelSupportError(routeModel)
		}
		return attachResolvedModelInfo(req, resolvedAPIKeyModelInfoMetadataKey, info), nil
	}
	return attachResolvedAPIKeyModelInfo(req, auth, routeModel, req.Model)
}

func attachResolvedAPIKeyModelInfo(
	req cliproxyexecutor.Request,
	auth *Auth,
	routeModel string,
	upstreamModel string,
) (cliproxyexecutor.Request, error) {
	if thinking.HasContext1MTag(routeModel) {
		modelInfo := authModelInfo(auth, routeModel)
		if !ModelSupportsContext1M(modelInfo) {
			return req, context1MModelSupportError(routeModel)
		}
		if configuredAPIKeyModelAuth(auth) {
			resolved, ok := resolveConfiguredAPIKeyModelInfo(modelInfo, upstreamModel)
			if !ok {
				return req, context1MModelSupportError(routeModel)
			}
			return attachResolvedModelInfo(req, resolvedAPIKeyModelInfoMetadataKey, resolved), nil
		}
		if auth != nil && auth.AuthKind() == AuthKindOAuth && strings.EqualFold(
			strings.TrimSpace(auth.Provider), "codex",
		) {
			resolved, ok := lookupCodexOAuthModelInfo(auth, upstreamModel)
			if !ok || !ModelSupportsContext1M(resolved) {
				return req, context1MModelSupportError(routeModel)
			}
			return attachResolvedModelInfo(req, resolvedCodexOAuthModelInfoMetadataKey, resolved), nil
		}
	}
	if auth != nil {
		if modelInfo, ok := lookupAPIKeyModelInfo(auth, routeModel, upstreamModel); ok {
			return attachResolvedModelInfo(req, resolvedAPIKeyModelInfoMetadataKey, modelInfo), nil
		}
		if modelInfo, ok := lookupCodexOAuthModelInfo(auth, upstreamModel); ok {
			return attachResolvedModelInfo(req, resolvedCodexOAuthModelInfoMetadataKey, modelInfo), nil
		}
		if auth.AuthKind() == AuthKindOAuth {
			switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
			case "vertex", "gemini-cli", "aistudio":
				info := authModelInfo(auth, routeModel)
				if info == nil {
					info = authModelInfo(auth, rewriteModelForAuth(routeModel, auth))
				}
				if info != nil && info.NativeCapabilities != nil && info.NativeCapabilities.WebSearch != nil {
					if resolved, ok := resolveConfiguredAPIKeyModelInfo(info, upstreamModel); ok {
						return attachResolvedModelInfo(req, resolvedOAuthModelInfoMetadataKey, resolved), nil
					}
				}
			}
		}
	}
	if _, hadOAuth := req.Metadata[resolvedOAuthModelInfoMetadataKey]; hadOAuth {
		return attachResolvedModelInfo(req, "", nil), nil
	}
	if _, hadAPIKey := req.Metadata[resolvedAPIKeyModelInfoMetadataKey]; hadAPIKey {
		return attachResolvedModelInfo(req, "", nil), nil
	}
	if _, hadOAuth := req.Metadata[resolvedCodexOAuthModelInfoMetadataKey]; hadOAuth {
		return attachResolvedModelInfo(req, "", nil), nil
	}
	return req, nil
}

func attachResolvedModelInfo(
	req cliproxyexecutor.Request,
	key string,
	modelInfo *registry.ModelInfo,
) cliproxyexecutor.Request {
	metadata := make(map[string]any, len(req.Metadata)+1)
	maps.Copy(metadata, req.Metadata)
	delete(metadata, resolvedAPIKeyModelInfoMetadataKey)
	delete(metadata, resolvedCodexOAuthModelInfoMetadataKey)
	delete(metadata, resolvedOAuthModelInfoMetadataKey)
	if modelInfo != nil {
		metadata[key] = modelInfo
	}
	req.Metadata = metadata
	return req
}

// BindFinalModelContext binds and checks a changed target for requests that require a 1M context.
func BindFinalModelContext(
	req cliproxyexecutor.Request,
	auth *Auth,
	requestedModel, finalModel string,
) (cliproxyexecutor.Request, error) {
	if !thinking.HasContext1MTag(requestedModel) {
		return req, nil
	}
	finalModel = strings.TrimSpace(thinking.ParseSuffix(finalModel).ModelName)
	selected, bound := ResolvedModelInfo(req)
	if bound && strings.EqualFold(strings.TrimSpace(thinking.ParseSuffix(selected.ID).ModelName), finalModel) {
		if ModelSupportsContext1M(selected) {
			return req, nil
		}
		return req, context1MModelSupportError(requestedModel)
	}
	if !bound && finalModel != "" && strings.EqualFold(
		strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName), finalModel,
	) {
		return req, nil
	}
	if auth == nil || strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]) != "" {
		return req, context1MModelSupportError(requestedModel)
	}
	if auth.AuthKind() == AuthKindOAuth && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		if info, ok := lookupCodexOAuthModelInfo(auth, finalModel); ok && ModelSupportsContext1M(info) {
			return attachResolvedModelInfo(req, resolvedCodexOAuthModelInfoMetadataKey, info), nil
		}
		return req, context1MModelSupportError(requestedModel)
	}
	for _, info := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if !ModelSupportsContext1M(info) || !configuredModelMatchesUpstream(info, finalModel) {
			continue
		}
		info.ID = finalModel
		info.UserDefined = false
		key := resolvedAPIKeyModelInfoMetadataKey
		if auth.AuthKind() == AuthKindOAuth {
			key = resolvedOAuthModelInfoMetadataKey
		}
		return attachResolvedModelInfo(req, key, info), nil
	}
	return req, context1MModelSupportError(requestedModel)
}

func lookupCodexOAuthModelInfo(auth *Auth, upstreamModel string) (*registry.ModelInfo, bool) {
	if auth == nil || auth.AuthKind() != AuthKindOAuth || !strings.EqualFold(
		strings.TrimSpace(auth.Provider), "codex",
	) {
		return nil, false
	}
	models := registry.GetCodexModelsForPlan(auth.Attributes["plan_type"], false)
	selected := strings.TrimSpace(thinking.ParseSuffix(upstreamModel).ModelName)
	for _, model := range models {
		if model != nil && strings.EqualFold(model.ID, selected) {
			return model, true
		}
	}
	return nil, false
}

func context1MModelSupportError(model string) *Error {
	return &Error{
		Code: "model_not_supported",
		Message: fmt.Sprintf(
			"requested model %s requires a 1,000,000-token context window", strings.TrimSpace(model),
		),
		Retryable:  false,
		HTTPStatus: http.StatusBadRequest,
	}
}

func authSupportsContext1MModel(auth *Auth, model string) bool {
	if auth == nil || !thinking.HasContext1MTag(model) {
		return auth != nil
	}
	return ModelSupportsContext1M(authModelInfo(auth, model))
}

func authModelInfo(auth *Auth, model string) *registry.ModelInfo {
	if auth == nil {
		return nil
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		return nil
	}
	reg := registry.GetGlobalRegistry()
	if info := reg.GetModelInfoForClient(auth.ID, modelKey); info != nil {
		return info
	}
	for _, info := range reg.GetModelsForClient(auth.ID) {
		if configuredModelMatchesUpstream(info, modelKey) {
			return info
		}
	}
	return nil
}

// ModelSupportsContext1M reports whether either advertised context limit supports one million tokens.
func ModelSupportsContext1M(modelInfo *registry.ModelInfo) bool {
	return modelInfo != nil &&
		max(modelInfo.ContextLength, modelInfo.MaxContextLength) >= context1MMinimumTokens
}

func (m *Manager) context1MSelectionPredicate(model string) func(*Auth) bool {
	if !thinking.HasContext1MTag(model) {
		return nil
	}
	return func(auth *Auth) bool {
		return m.executionModelSupportsContext1M(auth, model, m.ResolveExecutionModel(auth, model))
	}
}

func combineSelectionPredicates(first func(*Auth) bool, second func(*Auth) bool) func(*Auth) bool {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(auth *Auth) bool {
		return first(auth) && second(auth)
	}
}

func lookupAPIKeyModelInfo(auth *Auth, routeModel, upstreamModel string) (*registry.ModelInfo, bool) {
	if !configuredAPIKeyModelAuth(auth) {
		return nil, false
	}

	requestedModel := rewriteModelForAuth(routeModel, auth)
	_, candidates := modelAliasLookupCandidates(requestedModel)
	if prefixedModel := strings.TrimSpace(routeModel); prefixedModel != requestedModel {
		_, prefixedCandidates := modelAliasLookupCandidates(prefixedModel)
		candidates = append(candidates, prefixedCandidates...)
	}
	for _, candidate := range candidates {
		modelInfo := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, candidate)
		if resolved, ok := resolveConfiguredAPIKeyModelInfo(modelInfo, upstreamModel); ok {
			return resolved, true
		}
	}
	return nil, false
}

func configuredAPIKeyModelAuth(auth *Auth) bool {
	if auth == nil || auth.AuthKind() == AuthKindOAuth {
		return false
	}
	kind, _ := auth.AccountInfo()
	configuredAPIKey := strings.EqualFold(strings.TrimSpace(kind), "api_key")
	configuredCompat := false
	if auth.Attributes != nil {
		authKind := strings.ToLower(strings.TrimSpace(auth.Attributes["auth_kind"]))
		configuredAPIKey = configuredAPIKey || authKind == "apikey" || authKind == "api_key"
		configuredCompat = strings.TrimSpace(auth.Attributes["compat_name"]) != ""
	}
	return configuredAPIKey || configuredCompat
}

func resolveConfiguredAPIKeyModelInfo(
	modelInfo *registry.ModelInfo,
	upstreamModel string,
) (*registry.ModelInfo, bool) {
	if !configuredModelMatchesUpstream(modelInfo, upstreamModel) || !configuredModelHasCapability(modelInfo) {
		return nil, false
	}
	modelInfo.ID = strings.TrimSpace(thinking.ParseSuffix(upstreamModel).ModelName)
	modelInfo.UserDefined = false
	return modelInfo, true
}

func configuredModelHasCapability(modelInfo *registry.ModelInfo) bool {
	if modelInfo == nil {
		return false
	}
	if modelInfo.IsCompat || modelInfo.SupportConfigurationUpdate || modelInfo.Thinking != nil ||
		modelInfo.NativeCapabilities != nil || modelInfo.ContextLength > 0 || modelInfo.MaxContextLength > 0 {
		return true
	}
	configured := strings.TrimSpace(modelInfo.CatalogName)
	if configured == "" {
		configured = strings.TrimSpace(modelInfo.ID)
	}
	baseModel := strings.TrimSpace(thinking.ParseSuffix(configured).ModelName)
	return registry.LookupStaticModelInfo(baseModel) != nil
}

func configuredModelMatchesUpstream(modelInfo *registry.ModelInfo, upstreamModel string) bool {
	if modelInfo == nil {
		return false
	}
	configured := strings.TrimSpace(modelInfo.CatalogName)
	if configured == "" {
		configured = strings.TrimSpace(modelInfo.ID)
	}
	configuredBase := strings.TrimSpace(thinking.ParseSuffix(configured).ModelName)
	upstreamBase := strings.TrimSpace(thinking.ParseSuffix(upstreamModel).ModelName)
	return configuredBase != "" && strings.EqualFold(configuredBase, upstreamBase)
}
