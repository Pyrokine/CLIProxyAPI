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
	if modelInfo, ok := req.Metadata[resolvedCodexOAuthModelInfoMetadataKey].(*registry.ModelInfo); ok && modelInfo != nil {
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
				if strings.EqualFold(name, candidate) && (strings.EqualFold(alias, route) || strings.EqualFold(name, route)) {
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
	sameModel := ok && local != nil && strings.EqualFold(
		strings.TrimSpace(thinking.ParseSuffix(local.ID).ModelName),
		strings.TrimSpace(thinking.ParseSuffix(selected.ID).ModelName),
	)
	if support != nil {
		selected.SupportConfigurationUpdate = *support
	} else {
		selected.SupportConfigurationUpdate = sameModel && local.SupportConfigurationUpdate
	}
	if configured {
		selected.IsCompat = options.IsCompat
	}
	metadata[resolvedHomeModelInfoMetadataKey] = &selected
	return req
}

func attachResolvedAPIKeyModelInfo(
	req cliproxyexecutor.Request,
	auth *Auth,
	routeModel string,
	upstreamModel string,
) (cliproxyexecutor.Request, error) {
	if thinking.HasContext1MTag(routeModel) {
		modelInfo := authModelInfo(auth, routeModel)
		if !modelInfoSupportsContext1M(modelInfo) {
			return req, context1MModelSupportError(routeModel)
		}
		if configuredAPIKeyModelAuth(auth) {
			resolved, ok := resolveConfiguredAPIKeyModelInfo(modelInfo, upstreamModel)
			if !ok {
				return req, context1MModelSupportError(routeModel)
			}
			return attachResolvedModelInfo(req, resolvedAPIKeyModelInfoMetadataKey, resolved), nil
		}
		if auth != nil && auth.AuthKind() == AuthKindOAuth && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			resolved, ok := lookupCodexOAuthModelInfo(auth, upstreamModel)
			if !ok || !modelInfoSupportsContext1M(resolved) {
				return req, context1MModelSupportError(routeModel)
			}
			return attachResolvedModelInfo(req, resolvedCodexOAuthModelInfoMetadataKey, resolved), nil
		}
	}
	if modelInfo, ok := lookupAPIKeyModelInfo(auth, routeModel, upstreamModel); ok {
		return attachResolvedModelInfo(req, resolvedAPIKeyModelInfoMetadataKey, modelInfo), nil
	}
	if modelInfo, ok := lookupCodexOAuthModelInfo(auth, upstreamModel); ok {
		return attachResolvedModelInfo(req, resolvedCodexOAuthModelInfoMetadataKey, modelInfo), nil
	}
	if auth != nil && auth.AuthKind() == AuthKindOAuth {
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

func lookupCodexOAuthModelInfo(auth *Auth, upstreamModel string) (*registry.ModelInfo, bool) {
	if auth == nil || auth.AuthKind() != AuthKindOAuth || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil, false
	}
	var models []*registry.ModelInfo
	switch strings.ToLower(strings.TrimSpace(auth.Attributes["plan_type"])) {
	case "plus":
		models = registry.GetCodexPlusModels()
	case "team", "business", "go":
		models = registry.GetCodexTeamModels()
	case "pro":
		models = registry.GetCodexProModels()
	default:
		models = registry.GetCodexFreeModels()
	}
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
	return modelInfoSupportsContext1M(authModelInfo(auth, model))
}

func authModelInfo(auth *Auth, model string) *registry.ModelInfo {
	if auth == nil {
		return nil
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		return nil
	}
	return registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, modelKey)
}

func modelInfoSupportsContext1M(modelInfo *registry.ModelInfo) bool {
	return modelInfo != nil &&
		max(modelInfo.ContextLength, modelInfo.MaxContextLength) >= context1MMinimumTokens
}

func context1MSelectionPredicate(model string) func(*Auth) bool {
	if !thinking.HasContext1MTag(model) {
		return nil
	}
	return func(auth *Auth) bool {
		return authSupportsContext1MModel(auth, model)
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
