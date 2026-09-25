package responses

import (
	"bytes"
	"context"
	"fmt"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	geminiresp "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeAntigravityResponsesPayload(rawJSON []byte) []byte {
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[len("data:"):])
	}
	if responseResult := gjson.GetBytes(rawJSON, "response"); responseResult.Exists() {
		rawJSON = []byte(responseResult.Raw)
	}
	for candidateIndex, candidate := range gjson.GetBytes(rawJSON, "candidates").Array() {
		for partIndex, part := range candidate.Get("content.parts").Array() {
			functionCall := part.Get("functionCall")
			name := functionCall.Get("name").String()
			if name == "" {
				continue
			}
			restoredName := translatorcommon.AntigravityUpstreamToolNameToClient(name)
			if restoredName == name {
				continue
			}
			path := fmt.Sprintf("candidates.%d.content.parts.%d.functionCall.name", candidateIndex, partIndex)
			rawJSON, _ = sjson.SetBytes(rawJSON, path, restoredName)
		}
	}
	return rawJSON
}

func convertAntigravityResponseToOpenAIResponses(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	rawJSON = normalizeAntigravityResponsesPayload(rawJSON)
	return geminiresp.ConvertGeminiResponseToOpenAIResponses(
		ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param,
	)
}

func convertAntigravityResponseToOpenAIResponsesNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) []byte {
	rawJSON = normalizeAntigravityResponsesPayload(rawJSON)

	requestResult := gjson.GetBytes(originalRequestRawJSON, "request")
	if requestResult.Exists() {
		originalRequestRawJSON = []byte(requestResult.Raw)
	}

	requestResult = gjson.GetBytes(requestRawJSON, "request")
	if requestResult.Exists() {
		requestRawJSON = []byte(requestResult.Raw)
	}

	return geminiresp.ConvertGeminiResponseToOpenAIResponsesNonStream(
		ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param,
	)
}
