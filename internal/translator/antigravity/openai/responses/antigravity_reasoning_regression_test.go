package responses

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRegisteredAntigravityClaudeReasoningPreservesSignatureOrder(t *testing.T) {
	first := base64.StdEncoding.EncodeToString([]byte{0x12, 0x04, 0x0a, 0x02, 0x08, 0x0c})
	second := base64.StdEncoding.EncodeToString([]byte{0x12, 0x04, 0x0a, 0x02, 0x08, 0x0c, 0x18, 0x01})
	firstCanonical, firstOK := sigcompat.CompatibleAntigravityClaudeThinkingSignature(first)
	secondCanonical, secondOK := sigcompat.CompatibleAntigravityClaudeThinkingSignature(second)
	if !firstOK || !secondOK {
		t.Fatal("synthetic signatures are not compatible with Antigravity Claude")
	}
	reasoning := func(text, signature string) string {
		return `{"type":"reasoning","summary":[{"type":"summary_text","text":"` + text + `"}],"encrypted_content":"` + signature + `"}`
	}
	for _, tc := range []struct {
		name  string
		items []string
		want  []string
	}{
		{name: "repeated text", items: []string{reasoning("same", first), reasoning("same", second)}, want: []string{firstCanonical, secondCanonical}},
		{name: "empty reasoning", items: []string{reasoning("", first), reasoning("same", second)}, want: []string{secondCanonical}},
		{name: "invalid before valid", items: []string{reasoning("same", "gpt#invalid"), reasoning("same", second)}, want: []string{secondCanonical}},
		{name: "function binding", items: []string{reasoning("same", first), `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}`, `{"type":"function_call_output","call_id":"call-1","output":"done"}`}, want: []string{firstCanonical}},
		{name: "detached text signature", items: []string{reasoning("same", ""), `{"type":"reasoning","id":"reason_detached_after_0","summary":[],"encrypted_content":"` + second + `"}`}, want: []string{secondCanonical}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := []byte(`{"input":[` + strings.Join(tc.items, ",") + `,{"type":"message","role":"user","content":"continue"}]}`)
			body, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatAntigravity, "claude-sonnet-4", request, false)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, content := range gjson.GetBytes(body, "request.contents").Array() {
				for _, part := range content.Get("parts").Array() {
					if part.Get("thought").Bool() {
						if part.Get("text").String() != "same" {
							t.Fatalf("reasoning text changed: %s", body)
						}
						got = append(got, part.Get("thoughtSignature").String())
					}
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("reasoning signatures = %q, want %q: %s", got, tc.want, body)
			}
		})
	}
}
