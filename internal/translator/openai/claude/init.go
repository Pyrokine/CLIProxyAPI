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
		constant.OpenAI,
		convertClaudeRequestToOpenAIDefault,
		interfaces.TranslateResponse{
			Stream:     convertOpenAIResponseToClaude,
			NonStream:  convertOpenAIResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, func(model string, body []byte, stream bool) ([]byte, error) {
		return convertClaudeRequestToOpenAIChecked(model, body, stream, false)
	})
}
