package helps

import (
	"strings"
	"testing"
)

func TestBuildSensitiveWordMatcherFiltersInvalidEntries(t *testing.T) {
	matcher := BuildSensitiveWordMatcher([]string{"", "x", " proxy ", "Nous Research", "p" + devinZeroWidthSpace + "roxy"})
	if matcher == nil {
		t.Fatal("BuildSensitiveWordMatcher() = nil")
	}
	if !matcher.Matches("PROXY and nous research") {
		t.Fatal("matcher did not match configured words case-insensitively")
	}
	if matcher.Matches("unrelated") {
		t.Fatal("matcher matched unrelated text")
	}
}

func TestSanitizeDevinSystemPromptDeletesMatchingLines(t *testing.T) {
	matcher := BuildSensitiveWordMatcher([]string{"blocked phrase", "proxy"})
	prompt := "keep this line\r\nremove blocked phrase completely\r\nremove proxy line"

	got := SanitizeDevinSystemPrompt(prompt, matcher)
	if strings.Contains(got, "blocked phrase") || strings.Contains(got, "proxy") {
		t.Fatalf("SanitizeDevinSystemPrompt() retained a matching line: %q", got)
	}
	if got != "keep this line" {
		t.Fatalf("SanitizeDevinSystemPrompt() = %q, want %q", got, "keep this line")
	}
	if obfuscated := matcher.ObfuscateText("use proxy"); obfuscated != "use p"+devinZeroWidthSpace+"roxy" {
		t.Fatalf("ObfuscateText() = %q", obfuscated)
	}
}

func TestSanitizeDevinSystemPromptLeavesUnconfiguredContentUnchanged(t *testing.T) {
	const prompt = "ordinary user-defined system text\nwith tool guidance"
	if got := SanitizeDevinSystemPrompt(prompt, nil); got != prompt {
		t.Fatalf("SanitizeDevinSystemPrompt() = %q, want %q", got, prompt)
	}
}
