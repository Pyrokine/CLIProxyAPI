package gemini

import (
	"bytes"
	"context"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
)

// passthroughGeminiResponseStream forwards Gemini responses unchanged.
func passthroughGeminiResponseStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) [][]byte {
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return [][]byte{}
	}

	return [][]byte{rawJSON}
}

// passthroughGeminiResponseNonStream forwards Gemini responses unchanged.
func passthroughGeminiResponseNonStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	return rawJSON
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}
