package helps

import (
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

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
	if len(requests) > 0 {
		if modelInfo, ok := cliproxyauth.ResolvedAPIKeyModelInfo(requests[0]); ok {
			return thinking.ApplyThinkingWithModelInfoAndSummary(
				body,
				originalSourcePayload,
				model,
				fromFormat,
				toFormat,
				providerKey,
				modelInfo,
				summary,
			)
		}
	}
	return thinking.ApplyThinkingWithSummary(
		body,
		model,
		fromFormat,
		toFormat,
		providerKey,
		summary,
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

	currentSummary := thinking.ExtractSummaryConfig(currentSourcePayload, fromFormat)
	originalSummary := thinking.ExtractSummaryConfig(originalSourcePayload, fromFormat)
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
