package helps

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

const devinZeroWidthSpace = "\u200b"

// SensitiveWordMatcher matches configured provider-sensitive words.
type SensitiveWordMatcher struct {
	regex *regexp.Regexp
}

// BuildSensitiveWordMatcher compiles a case-insensitive matcher from valid configured words.
func BuildSensitiveWordMatcher(words []string) *SensitiveWordMatcher {
	validWords := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if utf8.RuneCountInString(word) < 2 || strings.Contains(word, devinZeroWidthSpace) {
			continue
		}
		validWords = append(validWords, word)
	}
	if len(validWords) == 0 {
		return nil
	}

	sort.Slice(validWords, func(i, j int) bool {
		return len(validWords[i]) > len(validWords[j])
	})
	escaped := make([]string, len(validWords))
	for i, word := range validWords {
		escaped[i] = regexp.QuoteMeta(word)
	}
	return &SensitiveWordMatcher{
		regex: regexp.MustCompile("(?i)" + strings.Join(escaped, "|")),
	}
}

// Matches reports whether text contains a configured sensitive word.
func (m *SensitiveWordMatcher) Matches(text string) bool {
	return m != nil && m.regex != nil && m.regex.MatchString(text)
}

// ObfuscateText inserts a zero-width space after the first rune of each match.
func (m *SensitiveWordMatcher) ObfuscateText(text string) string {
	if m == nil || m.regex == nil {
		return text
	}
	return m.regex.ReplaceAllStringFunc(text, func(word string) string {
		if strings.Contains(word, devinZeroWidthSpace) {
			return word
		}
		_, size := utf8.DecodeRuneInString(word)
		if size <= 0 || size >= len(word) {
			return word
		}
		return word[:size] + devinZeroWidthSpace + word[size:]
	})
}

// SanitizeDevinSystemPrompt removes unsupported Claude CLI identity lines and configured sensitive lines.
// Matching is limited to the system prompt; callers must not apply this function to messages or tool arguments.
func SanitizeDevinSystemPrompt(prompt string, matcher *SensitiveWordMatcher) string {
	if prompt == "" {
		return ""
	}

	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if util.IsClaudeCodeAttributionSystemText(trimmed) ||
			strings.HasPrefix(trimmed, "You are Claude Code") ||
			strings.Contains(trimmed, "authorized security testing") ||
			strings.Contains(trimmed, "destructive techniques, DoS attacks") ||
			strings.Contains(trimmed, "Claude Code is available as a CLI") ||
			strings.Contains(trimmed, "Fast mode for Claude Code") ||
			matcher.Matches(trimmed) {
			continue
		}
		kept = append(kept, line)
	}

	result := strings.TrimSpace(strings.Join(kept, "\n"))
	return matcher.ObfuscateText(result)
}
