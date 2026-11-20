package common_test

import (
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOriginalClaudeMessageSystemRolesUseRegisteredTranslators(t *testing.T) {
	input := []byte((`{"model":"gemini-3-flash-preview","system":[{"type":"text","text":"Top-level rules"}],"` +
		`messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]},{"role":"system","content":"` +
		`String mid-conversation rule"},{"role":"system","content":[{"type":"text","text":"Array mid-` +
		`conversation rule"}]}]}`))
	for _, sample := range []struct {
		format sdk.Format
		model  string
		prefix string
	}{
		{sdk.FormatAntigravity, "gemini-3-flash-agent", "request."},
		{sdk.FormatGeminiCLI, "gemini-3-flash-preview", "request."},
		{sdk.FormatGemini, "gemini-3-flash-preview", ""},
	} {
		t.Run(
			sample.format.String(), func(t *testing.T) {
				output, err := sdk.TranslateRequestChecked(
					sdk.WithPluginHooks(t.Context(), nil), sdk.FormatClaude, sample.format, sample.model, input, false,
				)
				if err != nil {
					t.Fatal(err)
				}
				instructions := gjson.GetBytes(output, sample.prefix+"systemInstruction.parts").Array()
				if len(instructions) != 1 || instructions[0].Get("text").String() != "Top-level rules" {
					t.Fatalf("top-level system instructions changed: %s", output)
				}
				var text strings.Builder
				for _, content := range gjson.GetBytes(output, sample.prefix+"contents").Array() {
					if content.Get("role").String() != "user" {
						t.Fatalf("message-level system role reached upstream contents: %s", output)
					}
					for _, part := range content.Get("parts").Array() {
						text.WriteString(part.Get("text").String())
						text.WriteByte('\n')
					}
				}
				remaining := text.String()
				for _, expected := range []string{
					"Hello", "String mid-conversation rule", "Array mid-conversation rule",
				} {
					_, tail, found := strings.Cut(remaining, expected)
					if !found {
						t.Fatalf("message text order lost %q: %s", expected, output)
					}
					remaining = tail
				}
			},
		)
	}
}
