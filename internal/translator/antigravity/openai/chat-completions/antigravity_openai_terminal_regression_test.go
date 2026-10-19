package chat_completions

import (
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRegisteredAntigravityTerminalPreservesUsageAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		wantUsage   bool
		wantCached  bool
	}{
		{name: "text without usage"},
		{name: "ordinary usage", usage: `"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}`, wantUsage: true},
		{name: "filtered usage", usage: `"cpaUsageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}`, wantUsage: true},
		{name: "explicit zero cache", usage: `"cpaUsageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5,"cachedContentTokenCount":0}`, wantUsage: true, wantCached: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			translate := func(raw string) [][]byte {
				return sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatAntigravity, sdktranslator.FormatOpenAI,
					"synthetic-model", nil, nil, []byte(raw), &state)
			}
			translate(`{"response":{"responseId":"response-1","modelVersion":"synthetic-model","candidates":[{"content":{"parts":[{"text":"answer"}]}}]}}`)
			if tc.usage != "" {
				translate(`{"response":{` + tc.usage + `}}`)
			}
			terminal := translate("data: [DONE]")
			if len(terminal) != 1 {
				t.Fatalf("terminal count = %d, want 1: %q", len(terminal), terminal)
			}
			root := gjson.ParseBytes(terminal[0])
			if root.Get("id").String() != "response-1" || root.Get("model").String() != "synthetic-model" ||
				root.Get("choices.0.finish_reason").String() != "stop" {
				t.Fatalf("terminal identity or reason changed: %s", terminal[0])
			}
			if content := root.Get("choices.0.delta.content"); content.Type != gjson.Null {
				t.Fatalf("terminal repeated content: %s", terminal[0])
			}
			if root.Get("usage").Exists() != tc.wantUsage || (tc.wantUsage && root.Get("usage.total_tokens").Int() != 5) {
				t.Fatalf("terminal usage changed: %s", terminal[0])
			}
			if cached := root.Get("usage.prompt_tokens_details.cached_tokens"); cached.Exists() != tc.wantCached || cached.Int() != 0 {
				t.Fatalf("explicit zero cache presence changed: %s", terminal[0])
			}
			if duplicate := translate("[DONE]"); len(duplicate) != 0 {
				t.Fatalf("duplicate terminal: %q", duplicate)
			}
		})
	}
	var empty any
	if chunks := sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatAntigravity, sdktranslator.FormatOpenAI,
		"synthetic-model", nil, nil, []byte("[DONE]"), &empty); len(chunks) != 0 {
		t.Fatalf("empty stream produced success: %q", chunks)
	}
}
