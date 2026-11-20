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

func TestValidateConfigCrossFamilyKnownLevels(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, level, want string
		levels                      []string
	}{
		{name: "OpenAI to Gemini xhigh", from: "openai", to: "gemini", level: "xhigh", want: "high"},
		{name: "Responses to Gemini xhigh", from: "openai-response", to: "gemini", level: "xhigh", want: "high"},
		{name: "Gemini to OpenAI xhigh", from: "gemini", to: "openai", level: "xhigh", want: "high"},
		{name: "minimal uses lowest supported", from: "openai", to: "gemini", level: "minimal", want: "low"},
		{name: "medium tie uses lower", from: "openai", to: "gemini", level: "medium", want: "low"},
		{
			name: "Responses to xAI minimal", from: "openai-response", to: "xai", level: "minimal", want: "low",
			levels: []string{"none", "low", "medium", "high"},
		},
		{name: "OpenAI to xAI xhigh", from: "openai", to: "xai", level: "xhigh", want: "high"},
		{name: "Responses to xAI max", from: "openai-response", to: "xai", level: "max", want: "high"},
		{name: "native xAI stays strict", from: "xai", to: "xai", level: "minimal"},
		{name: "native Gemini stays strict", from: "gemini", to: "gemini", level: "xhigh"},
		{name: "native OpenAI stays strict", from: "openai", to: "openai", level: "xhigh"},
		{name: "unknown cross-family level", from: "openai", to: "gemini", level: "ultra"},
		{
			name: "no representable target level", from: "openai", to: "gemini", level: "xhigh",
			levels: []string{"custom"},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				levels := tc.levels
				if levels == nil {
					levels = []string{"low", "high"}
				}
				model := &registry.ModelInfo{
					ID: "cross-family-levels", Type: tc.to,
					Thinking: &registry.ThinkingSupport{Levels: levels},
				}
				got, err := validateConfig(
					Config{Mode: ModeLevel, Level: Level(tc.level)}, model, tc.from, tc.to, false,
				)
				if tc.want == "" {
					if err == nil {
						t.Fatalf("unsupported level accepted: %+v", got)
					}
					return
				}
				if err != nil || got == nil || got.Mode != ModeLevel || string(got.Level) != tc.want {
					t.Fatalf("validated=%+v error=%v, want level %q", got, err, tc.want)
				}
			},
		)
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
		{name: "xAI stays distinct from OpenAI chat", from: "openai", to: "xai", want: false},
		{name: "xAI stays distinct from Responses", from: "openai-response", to: "xai", want: false},
		{name: "xAI stays distinct from Codex", from: "xai", to: "codex", want: false},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				if got := isSameProviderFamily(testCase.from, testCase.to); got != testCase.want {
					t.Fatalf(
						"isSameProviderFamily(%q, %q) = %t, want %t", testCase.from, testCase.to, got, testCase.want,
					)
				}
			},
		)
	}
}
