package thinking

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestValidateClaudeAlwaysOnThinkingUsesLowestEffort(t *testing.T) {
	model := &registry.ModelInfo{
		ID:   "claude-opus-5-5",
		Type: "claude",
		Thinking: &registry.ThinkingSupport{
			DynamicAllowed: true,
			Levels:         []string{"low", "medium", "high", "xhigh", "max"},
		},
	}

	config, err := validateConfig(Config{Mode: ModeNone}, model, "claude", "claude", false)
	if err != nil {
		t.Fatalf("validateConfig() error = %v", err)
	}
	if config.Mode != ModeLevel || config.Level != levelLow || config.Budget != 0 {
		t.Fatalf("validated config = %+v, want low adaptive thinking", config)
	}
}

func TestIsSameProviderFamily(t *testing.T) {
	for _, testCase := range []struct {
		name string
		from string
		to   string
		want bool
	}{
		{name: "same provider", from: "claude", to: "claude", want: true},
		{name: "Gemini aliases", from: "gemini-cli", to: "antigravity", want: true},
		{name: "OpenAI chat to Responses", from: "openai", to: "openai-response", want: true},
		{name: "Responses to Codex", from: "openai-response", to: "codex", want: true},
		{name: "Codex to OpenAI chat", from: "codex", to: "openai", want: true},
		{name: "different families", from: "openai", to: "gemini", want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isSameProviderFamily(testCase.from, testCase.to); got != testCase.want {
				t.Fatalf("isSameProviderFamily(%q, %q) = %t, want %t", testCase.from, testCase.to, got, testCase.want)
			}
		})
	}
}
