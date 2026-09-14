package test

import (
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/antigravity"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestSummaryIntentTranslation(t *testing.T) {
	tests := []struct {
		name       string
		from       sdktranslator.Format
		to         sdktranslator.Format
		body       string
		path       string
		want       string
		wantExists bool
	}{
		{
			name:       "Chat effort enables Gemini summary",
			from:       sdktranslator.FormatOpenAI,
			to:         sdktranslator.FormatGemini,
			body:       `{"model":"gemini-mixed-model","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			path:       "generationConfig.thinkingConfig.includeThoughts",
			want:       "true",
			wantExists: true,
		},
		{
			name: "Responses effort leaves Gemini summary absent",
			from: sdktranslator.FormatOpenAIResponse,
			to:   sdktranslator.FormatGemini,
			body: `{"model":"gemini-mixed-model","reasoning":{"effort":"high"},"input":"hi"}`,
			path: "generationConfig.thinkingConfig.includeThoughts",
		},
		{
			name:       "Responses summary enables Gemini summary",
			from:       sdktranslator.FormatOpenAIResponse,
			to:         sdktranslator.FormatGemini,
			body:       `{"model":"gemini-mixed-model","reasoning":{"effort":"high","summary":"detailed"},"input":"hi"}`,
			path:       "generationConfig.thinkingConfig.includeThoughts",
			want:       "true",
			wantExists: true,
		},
		{
			name:       "Claude omitted display disables Gemini summary",
			from:       sdktranslator.FormatClaude,
			to:         sdktranslator.FormatGemini,
			body:       `{"model":"gemini-mixed-model","thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`,
			path:       "generationConfig.thinkingConfig.includeThoughts",
			want:       "false",
			wantExists: true,
		},
		{
			name: "Interactions effort leaves Antigravity summary absent",
			from: sdktranslator.FormatInteractions,
			to:   sdktranslator.FormatAntigravity,
			body: `{"model":"antigravity-budget-model","reasoning":{"effort":"high"},"input":"hi"}`,
			path: "request.generationConfig.thinkingConfig.includeThoughts",
		},
		{
			name:       "Interactions summary enables Antigravity summary",
			from:       sdktranslator.FormatInteractions,
			to:         sdktranslator.FormatAntigravity,
			body:       `{"model":"antigravity-budget-model","reasoning":{"effort":"high","summary":"auto"},"input":"hi"}`,
			path:       "request.generationConfig.thinkingConfig.includeThoughts",
			want:       "true",
			wantExists: true,
		},
		{
			name: "Gemini effort leaves Codex summary absent",
			from: sdktranslator.FormatGemini,
			to:   sdktranslator.FormatCodex,
			body: `{"model":"level-model","generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			path: "reasoning.summary",
		},
		{
			name:       "Gemini summary enables Codex summary",
			from:       sdktranslator.FormatGemini,
			to:         sdktranslator.FormatCodex,
			body:       `{"model":"level-model","generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			path:       "reasoning.summary",
			want:       "auto",
			wantExists: true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				out := sdktranslator.TranslateRequest(test.from, test.to, "", []byte(test.body), true)
				result := gjson.GetBytes(out, test.path)
				if result.Exists() != test.wantExists {
					t.Fatalf("%s exists = %v, want %v; body=%s", test.path, result.Exists(), test.wantExists, out)
				}
				if test.wantExists && result.String() != test.want {
					t.Fatalf("%s = %q, want %q; body=%s", test.path, result.String(), test.want, out)
				}
			},
		)
	}
}

func TestInvalidInteractionsSummaryDoesNotWriteTargetControl(t *testing.T) {
	body := []byte(`{"model":"model","generation_config":{"thinking_summaries":"banana"},"input":"hi"}`)
	for _, test := range []struct {
		name string
		to   sdktranslator.Format
		path string
	}{
		{name: "Gemini", to: sdktranslator.FormatGemini, path: "generationConfig.thinkingConfig.includeThoughts"},
		{
			name: "Antigravity", to: sdktranslator.FormatAntigravity,
			path: "request.generationConfig.thinkingConfig.includeThoughts",
		},
		{name: "Codex", to: sdktranslator.FormatCodex, path: "reasoning.summary"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				out := sdktranslator.TranslateRequest(sdktranslator.FormatInteractions, test.to, "model", body, false)
				if result := gjson.GetBytes(out, test.path); result.Exists() {
					t.Fatalf("invalid Interactions summary wrote %s=%s; body=%s", test.path, result.Raw, out)
				}
			},
		)
	}
}
