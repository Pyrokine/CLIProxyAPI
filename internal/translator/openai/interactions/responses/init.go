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
		constant.Interactions,
		convertOpenAIResponsesRequestToInteractions,
		interfaces.TranslateResponse{
			Stream:    convertInteractionsResponseToOpenAIResponses,
			NonStream: convertInteractionsResponseToOpenAIResponsesNonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatInteractions,
		convertOpenAIResponsesRequestToInteractionsChecked,
	)
	translator.Register(
		constant.Interactions,
		constant.OpenaiResponse,
		convertInteractionsRequestToOpenAIResponses,
		interfaces.TranslateResponse{
			Stream:    convertOpenAIResponsesResponseToInteractions,
			NonStream: convertOpenAIResponsesResponseToInteractionsNonStream,
		},
	)
}
