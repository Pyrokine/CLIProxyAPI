package claude

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		Antigravity,
		convertClaudeRequestToAntigravity,
		interfaces.TranslateResponse{
			Stream:     convertAntigravityResponseToClaude,
			NonStream:  convertAntigravityResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
}
