package helps

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestIsClaudeMCPToolName(t *testing.T) {
	for _, name := range []string{
		"mcp__context7__query-docs",
		"mcp__amber_cedar__quiet_harbor",
		"mcp__server__tool__variant",
	} {
		if !IsClaudeMCPToolName(name) {
			t.Fatalf("IsClaudeMCPToolName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"context7__query-docs",
		"mcp____query-docs",
		"mcp__context7__",
		"mcp__context7__query.docs",
		"mcp__context7__" + strings.Repeat("x", 64),
	} {
		if IsClaudeMCPToolName(name) {
			t.Fatalf("IsClaudeMCPToolName(%q) = true, want false", name)
		}
	}
}

func TestClaudeMCPToolAlias(t *testing.T) {
	first := ClaudeMCPToolAlias("credential-secret", "search_web", 0)
	if second := ClaudeMCPToolAlias("credential-secret", "search_web", 0); second != first {
		t.Fatalf("alias is not deterministic: %q != %q", first, second)
	}
	caseDistinct := ClaudeMCPToolAlias("credential-secret", "Search_Web", 0)
	if first == caseDistinct {
		t.Fatalf("case-distinct names produced the same initial alias: %q", first)
	}
	retry := ClaudeMCPToolAlias("credential-secret", "search_web", 1)
	if first == retry {
		t.Fatalf("collision retry did not change alias: %q", first)
	}
	if !IsClaudeMCPToolName(first) {
		t.Fatalf("generated alias %q is not a valid MCP tool name", first)
	}
	if !strings.HasSuffix(first, "_search_web") {
		t.Fatalf("generated alias %q does not preserve the semantic suffix", first)
	}
	if matched, _ := regexp.MatchString(`^mcp__[a-z]+_[a-z]+__[a-z]+_search_web$`, first); !matched {
		t.Fatalf("generated alias %q does not contain word-based IDs plus semantics", first)
	}
	assertClaudeMCPAliasWords(t, first)
	server := strings.Split(first, "__")[1]
	if got := strings.Split(caseDistinct, "__")[1]; got != server {
		t.Fatalf("case-distinct tool server = %q, want shared caller server %q", got, server)
	}
	if got := strings.Split(retry, "__")[1]; got != server {
		t.Fatalf("retry server = %q, want shared caller server %q", got, server)
	}
	if got := strings.Split(ClaudeMCPToolAlias("other-caller", "search_web", 0), "__")[1]; got == server {
		t.Fatalf("different caller unexpectedly shared server %q", server)
	}
}

func TestClaudeMCPToolAliasSemanticSuffixIsSafeAndBounded(t *testing.T) {
	tests := []struct {
		name       string
		original   string
		wantSuffix string
	}{
		{name: "invalid separators", original: "browser.open URL", wantSuffix: "_browser_open_URL"},
		{name: "unicode mixed", original: "search.网页/tool with spaces", wantSuffix: "_search_tool_with_spaces"},
		{name: "unicode only", original: "搜索网页", wantSuffix: "_tool"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alias := ClaudeMCPToolAlias("credential-secret", test.original, 0)
			if !IsClaudeMCPToolName(alias) {
				t.Fatalf("generated alias %q is not a valid MCP tool name", alias)
			}
			if len(alias) > 64 {
				t.Fatalf("generated alias length = %d, want <= 64: %q", len(alias), alias)
			}
			if !strings.HasSuffix(alias, test.wantSuffix) {
				t.Fatalf("generated alias %q does not end in %q", alias, test.wantSuffix)
			}
		})
	}

	const original = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	alias := ClaudeMCPToolAlias("credential-secret", original, 0)
	semanticStart := strings.LastIndex(alias, "_") + 1
	if semanticStart == 0 {
		t.Fatalf("generated alias %q has no semantic separator", alias)
	}
	wantSemanticLen := 64 - semanticStart
	if got := alias[semanticStart:]; got != strings.Repeat("a", wantSemanticLen) {
		t.Fatalf("semantic suffix = %q, want %d a's", got, wantSemanticLen)
	}
	if len(alias) != 64 {
		t.Fatalf("generated alias length = %d, want 64: %q", len(alias), alias)
	}
}

func TestClaudeMCPAliasEnglishWordlistIntegrity(t *testing.T) {
	if got := len(claudeMCPAliasEnglishWords); got != 2048 {
		t.Fatalf("word count = %d, want 2048", got)
	}
	if first, last := claudeMCPAliasEnglishWords[0], claudeMCPAliasEnglishWords[len(claudeMCPAliasEnglishWords)-1]; first != "abandon" || last != "zoo" {
		t.Fatalf("wordlist endpoints = %q, %q, want abandon, zoo", first, last)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(rawBIP39EnglishWords))); got != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("wordlist SHA-256 = %s", got)
	}

	seen := make(map[string]bool, len(claudeMCPAliasEnglishWords))
	for _, word := range claudeMCPAliasEnglishWords {
		if len(word) < 1 || len(word) > 8 {
			t.Fatalf("word %q has length %d, want 1..8", word, len(word))
		}
		for _, char := range word {
			if char < 'a' || char > 'z' {
				t.Fatalf("word %q contains non-lowercase-ASCII character %q", word, char)
			}
		}
		if seen[word] {
			t.Fatalf("word %q is duplicated", word)
		}
		seen[word] = true
	}
}

func TestClaudeMCPToolAliasProbesAllWordsWithoutDuplicates(t *testing.T) {
	const seed = "test-seed"
	const original = "tool.name"
	seen := make(map[string]bool, ClaudeMCPAliasWordCount())
	for attempt := 0; attempt < ClaudeMCPAliasWordCount(); attempt++ {
		alias := ClaudeMCPToolAlias(seed, original, uint32(attempt))
		parts := strings.Split(alias, "__")
		toolID, _, ok := strings.Cut(parts[2], "_")
		if !ok {
			t.Fatalf("alias %q has no semantic suffix", alias)
		}
		if seen[toolID] {
			t.Fatalf("attempt %d generated duplicate tool ID %q", attempt, toolID)
		}
		seen[toolID] = true
	}
	if len(seen) != ClaudeMCPAliasWordCount() {
		t.Fatalf("covered %d words, want %d", len(seen), ClaudeMCPAliasWordCount())
	}
}

func TestAllocateClaudeMCPToolAliasUsesBoundedLinearProbe(t *testing.T) {
	const seed = "exhaust-space"
	const original = "tool.name"
	first := ClaudeMCPToolAlias(seed, original, 0)
	if alias, ok := AllocateClaudeMCPToolAlias(seed, original, nil); !ok || alias != first {
		t.Fatalf("empty allocation = %q, %v, want %q, true", alias, ok, first)
	}
	reserved := map[string]bool{first: true}
	second := ClaudeMCPToolAlias(seed, original, 1)
	if alias, ok := AllocateClaudeMCPToolAlias(seed, original, reserved); !ok || alias != second {
		t.Fatalf("collision allocation = %q, %v, want %q, true", alias, ok, second)
	}
	for attempt := 0; attempt < ClaudeMCPAliasWordCount(); attempt++ {
		reserved[ClaudeMCPToolAlias(seed, original, uint32(attempt))] = true
	}
	if alias, ok := AllocateClaudeMCPToolAlias(seed, original, reserved); ok || alias != "" {
		t.Fatalf("exhausted allocation = %q, %v, want empty, false", alias, ok)
	}
}

func TestClaudeMCPToolAliasStrict64CharLimitAcrossWordSpace(t *testing.T) {
	for attempt := 0; attempt < ClaudeMCPAliasWordCount(); attempt++ {
		secret := fmt.Sprintf("test-secret-%d", attempt)
		original := strings.Repeat(fmt.Sprintf("tool_%d_long_name_", attempt), 50)
		alias := ClaudeMCPToolAlias(secret, original, uint32(attempt))
		if len(alias) > 64 {
			t.Fatalf("alias length %d exceeds 64: %q", len(alias), alias)
		}
		if !IsClaudeMCPToolName(alias) {
			t.Fatalf("alias %q is not a valid MCP tool name", alias)
		}
		assertClaudeMCPAliasWords(t, alias)
	}
}

func assertClaudeMCPAliasWords(t *testing.T, alias string) {
	t.Helper()
	parts := strings.Split(alias, "__")
	if len(parts) != 3 {
		t.Fatalf("alias %q does not have MCP, server, and tool parts", alias)
	}
	serverWords := strings.Split(parts[1], "_")
	if len(serverWords) != 2 {
		t.Fatalf("alias %q server %q is not two words", alias, parts[1])
	}
	toolID, _, ok := strings.Cut(parts[2], "_")
	if !ok {
		t.Fatalf("alias %q tool component %q has no semantic suffix", alias, parts[2])
	}
	allowed := make(map[string]bool, len(claudeMCPAliasEnglishWords))
	for _, word := range claudeMCPAliasEnglishWords {
		allowed[word] = true
	}
	for _, word := range []string{serverWords[0], serverWords[1], toolID} {
		if !allowed[word] {
			t.Fatalf("alias %q uses non-BIP-39 word %q", alias, word)
		}
	}
}
