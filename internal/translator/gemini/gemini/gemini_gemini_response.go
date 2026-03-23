package gemini

import (
	"bytes"
	"context"
	"fmt"
)

// passthroughGeminiResponseStream forwards Gemini responses unchanged.
func passthroughGeminiResponseStream(
	_ context.Context,
	_ string,
	_, _, rawJSON []byte,
	_ *any,
) []string {
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return []string{}
	}

	return []string{string(rawJSON)}
}

// passthroughGeminiResponseNonStream forwards Gemini responses unchanged.
func passthroughGeminiResponseNonStream(
	_ context.Context,
	_ string,
	_, _, rawJSON []byte,
	_ *any,
) string {
	return string(rawJSON)
}

func TokenCount(_ context.Context, count int64) string {
	return fmt.Sprintf(`{"totalTokens":%d,"promptTokensDetails":[{"modality":"TEXT","tokenCount":%d}]}`, count, count)
}
