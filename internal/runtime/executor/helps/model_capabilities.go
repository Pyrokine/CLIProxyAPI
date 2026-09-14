package helps

import (
	"context"
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	chatcompletionsclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/chat-completions"
	responsesclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/responses"
	codexclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/claude"
	geminiclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/claude"
	interactionsclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/interactions/claude"
	openaiclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/claude"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

var apiKeyCompatRequestRegistry = func() *sdktranslator.Registry {
	registry := sdktranslator.NewRegistry()
	registry.Register(
		sdktranslator.FormatClaude,
		sdktranslator.FormatCodex,
		codexclaude.ConvertClaudeRequestToCodexWithCompat,
		sdktranslator.ResponseTransform{},
	)
	registry.Register(
		sdktranslator.FormatClaude,
		sdktranslator.FormatGemini,
		geminiclaude.ConvertClaudeRequestToGeminiWithCompat,
		sdktranslator.ResponseTransform{},
	)
	registry.Register(
		sdktranslator.FormatClaude,
		sdktranslator.FormatInteractions,
		interactionsclaude.ConvertClaudeRequestToInteractionsWithCompat,
		sdktranslator.ResponseTransform{},
	)
	registry.Register(
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAI,
		openaiclaude.ConvertClaudeRequestToOpenAIWithCompat,
		sdktranslator.ResponseTransform{},
	)
	registry.Register(
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatClaude,
		chatcompletionsclaude.ConvertOpenAIRequestToClaudeWithCompat,
		sdktranslator.ResponseTransform{},
	)
	registry.Register(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
		responsesclaude.ConvertOpenAIResponsesRequestToClaudeWithCompat,
		sdktranslator.ResponseTransform{},
	)
	return registry
}()

func APIKeyModelIsCompat(req cliproxyexecutor.Request) bool {
	modelInfo, ok := cliproxyauth.ResolvedAPIKeyModelInfo(req)
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
	if !isCompat {
		return translateRequestWithCodexMultiAgentV2(
			ctx,
			headers,
			cfg,
			from,
			to,
			model,
			payload,
			stream,
		)
	}

	if from == sdktranslator.FormatOpenAIResponse &&
		to != sdktranslator.FormatCodex &&
		to != sdktranslator.FormatOpenAIResponse {
		payload = rewriteCodexMultiAgentV2Input(ctx, headers, payload, cfg)
	}
	if apiKeyCompatRequestRegistry.HasRequestTransformer(from, to) {
		return apiKeyCompatRequestRegistry.TranslateRequestWithContext(
			ctx,
			from,
			to,
			model,
			payload,
			stream,
		)
	}
	return sdktranslator.TranslateRequestWithContext(
		ctx,
		from,
		to,
		model,
		payload,
		stream,
	)
}
