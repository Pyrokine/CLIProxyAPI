package responses

import (
	"context"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func init() {
	translator.Register(
		constant.OpenaiResponse,
		constant.Antigravity,
		convertOpenAIResponsesRequestToAntigravity,
		interfaces.TranslateResponse{
			Stream:    convertAntigravityResponseToOpenAIResponses,
			NonStream: convertAntigravityResponseToOpenAIResponsesNonStream,
		},
	)
	sdktranslator.RegisterRequestEnvelope(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatAntigravity,
		func(_ context.Context, req sdktranslator.RequestEnvelope) sdktranslator.RequestEnvelope {
			req.Body, req.Err = convertOpenAIResponsesRequestToAntigravityChecked(req.Model, req.Body, req.Stream, req.ModelInfo)
			return req
		},
	)
}
