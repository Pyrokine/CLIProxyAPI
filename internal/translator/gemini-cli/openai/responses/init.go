package responses

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenaiResponse,
		GeminiCLI,
		convertOpenAIResponsesRequestToGeminiCLI,
		interfaces.TranslateResponse{
			Stream:    convertGeminiCLIResponseToOpenAIResponses,
			NonStream: convertGeminiCLIResponseToOpenAIResponsesNonStream,
		},
	)
}
