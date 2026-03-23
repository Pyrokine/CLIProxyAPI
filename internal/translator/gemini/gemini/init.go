package gemini

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/translator"
)

// Register a no-op response translator and a request normalizer for Gemini→Gemini.
// The request converter ensures missing or invalid roles are normalized to valid values.
func init() {
	translator.Register(
		Gemini,
		Gemini,
		convertGeminiRequestToGemini,
		interfaces.TranslateResponse{
			Stream:     passthroughGeminiResponseStream,
			NonStream:  passthroughGeminiResponseNonStream,
			TokenCount: TokenCount,
		},
	)
}
