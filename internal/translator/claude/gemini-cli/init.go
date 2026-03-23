package geminiCLI

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		GeminiCLI,
		Claude,
		convertGeminiCLIRequestToClaude,
		interfaces.TranslateResponse{
			Stream:     convertClaudeResponseToGeminiCLI,
			NonStream:  convertClaudeResponseToGeminiCLINonStream,
			TokenCount: TokenCount,
		},
	)
}
