package helps

import (
	"context"
	"net/http"

	multiagentv2 "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/optimize-multi-agent-v2"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func rewriteCodexMultiAgentV2Input(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	cfg *config.Config,
) []byte {
	return multiagentv2.RewriteCodexMultiAgentV2Input(ctx, headers, payload, cfg, true)
}

func rewriteCodexOrphanDelegationInput(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	cfg *config.Config,
) []byte {
	return multiagentv2.RewriteCodexOrphanDelegationInputForConfig(ctx, headers, payload, cfg)
}

func translateRequestWithCodexMultiAgentV2(
	ctx context.Context,
	headers http.Header,
	cfg *config.Config,
	from sdktranslator.Format,
	to sdktranslator.Format,
	model string,
	payload []byte,
	stream bool,
	modelInfo *registry.ModelInfo,
) sdktranslator.RequestEnvelope {
	if from == sdktranslator.FormatOpenAIResponse {
		payload = multiagentv2.RewriteCodexOrphanDelegationInputForConfig(ctx, headers, payload, cfg)
		if to != sdktranslator.FormatCodex && to != sdktranslator.FormatOpenAIResponse {
			payload = multiagentv2.RewriteCodexMultiAgentV2Input(ctx, headers, payload, cfg)
		}
	}
	return sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{
		Format: from, Model: model, ModelInfo: modelInfo, Body: payload, Stream: stream,
	})
}

// HasCodexMultiAgentV2NamespaceConflict reports whether the request defines
// the reserved optimized namespace, which must remain untouched.
func HasCodexMultiAgentV2NamespaceConflict(payload []byte) bool {
	return multiagentv2.HasCodexMultiAgentV2NamespaceConflict(payload)
}

func OptimizeCodexMultiAgentV2Request(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	cfg *config.Config,
	isCompat ...bool,
) ([]byte, bool) {
	updated, optimized := multiagentv2.OptimizeCodexMultiAgentV2Request(
		ctx,
		headers,
		payload,
		cfg,
	)
	if len(isCompat) > 0 && isCompat[0] {
		updated = multiagentv2.RewriteCodexMultiAgentV2Input(
			ctx,
			headers,
			updated,
			cfg,
			true,
		)
	}
	return updated, optimized
}

func RestoreCodexMultiAgentV2Response(payload []byte, optimized bool) []byte {
	return multiagentv2.RestoreCodexMultiAgentV2Response(payload, optimized)
}
