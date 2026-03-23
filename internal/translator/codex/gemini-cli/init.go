package geminiCLI

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		GeminiCLI,
		Codex,
		convertGeminiCLIRequestToCodex,
		interfaces.TranslateResponse{
			Stream:     convertCodexResponseToGeminiCLI,
			NonStream:  convertCodexResponseToGeminiCLINonStream,
			TokenCount: TokenCount,
		},
	)
}
