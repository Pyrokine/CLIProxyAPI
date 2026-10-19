package openai

import codexmodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/models"

type codexClientProvidersForModelFunc = codexmodels.ProvidersForModelFunc
type codexClientWebSearchCapabilityForModelFunc = codexmodels.WebSearchCapabilityForModelFunc

func CodexClientModelsResponse(models []map[string]any) map[string]any {
	return codexmodels.BuildResponse(models, nil, false)
}

func CodexClientModelsResponseWithMultiAgentV2(models []map[string]any, enabled bool) map[string]any {
	return codexmodels.BuildResponse(models, nil, enabled)
}

func CodexClientModelsResponseForClient(models []map[string]any, clientVersion string, enabled bool) map[string]any {
	return codexmodels.BuildResponseForClient(models, nil, enabled, clientVersion)
}

func codexClientModelsResponseWithCPACapabilities(
	models []map[string]any,
	providersForModel codexClientProvidersForModelFunc,
	webSearchCapabilityForModel codexClientWebSearchCapabilityForModelFunc,
	optimizeMultiAgentV2 bool,
	clientVersion string,
	applyPatchCapabilityForModel ...codexmodels.ApplyPatchCapabilityForModelFunc,
) map[string]any {
	return codexmodels.BuildResponseForClientWithCPACapabilities(
		models,
		providersForModel,
		webSearchCapabilityForModel,
		optimizeMultiAgentV2,
		clientVersion,
		applyPatchCapabilityForModel...,
	)
}
