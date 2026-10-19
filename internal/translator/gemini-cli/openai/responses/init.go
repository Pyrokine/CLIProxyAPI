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
		constant.GeminiCLI,
		convertOpenAIResponsesRequestToGeminiCLI,
		interfaces.TranslateResponse{
			Stream:    convertGeminiCLIResponseToOpenAIResponses,
			NonStream: convertGeminiCLIResponseToOpenAIResponsesNonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGeminiCLI, convertOpenAIResponsesRequestToGeminiCLIChecked)
}
