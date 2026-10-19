package responses

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"
)

// translateGeminiFrameWithEOF 将有限响应样本的 finishReason 与传输结束一并转换
func translateGeminiFrameWithEOF(ctx context.Context, modelName string, originalRequest, translatedRequest, frame []byte, state *any) [][]byte {
	events := ConvertGeminiResponseToOpenAIResponses(ctx, modelName, originalRequest, translatedRequest, frame, state)
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(frame)), "data:"))
	root := unwrapGeminiResponseRoot(gjson.Parse(payload))
	if terminal, ready := geminiResponsesTerminalFromRoot(root, false); ready && terminal.status != "failed" {
		events = append(events, ConvertGeminiResponseToOpenAIResponses(ctx, modelName, originalRequest, translatedRequest, []byte("[DONE]"), state)...)
	}
	return events
}
