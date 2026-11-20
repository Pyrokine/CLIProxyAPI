package claude

import (
	"context"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func init() {
	translator.Register(
		constant.Claude,
		constant.Gemini,
		convertClaudeRequestToGeminiDefault,
		interfaces.TranslateResponse{
			Stream:     convertGeminiResponseToClaude,
			NonStream:  convertGeminiResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
	sdktranslator.RegisterRequestEnvelope(
		sdktranslator.FormatClaude, sdktranslator.FormatGemini,
		func(ctx context.Context, req sdktranslator.RequestEnvelope) sdktranslator.RequestEnvelope {
			return ConvertClaudeRequestToGeminiEnvelope(ctx, req, false)
		},
	)
}
