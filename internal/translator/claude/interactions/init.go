package interactions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func init() {
	translator.Register(
		constant.Interactions,
		constant.Claude,
		convertInteractionsRequestToClaude,
		interfaces.TranslateResponse{
			Stream:    convertClaudeResponseToInteractions,
			NonStream: convertClaudeResponseToInteractionsNonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatInteractions, sdktranslator.FormatClaude, convertInteractionsRequestToClaudeChecked)
}
