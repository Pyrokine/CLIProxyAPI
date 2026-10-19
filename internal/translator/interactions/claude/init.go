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
		constant.Interactions,
		convertClaudeRequestToInteractionsDefault,
		interfaces.TranslateResponse{
			Stream:    convertInteractionsResponseToClaude,
			NonStream: convertInteractionsResponseToClaudeNonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatClaude, sdktranslator.FormatInteractions, func(model string, body []byte, stream bool) ([]byte, error) {
		return convertClaudeRequestToInteractionsChecked(model, body, stream, false)
	})
}
