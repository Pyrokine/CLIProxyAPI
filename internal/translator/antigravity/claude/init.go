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
		constant.Antigravity,
		convertClaudeRequestToAntigravity,
		interfaces.TranslateResponse{
			Stream:     convertAntigravityResponseToClaude,
			NonStream:  convertAntigravityResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatClaude, sdktranslator.FormatAntigravity, convertClaudeRequestToAntigravityChecked)
}
