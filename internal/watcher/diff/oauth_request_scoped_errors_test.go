package diff

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestSummarizeOAuthRequestScopedErrorsNormalizesAndFilters(t *testing.T) {
	summary := SummarizeOAuthRequestScopedErrors(map[string][]config.RequestScopedErrorRule{
		" Vertex ": {
			{Status: 0, Match: []string{"ignored"}, Action: "stop"},
			{Status: 400, Match: []string{"", "  "}, Action: "stop"},
			{Status: 400, Match: []string{"context-limit"}, Action: "stop"},
		},
		"": {{Status: 500, Match: []string{"ignored"}, Action: "continue"}},
	})

	if len(summary) != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	vertex, ok := summary["vertex"]
	if !ok || vertex.count != 1 || vertex.hash == "" {
		t.Fatalf("vertex summary = %#v", vertex)
	}
	if empty := SummarizeOAuthRequestScopedErrors(nil); empty != nil {
		t.Fatalf("nil summary = %#v", empty)
	}
}

func TestDiffOAuthRequestScopedErrorsTracksOrderAndChannels(t *testing.T) {
	first := config.RequestScopedErrorRule{Status: 400, Match: []string{"first"}, Action: "stop"}
	second := config.RequestScopedErrorRule{Status: 429, MatchRegexr: []string{"^second$"}, Action: "continue"}
	oldMap := map[string][]config.RequestScopedErrorRule{
		"vertex": {first, second},
		"claude": {{Status: 400, Match: []string{"old"}, Action: "stop"}},
	}
	newMap := map[string][]config.RequestScopedErrorRule{
		"VERTEX": {second, first},
		"codex":  {{Status: 422, Match: []string{"new"}, Action: "continue-and-cooldown"}},
	}

	changes, affected := DiffOAuthRequestScopedErrorsChanges(oldMap, newMap)
	expectContains(t, changes, "oauth-request-scoped-errors[vertex]: updated (2 -> 2 entries)")
	expectContains(t, changes, "oauth-request-scoped-errors[claude]: removed")
	expectContains(t, changes, "oauth-request-scoped-errors[codex]: added (1 entries)")
	expectContains(t, affected, "vertex")
	expectContains(t, affected, "claude")
	expectContains(t, affected, "codex")
}

func TestBuildConfigChangeDetailsIncludesRequestScopedErrors(t *testing.T) {
	oldCfg := &config.Config{
		GeminiKey:           []config.GeminiKey{{APIKey: "g"}},
		InteractionsKey:     []config.GeminiKey{{APIKey: "i"}},
		ClaudeKey:           []config.ClaudeKey{{APIKey: "c"}},
		CodexKey:            []config.CodexKey{{APIKey: "x"}},
		XAIKey:              []config.CodexKey{{APIKey: "a"}},
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "compat", BaseURL: "https://example.test"}},
	}
	rule := []config.RequestScopedErrorRule{{Status: 400, Match: []string{"limit"}, Action: "stop"}}
	newCfg := &config.Config{
		GeminiKey:                []config.GeminiKey{{APIKey: "g", RequestScopedErrors: rule}},
		InteractionsKey:          []config.GeminiKey{{APIKey: "i", RequestScopedErrors: rule}},
		ClaudeKey:                []config.ClaudeKey{{APIKey: "c", RequestScopedErrors: rule}},
		CodexKey:                 []config.CodexKey{{APIKey: "x", RequestScopedErrors: rule}},
		XAIKey:                   []config.CodexKey{{APIKey: "a", RequestScopedErrors: rule}},
		OpenAICompatibility:      []config.OpenAICompatibility{{Name: "compat", BaseURL: "https://example.test", RequestScopedErrors: rule}},
		OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{"vertex": rule},
	}

	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	for _, expected := range []string{
		"gemini[0].request-scoped-errors: updated (0 -> 1 entries)",
		"interactions[0].request-scoped-errors: updated (0 -> 1 entries)",
		"claude[0].request-scoped-errors: updated (0 -> 1 entries)",
		"codex[0].request-scoped-errors: updated (0 -> 1 entries)",
		"xai[0].request-scoped-errors: updated (0 -> 1 entries)",
		"  provider updated: compat (request-scoped-errors updated (0 -> 1 entries))",
		"oauth-request-scoped-errors[vertex]: added (1 entries)",
	} {
		expectContains(t, changes, expected)
	}
}
