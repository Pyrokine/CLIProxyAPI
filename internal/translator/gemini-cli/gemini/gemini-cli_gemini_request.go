// Package gemini provides request translation functionality for Gemini CLI to Gemini API compatibility.
// It handles parsing and transforming Gemini CLI API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini CLI API format and Gemini API's expected format.
package gemini

import (
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToGeminiCLI parses and transforms a Gemini CLI API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Gemini CLI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToGeminiCLI(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	template := ""
	template = `{"project":"","request":{},"model":""}`
	template, _ = sjson.SetRaw(template, "request", string(rawJSON))
	template, _ = sjson.Set(template, "model", gjson.Get(template, "request.model").String())
	template, _ = sjson.Delete(template, "request.model")

	rawJSON, err := common.NormalizeCLIRequest(template)
	if err != nil {
		return []byte{}
	}

	gjson.GetBytes(rawJSON, "request.contents").ForEach(
		func(key, content gjson.Result) bool {
			if content.Get("role").String() == "model" {
				content.Get("parts").ForEach(
					func(partKey, part gjson.Result) bool {
						if part.Get("functionCall").Exists() {
							rawJSON, _ = sjson.SetBytes(
								rawJSON,
								fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", key.Int(), partKey.Int()),
								"skip_thought_signature_validator",
							)
						} else if part.Get("thoughtSignature").Exists() {
							rawJSON, _ = sjson.SetBytes(
								rawJSON,
								fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", key.Int(), partKey.Int()),
								"skip_thought_signature_validator",
							)
						}
						return true
					},
				)
			}
			return true
		},
	)

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}
