// Package gemini provides request translation functionality for Gemini CLI to Gemini API compatibility.
// It handles parsing and transforming Gemini CLI API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini CLI API format and Gemini API's expected format.
package gemini

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToAntigravity parses and transforms a Gemini CLI API request into Gemini API format.
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
func ConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	template := ""
	template = `{"project":"","request":{},"model":""}`
	template, _ = sjson.SetRaw(template, "request", string(rawJSON))
	template, _ = sjson.Set(template, "model", modelName)
	template, _ = sjson.Delete(template, "request.model")

	rawJSON, err := common.NormalizeCLIRequest(template)
	if err != nil {
		return []byte{}
	}

	// Gemini-specific handling for non-Claude models:
	// - Add skip_thought_signature_validator to functionCall parts so upstream can bypass signature validation.
	// - Also mark thinking parts with the same sentinel when present (we keep the parts; we only annotate them).
	if !strings.Contains(modelName, "claude") {
		const skipSentinel = "skip_thought_signature_validator"

		gjson.GetBytes(rawJSON, "request.contents").ForEach(
			func(contentIdx, content gjson.Result) bool {
				if content.Get("role").String() == "model" {
					// First pass: collect indices of thinking parts to mark with skip sentinel
					var thinkingIndicesToSkipSignature []int64
					content.Get("parts").ForEach(
						func(partIdx, part gjson.Result) bool {
							// Collect indices of thinking blocks to mark with skip sentinel
							if part.Get("thought").Bool() {
								thinkingIndicesToSkipSignature = append(thinkingIndicesToSkipSignature, partIdx.Int())
							}
							// Add skip sentinel to functionCall parts
							if part.Get("functionCall").Exists() {
								existingSig := part.Get("thoughtSignature").String()
								if existingSig == "" || len(existingSig) < 50 {
									rawJSON, _ = sjson.SetBytes(
										rawJSON, fmt.Sprintf(
											"request.contents.%d.parts.%d.thoughtSignature", contentIdx.Int(),
											partIdx.Int(),
										), skipSentinel,
									)
								}
							}
							return true
						},
					)

					// Add skip_thought_signature_validator sentinel to thinking blocks in reverse order to preserve indices
					for i := len(thinkingIndicesToSkipSignature) - 1; i >= 0; i-- {
						idx := thinkingIndicesToSkipSignature[i]
						rawJSON, _ = sjson.SetBytes(
							rawJSON,
							fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", contentIdx.Int(), idx),
							skipSentinel,
						)
					}
				}
				return true
			},
		)
	}

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}
