package util

import "strings"

func IsClaudeCodeAttributionSystemText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return false
	}
	return strings.HasPrefix(text, "x-anthropic-billing-header:")
}
