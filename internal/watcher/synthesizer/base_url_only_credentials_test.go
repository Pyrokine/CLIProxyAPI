package synthesizer

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestConfigSynthesizerKeepsBaseURLOnlyCredentials(t *testing.T) {
	cfg := &config.Config{
		GeminiKey:       []config.GeminiKey{{BaseURL: "https://gemini.example"}},
		InteractionsKey: []config.GeminiKey{{BaseURL: "https://interactions.example"}},
		ClaudeKey:       []config.ClaudeKey{{BaseURL: "https://claude.example"}},
		CodexKey:        []config.CodexKey{{BaseURL: "https://codex.example"}},
		XAIKey:          []config.XAIKey{{BaseURL: "https://xai.example"}},
	}
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Unix(1, 0).UTC(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}

	want := map[string]string{
		"gemini":              "https://gemini.example",
		"gemini-interactions": "https://interactions.example",
		"claude":              "https://claude.example",
		"codex":               "https://codex.example",
		"xai":                 "https://xai.example",
	}
	for _, auth := range auths {
		baseURL, expected := want[auth.Provider]
		if !expected {
			continue
		}
		if got := auth.Attributes["base_url"]; got != baseURL {
			t.Fatalf("provider %s base_url = %q, want %q", auth.Provider, got, baseURL)
		}
		if _, exists := auth.Attributes["api_key"]; exists {
			t.Fatalf("provider %s synthesized an empty api_key attribute", auth.Provider)
		}
		delete(want, auth.Provider)
	}
	if len(want) != 0 {
		t.Fatalf("missing base-url-only providers: %v", want)
	}
}
