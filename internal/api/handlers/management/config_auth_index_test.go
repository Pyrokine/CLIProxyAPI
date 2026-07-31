package management

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBuildConfigAuthIndexViewsDoesNotShiftAfterInteractionsKeys(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "gemini-key"}},
		InteractionsKey: []config.GeminiKey{{
			APIKey:  "interactions-key",
			BaseURL: "https://interactions.example.com",
		}},
		ClaudeKey: []config.ClaudeKey{{
			APIKey:  "claude-key",
			BaseURL: "https://claude.example.com",
		}},
		CodexKey: []config.CodexKey{{
			APIKey:  "codex-key",
			BaseURL: "https://codex.example.com",
		}},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "interactions",
				BaseURL:       "https://compat-interactions.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "compat-interactions-key"}},
			},
			{
				Name:          "custom",
				BaseURL:       "https://custom.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "custom-key"}},
			},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Unix(1_700_000_000, 0),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("synthesize auths: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err = manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s auth: %v", auth.Provider, err)
		}
	}

	expected := make(map[string]string)
	for _, auth := range manager.List() {
		auth.EnsureIndex()
		expected[auth.Provider] = auth.Index
	}
	views := (&Handler{authManager: manager}).buildConfigAuthIndexViews(cfg)
	if got := views.gemini[0]; got != expected["gemini"] {
		t.Fatalf("gemini auth index = %q, want %q", got, expected["gemini"])
	}
	if got := views.claude[0]; got != expected["claude"] {
		t.Fatalf("claude auth index = %q, want %q", got, expected["claude"])
	}
	if got := views.codex[0]; got != expected["codex"] {
		t.Fatalf("codex auth index = %q, want %q", got, expected["codex"])
	}
	if got := views.openAIEntries[0][0]; got != expected["interactions"] {
		t.Fatalf("Interactions-named compatibility auth index = %q, want %q", got, expected["interactions"])
	}
	if got := views.openAIEntries[1][0]; got != expected["custom"] {
		t.Fatalf("custom compatibility auth index = %q, want %q", got, expected["custom"])
	}
	if views.claude[0] == expected["gemini-interactions"] {
		t.Fatal("claude config received the Interactions auth index")
	}
}
