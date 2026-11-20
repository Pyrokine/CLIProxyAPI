package util

import (
	"strings"

	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
)

// ensureClaudeModelIDPrefix 保留旧版反转格式的模型 ID
func ensureClaudeModelIDPrefix(id string) string {
	return claudemodels.EnsureClaudeModelIDPrefix(id)
}

// resolveClaudeModelIDPrefix 还原旧版模型 ID，保留思考后缀
func resolveClaudeModelIDPrefix(id string) string {
	return claudemodels.ResolveClaudeModelIDPrefix(id)
}

// IsClaudeThinkingModel checks if the model is a Claude thinking model
// that requires the interleaved-thinking beta header.
func IsClaudeThinkingModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "claude") && strings.Contains(lower, "thinking")
}

// IsClaudeModel reports whether a model ID identifies a Claude model.
func IsClaudeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}
