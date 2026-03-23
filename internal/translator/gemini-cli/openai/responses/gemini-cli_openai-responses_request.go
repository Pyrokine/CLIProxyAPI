package responses

import (
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini-cli/gemini"
	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
)

func convertOpenAIResponsesRequestToGeminiCLI(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	rawJSON = ConvertOpenAIResponsesRequestToGemini(modelName, rawJSON, stream)
	return ConvertGeminiRequestToGeminiCLI(modelName, rawJSON, stream)
}
