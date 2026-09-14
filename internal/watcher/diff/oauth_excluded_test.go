package diff

import (
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestSummarizeExcludedModels_NormalizesAndDedupes(t *testing.T) {
	summary := summarizeExcludedModels([]string{"A", " a ", "B", "b"})
	if summary.count != 2 {
		t.Fatalf("expected 2 unique entries, got %d", summary.count)
	}
	if summary.hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if empty := summarizeExcludedModels(nil); empty.count != 0 || empty.hash != "" {
		t.Fatalf("expected empty summary for nil input, got %+v", empty)
	}
}

func TestOAuthExcludedModelChanges(t *testing.T) {
	oldMap := map[string][]string{
		"ProviderA": {"model-1", "model-2"},
		"providerB": {"x"},
	}
	newMap := map[string][]string{
		"providerA": {"model-1", "model-3"},
		"providerC": {"y"},
	}

	changes, affected := OAuthExcludedModelChanges(oldMap, newMap)
	expectContains(t, changes, "oauth-excluded-models[providera]: updated (2 -> 2 entries)")
	expectContains(t, changes, "oauth-excluded-models[providerb]: removed")
	expectContains(t, changes, "oauth-excluded-models[providerc]: added (1 entries)")

	if len(affected) != 3 {
		t.Fatalf("expected 3 affected providers, got %d", len(affected))
	}
}

func TestSummarizeOAuthExcludedModels_NormalizesKeys(t *testing.T) {
	out := summarizeOAuthExcludedModels(
		map[string][]string{
			"ProvA": {"X"},
			"":      {"ignored"},
		},
	)
	if len(out) != 1 {
		t.Fatalf("expected only non-empty key summary, got %d", len(out))
	}
	if _, ok := out["prova"]; !ok {
		t.Fatalf("expected normalized key 'prova', got keys %v", out)
	}
	if out["prova"].count != 1 || out["prova"].hash == "" {
		t.Fatalf("unexpected summary %+v", out["prova"])
	}
	if outEmpty := summarizeOAuthExcludedModels(nil); outEmpty != nil {
		t.Fatalf("expected nil map for nil input, got %v", outEmpty)
	}
}

func TestSummarizeVertexModels(t *testing.T) {
	summary := summarizeVertexModels(
		[]config.VertexCompatModel{
			{Name: "m1"},
			{Name: " ", Alias: "alias"},
			{}, // ignored
		},
	)
	if summary.count != 2 {
		t.Fatalf("expected 2 vertex models, got %d", summary.count)
	}
	if summary.hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if empty := summarizeVertexModels(nil); empty.count != 0 || empty.hash != "" {
		t.Fatalf("expected empty summary for nil input, got %+v", empty)
	}
	if blank := summarizeVertexModels([]config.VertexCompatModel{{Name: " "}}); blank.count != 0 || blank.hash != "" {
		t.Fatalf("expected blank model ignored, got %+v", blank)
	}
}

func TestConfiguredModelSummariesIncludeThinking(t *testing.T) {
	thinking := &registry.ThinkingSupport{Levels: []string{"high"}}
	tests := []struct {
		name string
		base func() modelsSummary
		with func() modelsSummary
	}{
		{
			name: "gemini",
			base: func() modelsSummary {
				return summarizeGeminiModels([]config.GeminiModel{{Name: "model", Alias: "alias"}})
			},
			with: func() modelsSummary {
				return summarizeGeminiModels([]config.GeminiModel{{Name: "model", Alias: "alias", Thinking: thinking}})
			},
		},
		{
			name: "claude",
			base: func() modelsSummary {
				return summarizeClaudeModels([]config.ClaudeModel{{Name: "model", Alias: "alias"}})
			},
			with: func() modelsSummary {
				return summarizeClaudeModels([]config.ClaudeModel{{Name: "model", Alias: "alias", Thinking: thinking}})
			},
		},
		{
			name: "codex",
			base: func() modelsSummary {
				return summarizeCodexModels([]config.CodexModel{{Name: "model", Alias: "alias"}})
			},
			with: func() modelsSummary {
				return summarizeCodexModels([]config.CodexModel{{Name: "model", Alias: "alias", Thinking: thinking}})
			},
		},
		{
			name: "vertex",
			base: func() modelsSummary {
				return summarizeVertexModels([]config.VertexCompatModel{{Name: "model", Alias: "alias"}})
			},
			with: func() modelsSummary {
				return summarizeVertexModels(
					[]config.VertexCompatModel{
						{
							Name: "model", Alias: "alias", Thinking: thinking,
						},
					},
				)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				base := testCase.base()
				with := testCase.with()
				if base.hash == "" || with.hash == "" || base.hash == with.hash {
					t.Fatalf("summary hashes = %q / %q, want distinct non-empty values", base.hash, with.hash)
				}
			},
		)
	}
}

func expectContains(t *testing.T, list []string, target string) {
	t.Helper()
	if slices.Contains(list, target) {
		return
	}
	t.Fatalf("expected list to contain %q, got %#v", target, list)
}
