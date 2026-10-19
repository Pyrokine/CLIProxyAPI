package helps

import (
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

const configurationUpdatesChangedMetadataKey = "cliproxy.normalized_configuration_updates_changed"

// ApplyThinkingWithSourcePayload preserves summary visibility from the original
// client payload while applying thinking configuration to the translated target
func ApplyThinkingWithSourcePayload(
	body, currentSourcePayload, originalSourcePayload []byte,
	model, fromFormat, toFormat, providerKey string,
	requests ...cliproxyexecutor.Request,
) ([]byte, error) {
	summary := translatedRequestSummaryConfig(
		body,
		currentSourcePayload,
		originalSourcePayload,
		model,
		fromFormat,
		toFormat,
	)
	source := currentSourcePayload
	if len(source) == 0 {
		source = originalSourcePayload
	}
	var modelInfo *registry.ModelInfo
	var bound, updatesChanged bool
	if len(requests) > 0 {
		modelInfo, bound = cliproxyauth.ResolvedModelInfo(requests[0])
		updatesChanged, _ = requests[0].Metadata[configurationUpdatesChangedMetadataKey].(bool)
	}
	if !bound {
		lookupProvider := strings.ToLower(strings.TrimSpace(providerKey))
		if lookupProvider == "" {
			lookupProvider = strings.ToLower(strings.TrimSpace(toFormat))
			if lookupProvider == "openai-response" {
				lookupProvider = "codex"
			}
		}
		modelInfo = registry.LookupModelInfo(thinking.ParseSuffix(model).ModelName, lookupProvider)
	}
	return thinking.ApplyThinkingWithModelInfoAndSummary(
		body, source, model, fromFormat, toFormat, providerKey, modelInfo, summary, updatesChanged,
	)
}

func translatedRequestSummaryConfig(
	body, currentSourcePayload, originalSourcePayload []byte,
	model, fromFormat, toFormat string,
) thinking.SummaryConfig {
	fromFormat = strings.ToLower(strings.TrimSpace(fromFormat))
	toFormat = strings.ToLower(strings.TrimSpace(toFormat))

	var targetSummary thinking.SummaryConfig
	if fromFormat == toFormat {
		targetSummary = thinking.ExtractSummaryConfig(body, toFormat)
	} else {
		targetSummary = thinking.ExtractExplicitSummaryConfig(body, toFormat)
	}
	if targetSummary.Mode != thinking.SummaryUnspecified {
		return targetSummary
	}

	currentSummary := thinking.ExtractTranslatedSummaryConfig(currentSourcePayload, fromFormat, toFormat)
	originalSummary := thinking.ExtractTranslatedSummaryConfig(originalSourcePayload, fromFormat, toFormat)
	if currentSummary.Mode == thinking.SummaryUnspecified {
		return originalSummary
	}

	from := sdktranslator.FromString(fromFormat)
	to := sdktranslator.FromString(toFormat)
	if !sdktranslator.HasRequestTransformer(from, to) {
		return thinking.SummaryConfig{}
	}

	candidate := thinking.ApplySummaryConfigForModel(body, toFormat, model, currentSummary)
	if thinking.ExtractExplicitSummaryConfig(candidate, toFormat).Mode != thinking.SummaryUnspecified {
		return thinking.SummaryConfig{}
	}
	return currentSummary
}
