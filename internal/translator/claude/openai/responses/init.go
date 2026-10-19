package responses

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func init() {
	translator.Register(
		constant.OpenaiResponse,
		constant.Claude,
		convertOpenAIResponsesRequestToClaudeDefault,
		interfaces.TranslateResponse{
			Stream:    convertClaudeResponseToOpenAIResponses,
			NonStream: convertClaudeResponseToOpenAIResponsesNonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, func(model string, body []byte, stream bool) ([]byte, error) {
		return convertOpenAIResponsesRequestToClaudeChecked(model, body, stream, false)
	})
}
