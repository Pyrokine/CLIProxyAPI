package chat_completions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		constant.OpenAI,
		constant.Interactions,
		convertOpenAIRequestToInteractions,
		interfaces.TranslateResponse{
			Stream:    convertInteractionsResponseToOpenAI,
			NonStream: convertInteractionsResponseToOpenAINonStream,
		},
	)
	translator.Register(
		constant.Interactions,
		constant.OpenAI,
		convertInteractionsRequestToOpenAI,
		interfaces.TranslateResponse{
			Stream:    convertOpenAIResponseToInteractions,
			NonStream: convertOpenAIResponseToInteractionsNonStream,
		},
	)
}
