package chat_completions

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI,
		Codex,
		convertOpenAIRequestToCodex,
		interfaces.TranslateResponse{
			Stream:    convertCodexResponseToOpenAI,
			NonStream: convertCodexResponseToOpenAINonStream,
		},
	)
}
