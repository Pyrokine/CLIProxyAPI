// Package geminiCLI provides request translation functionality for Gemini CLI to Gemini API.
// It handles parsing and transforming Gemini CLI API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Gemini CLI API format and Gemini API's expected format.
package geminiCLI

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertGeminiCLIRequestToGemini parses and transforms a Gemini CLI API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
func convertGeminiCLIRequestToGemini(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	modelResult := gjson.GetBytes(rawJSON, "model")
	rawJSON = []byte(gjson.GetBytes(rawJSON, "request").Raw)
	rawJSON, _ = sjson.SetBytes(rawJSON, "model", modelResult.String())
	if gjson.GetBytes(rawJSON, "systemInstruction").Exists() {
		rawJSON, _ = sjson.SetRawBytes(
			rawJSON, "system_instruction", []byte(gjson.GetBytes(rawJSON, "systemInstruction").Raw),
		)
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "systemInstruction")
	}

	rawJSON = common.RenameToolParameters(rawJSON)
	rawJSON = common.InjectThoughtSignatures(rawJSON, "contents")

	return common.AttachDefaultSafetySettings(rawJSON, "safetySettings")
}
