package auth

import (
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
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
		if !configuredAPIKeyModelAuth(auth) {
			return req, nil
		}
		resolved, ok := resolveConfiguredAPIKeyModelInfo(modelInfo, upstreamModel)
		if !ok {
			return req, context1MModelSupportError(routeModel)
		}
		return attachAPIKeyModelInfo(req, resolved), nil
	}
	modelInfo, ok := lookupAPIKeyModelInfo(auth, routeModel, upstreamModel)
	if !ok {
		return req, nil
	}
	return attachAPIKeyModelInfo(req, modelInfo), nil
}

func attachAPIKeyModelInfo(
	req cliproxyexecutor.Request,
	modelInfo *registry.ModelInfo,
) cliproxyexecutor.Request {
	metadata := make(map[string]any, len(req.Metadata)+1)
	maps.Copy(metadata, req.Metadata)
	metadata[resolvedAPIKeyModelInfoMetadataKey] = modelInfo
	req.Metadata = metadata
	return req
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
	if auth == nil {
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
	if modelInfo.IsCompat || modelInfo.Thinking != nil || modelInfo.NativeCapabilities != nil ||
		modelInfo.ContextLength > 0 || modelInfo.MaxContextLength > 0 {
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
