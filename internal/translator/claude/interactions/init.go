package interactions

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Interactions,
		Claude,
		ConvertInteractionsRequestToClaude,
		interfaces.TranslateResponse{
			Stream:    ConvertClaudeResponseToInteractions,
			NonStream: ConvertClaudeResponseToInteractionsNonStream,
		},
	)
}
