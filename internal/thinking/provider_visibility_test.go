package thinking_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/antigravity"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/gemini"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/openai"
	"github.com/tidwall/gjson"
)

func TestApplyThinkingWithModelInfoAndSummaryUsesProviderVisibility(t *testing.T) {
	modelInfo := &registry.ModelInfo{
		ID:       "private-openai",
		Type:     "openai-compatibility",
		Thinking: &registry.ThinkingSupport{Levels: []string{"high", "max"}},
	}
	sourceBody := []byte(`{"model":"private-openai","reasoning":{"summary":"auto"},"input":"hi"}`)

	tests := []struct {
		name        string
		provider    string
		wantExclude bool
	}{
		{name: "OpenAI has no visibility field", provider: "openai"},
		{name: "OpenRouter uses reasoning exclude", provider: "prod-openrouter", wantExclude: true},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
					[]byte(`{"model":"private-openai","messages":[{"role":"user","content":"hi"}]}`),
					sourceBody,
					"private-openai",
					"openai-response",
					"openai",
					test.provider,
					modelInfo,
					thinking.SummaryConfig{Mode: thinking.SummaryEnabled, Detail: "auto"},
				)
				if err != nil {
					t.Fatalf("ApplyThinkingWithModelInfoAndSummary() error = %v; body=%s", err, out)
				}
				if gjson.GetBytes(out, "reasoning_effort").Exists() {
					t.Fatalf("summary visibility invented reasoning_effort: %s", out)
				}
				exclude := gjson.GetBytes(out, "reasoning.exclude")
				if exclude.Exists() != test.wantExclude {
					t.Fatalf("reasoning.exclude exists = %v, want %v; body=%s", exclude.Exists(), test.wantExclude, out)
				}
				if test.wantExclude && exclude.Bool() {
					t.Fatalf("OpenRouter summary visibility remained excluded: %s", out)
				}
			},
		)
	}
}

func TestApplyThinkingWithSummaryKeepsOpenAIChatSuffixNone(t *testing.T) {
	out, err := thinking.ApplyThinkingWithSummary(
		[]byte(`{"model":"private-openai","messages":[{"role":"user","content":"hi"}]}`),
		"private-openai(none)",
		"openai-response",
		"openai",
		"openai",
		thinking.SummaryConfig{Mode: thinking.SummaryEnabled, Detail: "auto"},
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithSummary() error = %v; body=%s", err, out)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "none" {
		t.Fatalf("reasoning_effort = %q, want none; body=%s", got, out)
	}
}

func TestApplyThinkingWithModelInfoAndSummaryDropsRemovedClaudeSummaryActivation(t *testing.T) {
	modelInfo := &registry.ModelInfo{
		ID:       "private-manual-claude",
		Type:     "claude",
		Thinking: &registry.ThinkingSupport{Min: 1024, Max: 16000},
	}
	out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
		[]byte(`{"model":"private-manual-claude","max_tokens":32000,"thinking":{"type":"adaptive"}}`),
		[]byte(`{"reasoning":{"summary":"auto"}}`),
		"private-manual-claude",
		"openai-response",
		"claude",
		"claude",
		modelInfo,
		thinking.SummaryConfig{},
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithModelInfoAndSummary() error = %v", err)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("removed summary retained globally inferred adaptive thinking: %s", out)
	}
}

func TestApplyThinkingWithModelInfoAndSummaryFullyDisabledGeminiStaysDisabled(t *testing.T) {
	modelInfo := &registry.ModelInfo{
		ID:   "private-gemini",
		Type: "gemini",
		Thinking: &registry.ThinkingSupport{
			Levels:      []string{"none", "low", "high"},
			ZeroAllowed: true,
		},
	}

	tests := []struct {
		name       string
		body       string
		toFormat   string
		provider   string
		configPath string
	}{
		{
			name:       "Gemini",
			body:       `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}`,
			toFormat:   "gemini",
			provider:   "gemini",
			configPath: "generationConfig.thinkingConfig",
		},
		{
			name:       "Antigravity",
			body:       `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}}`,
			toFormat:   "antigravity",
			provider:   "antigravity",
			configPath: "request.generationConfig.thinkingConfig",
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
					[]byte(test.body),
					[]byte(`{"reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`),
					"private-gemini",
					"openai",
					test.toFormat,
					test.provider,
					modelInfo,
					thinking.SummaryConfig{Mode: thinking.SummaryDisabled},
				)
				if err != nil {
					t.Fatalf("ApplyThinkingWithModelInfoAndSummary() error = %v; body=%s", err, out)
				}
				if gjson.GetBytes(out, test.configPath).Exists() {
					t.Fatalf("fully disabled thinking config was recreated: %s", out)
				}
			},
		)
	}
}
