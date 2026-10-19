package util

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const geminiClaudeToolUseIDPrefix = "cpa_gemini_"

var (
	claudeToolUseIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	claudeToolUseIDCounter   uint64
)

// SanitizeClaudeToolID ensures the given id conforms to Claude's
// tool_use.id regex ^[a-zA-Z0-9_-]+$.  Non-conforming characters are
// replaced with '_'; an empty result gets a generated fallback.
func SanitizeClaudeToolID(id string) string {
	s := claudeToolUseIDSanitizer.ReplaceAllString(id, "_")
	if s == "" {
		s = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&claudeToolUseIDCounter, 1))
	}
	return s
}

// SanitizeClaudeFunctionName 将工具名称限制为 Claude 接受的字符和长度
func SanitizeClaudeFunctionName(name string) string {
	sanitized := claudeToolUseIDSanitizer.ReplaceAllString(name, "_")
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}

// GeminiClaudeToolUseID returns a stable Claude-facing ID for a provider-native
// Gemini function call. The opaque ID binds replay to the original call semantics.
func GeminiClaudeToolUseID(callID, name, argsRaw string) string {
	callID = strings.TrimSpace(callID)
	name = strings.TrimSpace(name)
	if callID == "" || name == "" {
		return ""
	}
	if strings.TrimSpace(argsRaw) != "" {
		var value any
		if json.Unmarshal([]byte(argsRaw), &value) == nil {
			if canonical, err := json.Marshal(value); err == nil {
				argsRaw = string(canonical)
			}
		} else {
			argsRaw = strings.TrimSpace(argsRaw)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{callID, name, argsRaw}, "\x00")))
	return geminiClaudeToolUseIDPrefix + hex.EncodeToString(sum[:16])
}

// IsGeminiClaudeToolUseID reports whether id belongs to the reserved
// Claude-facing Gemini provenance namespace.
func IsGeminiClaudeToolUseID(id string) bool {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, geminiClaudeToolUseIDPrefix) {
		return false
	}
	digest := strings.TrimPrefix(id, geminiClaudeToolUseIDPrefix)
	if len(digest) != 32 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
