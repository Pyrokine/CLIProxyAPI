package geminiCLI

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		constant.GeminiCLI,
		constant.Codex,
		ConvertGeminiCLIRequestToCodex,
		interfaces.TranslateResponse{
			Stream:     ConvertCodexResponseToGeminiCLI,
			NonStream:  ConvertCodexResponseToGeminiCLINonStream,
			TokenCount: tokenCount,
		},
	)
}
