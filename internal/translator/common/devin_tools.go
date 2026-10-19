package common

import (
	"regexp"
	"strings"
)

// IsDevinCodexAppAutomationUpdate identifies the unsupported automation_update tool.
func IsDevinCodexAppAutomationUpdate(namespace, toolName string) bool {
	namespace = strings.TrimSpace(namespace)
	toolName = strings.TrimSpace(toolName)
	return strings.EqualFold(namespace, "mcp__codex_app") && strings.EqualFold(toolName, "automation_update") ||
		strings.EqualFold(toolName, "mcp__codex_app__automation_update")
}

var (
	devinExecCommandDescription = regexp.MustCompile(`(?i)returning output or a session ID for ongoing interaction`)
	devinWriteStdinDescription  = regexp.MustCompile(`(?i)Writes characters to an existing unified exec session and returns recent output(\.?)`)
)

// SanitizeDevinToolDescription adjusts the tool-specific descriptions sent to Devin.
func SanitizeDevinToolDescription(toolName, description string) string {
	name := strings.ToLower(strings.TrimSpace(toolName))
	if name == "exec_command" || strings.HasSuffix(name, "__exec_command") {
		description = devinExecCommandDescription.ReplaceAllString(description, "returning output or an session ID for ongoing interaction")
	}
	if name == "write_stdin" || strings.HasSuffix(name, "__write_stdin") {
		description = devinWriteStdinDescription.ReplaceAllString(description, "Writes characters to a existing unified exec session and returns recent output$1")
	}
	return description
}
