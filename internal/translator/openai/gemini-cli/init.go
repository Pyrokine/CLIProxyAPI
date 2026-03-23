package geminiCLI

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		GeminiCLI,
		OpenAI,
		convertGeminiCLIRequestToOpenAI,
		interfaces.TranslateResponse{
			Stream:     convertOpenAIResponseToGeminiCLI,
			NonStream:  convertOpenAIResponseToGeminiCLINonStream,
			TokenCount: TokenCount,
		},
	)
}
