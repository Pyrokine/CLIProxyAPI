// Package chat_completions provides request translation functionality for OpenAI to Gemini API compatibility.
// It converts OpenAI Chat Completions requests into Gemini compatible JSON using gjson/sjson only.
package chat_completions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
)

// geminiRequestConfig defines the direct Gemini API-specific request conversion behavior.
// Direct Gemini uses no request envelope, snake_case system_instruction key, and supports max_tokens.
var geminiRequestConfig = common.RequestConfig{
	BaseEnvelope:         `{"contents":[]}`,
	SystemInstructionKey: "system_instruction",
	MimeTypeField:        "mime_type",
	SupportMaxTokens:     true,
	CheckTextEmptiness:   true,
}

// convertOpenAIRequestToGemini converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func convertOpenAIRequestToGemini(modelName string, rawJSON []byte, _ bool) []byte {
	return common.ConvertOpenAIRequest(modelName, rawJSON, &geminiRequestConfig)
}
