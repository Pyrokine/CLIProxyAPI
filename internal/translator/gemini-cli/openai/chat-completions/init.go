package chat_completions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func init() {
	translator.Register(
		constant.OpenAI,
		constant.GeminiCLI,
		convertOpenAIRequestToGeminiCLI,
		interfaces.TranslateResponse{
			Stream:    convertCliResponseToOpenAI,
			NonStream: convertCliResponseToOpenAINonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatOpenAI, sdktranslator.FormatGeminiCLI, convertOpenAIRequestToGeminiCLIChecked)
}
