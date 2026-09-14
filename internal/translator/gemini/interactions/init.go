package interactions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		constant.Interactions,
		constant.Interactions,
		convertInteractionsRequestToInteractions,
		interfaces.TranslateResponse{
			Stream:    convertInteractionsResponsePassthrough,
			NonStream: convertInteractionsResponsePassthroughNonStream,
		},
	)
	translator.Register(
		constant.Interactions,
		constant.Gemini,
		convertInteractionsRequestToGemini,
		interfaces.TranslateResponse{
			Stream:    convertGeminiResponseToInteractions,
			NonStream: convertGeminiResponseToInteractionsNonStream,
		},
	)
	translator.Register(
		constant.Gemini,
		constant.Interactions,
		convertGeminiRequestToInteractions,
		interfaces.TranslateResponse{
			Stream:    convertInteractionsResponseToGemini,
			NonStream: convertInteractionsResponseToGeminiNonStream,
		},
	)
}
