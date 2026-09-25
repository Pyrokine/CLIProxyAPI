package synthesizer

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestConfigSynthesizerProjectsRequestScopedErrorRules(t *testing.T) {
	rule := []config.RequestScopedErrorRule{{
		Status:      400,
		Match:       []string{"limit"},
		MatchRegexr: []string{"^limit$"},
		Action:      "stop",
	}}
	cfg := &config.Config{
		GeminiKey:       []config.GeminiKey{{APIKey: "gemini", RequestScopedErrors: rule}},
		InteractionsKey: []config.GeminiKey{{APIKey: "interactions", RequestScopedErrors: rule}},
		ClaudeKey:       []config.ClaudeKey{{APIKey: "claude", RequestScopedErrors: rule}},
		CodexKey:        []config.CodexKey{{APIKey: "codex", RequestScopedErrors: rule}},
		XAIKey:          []config.CodexKey{{APIKey: "xai", RequestScopedErrors: rule}},
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "compat-with-key", BaseURL: "https://with-key.example/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "compat"}}, RequestScopedErrors: rule},
			{Name: "compat-without-key", BaseURL: "https://without-key.example/v1", RequestScopedErrors: rule},
		},
		VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "vertex", BaseURL: "https://vertex.example/v1"}},
	}

	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config: cfg, Now: time.Unix(100, 0), IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 8 {
		t.Fatalf("auth count = %d, want 8", len(auths))
	}

	withRules := 0
	for _, auth := range auths {
		raw, present := auth.Metadata["request_scoped_errors"]
		if auth.Provider == "vertex" {
			if present {
				t.Fatalf("vertex metadata contains API-key request rules: %#v", auth.Metadata)
			}
			continue
		}
		projected, ok := raw.([]config.RequestScopedErrorRule)
		if !present || !ok || len(projected) != 1 || projected[0].Action != "stop" {
			t.Fatalf("%s metadata rules = %#v", auth.Provider, raw)
		}
		withRules++
	}
	if withRules != 7 {
		t.Fatalf("auths with rules = %d, want 7", withRules)
	}

	cfg.GeminiKey[0].RequestScopedErrors[0].Match[0] = "mutated"
	for _, auth := range auths {
		if auth.Provider != "gemini" {
			continue
		}
		projected := auth.Metadata["request_scoped_errors"].([]config.RequestScopedErrorRule)
		if projected[0].Match[0] != "limit" {
			t.Fatalf("synthesized metadata aliases config rules: %#v", projected)
		}
	}
}

func TestRequestScopedErrorRulesDoNotChangeStableAuthIdentity(t *testing.T) {
	synthesize := func(action string) map[string]string {
		t.Helper()
		rules := []config.RequestScopedErrorRule{{Status: 400, Match: []string{"limit"}, Action: action}}
		cfg := &config.Config{
			GeminiKey:       []config.GeminiKey{{APIKey: "gemini", RequestScopedErrors: rules}},
			InteractionsKey: []config.GeminiKey{{APIKey: "interactions", RequestScopedErrors: rules}},
			ClaudeKey:       []config.ClaudeKey{{APIKey: "claude", RequestScopedErrors: rules}},
			CodexKey:        []config.CodexKey{{APIKey: "codex", RequestScopedErrors: rules}},
			XAIKey:          []config.CodexKey{{APIKey: "xai", RequestScopedErrors: rules}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name: "compat", BaseURL: "https://compat.example/v1", RequestScopedErrors: rules,
			}},
		}
		auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
			Config: cfg, Now: time.Unix(100, 0), IDGenerator: NewStableIDGenerator(),
		})
		if err != nil {
			t.Fatalf("Synthesize() error = %v", err)
		}
		ids := make(map[string]string, len(auths))
		for _, auth := range auths {
			ids[auth.Provider] = auth.ID
		}
		return ids
	}

	before := synthesize("stop")
	after := synthesize("continue-and-cooldown")
	if len(before) != len(after) {
		t.Fatalf("identity sets = %#v / %#v", before, after)
	}
	for provider, id := range before {
		if after[provider] != id {
			t.Fatalf("%s ID changed with request rule policy: %s -> %s", provider, id, after[provider])
		}
	}
}
