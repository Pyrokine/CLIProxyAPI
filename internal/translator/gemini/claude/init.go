package claude

import (
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
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatClaude, sdktranslator.FormatGemini, func(model string, body []byte, stream bool) ([]byte, error) {
		return convertClaudeRequestToGeminiChecked(model, body, stream, false)
	})
}
