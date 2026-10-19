package responses

import (
	geminiclgemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini-cli/gemini"
	geminiresp "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
)

func convertOpenAIResponsesRequestToGeminiCLI(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, _ := convertOpenAIResponsesRequestToGeminiCLIChecked(modelName, inputRawJSON, stream)
	return out
}

func convertOpenAIResponsesRequestToGeminiCLIChecked(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	geminiRequest, err := geminiresp.ConvertOpenAIResponsesRequestToGeminiChecked(modelName, inputRawJSON, stream)
	if err != nil {
		return nil, err
	}
	return geminiclgemini.ConvertGeminiRequestToGeminiCLI(modelName, geminiRequest, stream), nil
}
