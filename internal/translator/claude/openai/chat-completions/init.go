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
		constant.Claude,
		convertOpenAIRequestToClaudeDefault,
		interfaces.TranslateResponse{
			Stream:    convertClaudeResponseToOpenAI,
			NonStream: convertClaudeResponseToOpenAINonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, func(model string, body []byte, stream bool) ([]byte, error) {
		return convertOpenAIRequestToClaudeChecked(model, body, stream, false)
	})
}
