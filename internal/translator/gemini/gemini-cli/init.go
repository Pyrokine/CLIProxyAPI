package geminiCLI

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		GeminiCLI,
		Gemini,
		convertGeminiCLIRequestToGemini,
		interfaces.TranslateResponse{
			Stream:     convertGeminiResponseToGeminiCLI,
			NonStream:  convertGeminiResponseToGeminiCLINonStream,
			TokenCount: TokenCount,
		},
	)
}
