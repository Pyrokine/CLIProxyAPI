// Package claude provides request translation functionality for Claude Code API compatibility.
// This package handles the conversion of Claude Code API requests into Gemini CLI-compatible
// JSON format, transforming message contents, system instructions, and tool declarations
// into the format expected by Gemini CLI API clients.
package claude

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
)

// cliConfig defines the path layout for Gemini CLI format.
var cliConfig = &common.ClaudeRequestConfig{
	BaseEnvelope:          `{"model":"","request":{"contents":[]}}`,
	ModelPath:             "model",
	ContentsPath:          "request.contents",
	SystemInstructionPath: "request.systemInstruction",
	ToolsPath:             "request.tools",
	GenConfigPath:         "request.generationConfig",
	SafetyPath:            "request.safetySettings",
	SupportImage:          true,
}

// convertClaudeRequestToCLI parses and transforms a Claude Code API request into Gemini CLI API format.
func convertClaudeRequestToCLI(modelName string, inputRawJSON []byte, _ bool) []byte {
	return common.ConvertClaudeRequest(modelName, inputRawJSON, cliConfig)
}
