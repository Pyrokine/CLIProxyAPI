// Package chat_completions provides request translation functionality for OpenAI to Gemini CLI API compatibility.
// It converts OpenAI Chat Completions requests into Gemini CLI compatible JSON using gjson/sjson only.
package chat_completions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
)

// geminiCLIRequestConfig defines the Gemini CLI-specific request conversion behavior.
// Gemini CLI uses a request envelope with snake_case MIME type fields and direct function arg passthrough.
var geminiCLIRequestConfig = common.RequestConfig{
	BaseEnvelope:         `{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`,
	PathPrefix:           "request.",
	SystemInstructionKey: "systemInstruction",
	MimeTypeField:        "mime_type",
}

// convertOpenAIRequestToGeminiCLI converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini CLI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini CLI API format
func convertOpenAIRequestToGeminiCLI(modelName string, rawJSON []byte, _ bool) []byte {
	return common.ConvertOpenAIRequest(modelName, rawJSON, &geminiCLIRequestConfig)
}
