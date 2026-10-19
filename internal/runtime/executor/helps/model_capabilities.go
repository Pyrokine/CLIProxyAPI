package helps

import (
	"bytes"
	"context"
	"maps"
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	chatcompletionsclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/chat-completions"
	responsesclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/responses"
	codexclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/claude"
	geminiclgemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini-cli/gemini"
	geminiclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/claude"
	geminiresponses "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
	interactionsclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/interactions/claude"
	openaiclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/claude"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

var apiKeyCompatRequestRegistry = func() *sdktranslator.Registry {
	registry := sdktranslator.NewRegistry()
	registry.RegisterCheckedRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatCodex,
		codexclaude.ConvertClaudeRequestToCodexWithCompatReturningError,
	)
	registry.RegisterCheckedRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatGemini,
		geminiclaude.ConvertClaudeRequestToGeminiWithCompatReturningError,
	)
	registry.RegisterCheckedRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatInteractions,
		interactionsclaude.ConvertClaudeRequestToInteractionsWithCompatReturningError,
	)
	registry.RegisterCheckedRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAI,
		openaiclaude.ConvertClaudeRequestToOpenAIWithCompatReturningError,
	)
	registry.RegisterCheckedRequest(
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatClaude,
		chatcompletionsclaude.ConvertOpenAIRequestToClaudeWithCompatReturningError,
	)
	registry.RegisterCheckedRequest(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
		responsesclaude.ConvertOpenAIResponsesRequestToClaudeWithCompatReturningError,
	)
	return registry
}()

func APIKeyModelIsCompat(req cliproxyexecutor.Request) bool {
	modelInfo, ok := cliproxyauth.ResolvedModelInfo(req)
	return ok && modelInfo.IsCompat
}

func TranslateRequestWithAPIKeyModelCompatibility(
	ctx context.Context,
	headers http.Header,
	cfg *config.Config,
	from,
	to sdktranslator.Format,
	model string,
	payload []byte,
	stream,
	isCompat bool,
) []byte {
	body, err := TranslateRequestWithAPIKeyModelCompatibilityChecked(ctx, headers, cfg, from, to, model, payload, stream, isCompat)
	if err != nil {
		log.WithError(err).Warnf("request translation rejected for %s -> %s", from, to)
	}
	return body
}

func TranslateRequestWithAPIKeyModelCompatibilityChecked(
	ctx context.Context,
	headers http.Header,
	cfg *config.Config,
	from,
	to sdktranslator.Format,
	model string,
	payload []byte,
	stream,
	isCompat bool,
	requests ...*cliproxyexecutor.Request,
) ([]byte, error) {
	var modelInfo *registry.ModelInfo
	for _, request := range requests {
		if request != nil {
			if info, ok := cliproxyauth.ResolvedModelInfo(*request); ok {
				modelInfo = info
				break
			}
		}
	}
	var requestRegistry *sdktranslator.Registry
	if from == sdktranslator.FormatOpenAIResponse && (to == sdktranslator.FormatGemini || to == sdktranslator.FormatGeminiCLI) {
		for _, request := range requests {
			if request == nil {
				continue
			}
			info, ok := cliproxyauth.ResolvedModelInfo(*request)
			if !ok || info.NativeCapabilities == nil || info.NativeCapabilities.WebSearch == nil {
				continue
			}
			nativeWebSearch := *info.NativeCapabilities.WebSearch
			requestRegistry = sdktranslator.NewRegistry()
			requestRegistry.RegisterCheckedRequest(from, to, func(model string, body []byte, stream bool) ([]byte, error) {
				converted, err := geminiresponses.ConvertOpenAIResponsesRequestToGeminiChecked(model, body, stream, &nativeWebSearch)
				if err != nil || to == sdktranslator.FormatGemini {
					return converted, err
				}
				return geminiclgemini.ConvertGeminiRequestToGeminiCLI(model, converted, stream), nil
			})
			break
		}
	}
	var translated sdktranslator.RequestEnvelope
	if !isCompat && requestRegistry == nil {
		translated = translateRequestWithCodexMultiAgentV2(ctx, headers, cfg, from, to, model, payload, stream, modelInfo)
	} else {
		if from == sdktranslator.FormatOpenAIResponse {
			payload = rewriteCodexOrphanDelegationInput(ctx, headers, payload, cfg)
			if to != sdktranslator.FormatCodex && to != sdktranslator.FormatOpenAIResponse {
				payload = rewriteCodexMultiAgentV2Input(ctx, headers, payload, cfg)
			}
		}
		if requestRegistry == nil {
			requestRegistry = sdktranslator.Default()
			if isCompat && apiKeyCompatRequestRegistry.HasRequestTransformer(from, to) {
				requestRegistry = apiKeyCompatRequestRegistry
			}
		}
		translated = requestRegistry.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{
			Format: from, Model: model, ModelInfo: modelInfo, Body: payload, Stream: stream,
		})
	}
	for _, request := range requests {
		if request == nil {
			continue
		}
		request.Metadata = maps.Clone(request.Metadata)
		if request.Metadata == nil {
			request.Metadata = make(map[string]any)
		}
		request.Metadata[configurationUpdatesChangedMetadataKey] = translated.ConfigurationUpdatesChanged
	}
	return translated.Body, translated.Err
}

// TranslateRequestPairWithAPIKeyModelCompatibilityChecked checks only the payload
// sent upstream; the original payload is used solely as a configuration baseline.
func TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
	ctx context.Context,
	headers http.Header,
	cfg *config.Config,
	from, to sdktranslator.Format,
	model string,
	originalPayload, requestPayload []byte,
	stream, isCompat bool,
	requests ...*cliproxyexecutor.Request,
) (original, working []byte, err error) {
	if bytes.Equal(originalPayload, requestPayload) {
		working, err = TranslateRequestWithAPIKeyModelCompatibilityChecked(
			ctx, headers, cfg, from, to, model, requestPayload, stream, isCompat, requests...,
		)
		return bytes.Clone(working), working, err
	}
	original, _ = TranslateRequestWithAPIKeyModelCompatibilityChecked(
		ctx, headers, cfg, from, to, model, originalPayload, stream, isCompat, requests...,
	)
	working, err = TranslateRequestWithAPIKeyModelCompatibilityChecked(
		ctx, headers, cfg, from, to, model, requestPayload, stream, isCompat, requests...,
	)
	return original, working, err
}
