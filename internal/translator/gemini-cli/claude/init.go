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
		constant.GeminiCLI,
		convertClaudeRequestToCLI,
		interfaces.TranslateResponse{
			Stream:     convertGeminiCLIResponseToClaude,
			NonStream:  convertGeminiCLIResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatClaude, sdktranslator.FormatGeminiCLI, convertClaudeRequestToCLIChecked)
}
