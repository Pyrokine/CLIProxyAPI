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
		constant.Codex,
		ConvertClaudeRequestToCodex,
		interfaces.TranslateResponse{
			Stream:     ConvertCodexResponseToClaude,
			NonStream:  ConvertCodexResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
	sdktranslator.RegisterCheckedRequest(sdktranslator.FormatClaude, sdktranslator.FormatCodex, func(model string, body []byte, stream bool) ([]byte, error) {
		return convertClaudeRequestToCodexChecked(model, body, stream, false)
	})
}
