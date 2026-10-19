package geminiCLI

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func init() {
	translator.Register(
		constant.GeminiCLI,
		constant.Claude,
		convertGeminiCLIRequestToClaude,
		interfaces.TranslateResponse{
			Stream:     convertClaudeResponseToGeminiCLI,
			NonStream:  convertClaudeResponseToGeminiCLINonStream,
			TokenCount: tokenCount,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatGeminiCLI, sdktranslator.FormatClaude, convertGeminiCLIRequestToClaudeChecked)
}
