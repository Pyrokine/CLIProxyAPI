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
		constant.Codex,
		convertInteractionsRequestToCodex,
		interfaces.TranslateResponse{
			Stream:    convertCodexResponseToInteractions,
			NonStream: convertCodexResponseToInteractionsNonStream,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatInteractions, sdktranslator.FormatCodex, convertInteractionsRequestToCodexChecked)
}
