package interactions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		constant.Interactions,
		constant.Antigravity,
		convertInteractionsRequestToAntigravity,
		interfaces.TranslateResponse{
			Stream:    convertAntigravityResponseToInteractions,
			NonStream: convertAntigravityResponseToInteractionsNonStream,
		},
	)
}
