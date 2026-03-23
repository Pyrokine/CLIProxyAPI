// Package claude provides request translation functionality for Claude API.
// It handles parsing and transforming Claude API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
package claude

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
)

// directConfig defines the path layout for direct Gemini API format.
var directConfig = &common.ClaudeRequestConfig{
	BaseEnvelope:          `{"contents":[]}`,
	ModelPath:             "model",
	ContentsPath:          "contents",
	SystemInstructionPath: "system_instruction",
	ToolsPath:             "tools",
	GenConfigPath:         "generationConfig",
	SafetyPath:            "safetySettings",
	SupportImage:          false,
}

// convertClaudeRequestToGemini parses a Claude API request and returns a complete
// Gemini CLI request body (as JSON bytes) ready to be sent via SendRawMessageStream.
// All JSON transformations are performed using gjson/sjson.
func convertClaudeRequestToGemini(modelName string, inputRawJSON []byte, _ bool) []byte {
	return common.ConvertClaudeRequest(modelName, inputRawJSON, directConfig)
}
