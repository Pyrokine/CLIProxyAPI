// Package chat_completions provides request translation functionality for OpenAI to Gemini CLI API compatibility.
// It converts OpenAI Chat Completions requests into Gemini CLI compatible JSON using gjson/sjson only.
package chat_completions

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
)

// antigravityRequestConfig defines the Antigravity-specific request conversion behavior.
// Antigravity uses a request envelope with camelCase MIME type fields and strict function handling
// (JSON validation for args, JSON parsing for responses, inclusion of function IDs).
var antigravityRequestConfig = common.RequestConfig{
	BaseEnvelope:          `{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`,
	PathPrefix:            "request.",
	SystemInstructionKey:  "systemInstruction",
	MimeTypeField:         "mimeType",
	SupportMaxTokens:      true,
	IncludeFunctionIDs:    true,
	ValidateFunctionArgs:  true,
	ParseFunctionResponse: true,
	CheckTextEmptiness:    true,
}

// convertOpenAIRequestToAntigravity converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini CLI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini CLI API format
func convertOpenAIRequestToAntigravity(modelName string, rawJSON []byte, _ bool) []byte {
	return common.ConvertOpenAIRequest(modelName, rawJSON, &antigravityRequestConfig)
}
