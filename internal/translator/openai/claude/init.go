package claude

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		constant.Claude,
		constant.OpenAI,
		convertClaudeRequestToOpenAIDefault,
		interfaces.TranslateResponse{
			Stream:     convertOpenAIResponseToClaude,
			NonStream:  convertOpenAIResponseToClaudeNonStream,
			TokenCount: tokenCount,
		},
	)
}
