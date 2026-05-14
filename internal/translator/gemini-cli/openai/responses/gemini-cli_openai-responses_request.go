package responses

import (
	geminiclgemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini-cli/gemini"
	geminiresp "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
)

func ConvertOpenAIResponsesRequestToGeminiCLI(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	rawJSON = geminiresp.ConvertOpenAIResponsesRequestToGemini(modelName, rawJSON, stream)
	return geminiclgemini.ConvertGeminiRequestToGeminiCLI(modelName, rawJSON, stream)
}
