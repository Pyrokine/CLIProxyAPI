package responses

import (
	antigravgemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/gemini"
	geminiresp "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
)

func ConvertOpenAIResponsesRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	rawJSON = geminiresp.ConvertOpenAIResponsesRequestToGemini(modelName, rawJSON, stream)
	return antigravgemini.ConvertGeminiRequestToAntigravity(modelName, rawJSON, stream)
}
