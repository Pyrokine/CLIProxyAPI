package interactions

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsGeminiRegisteredToolsMapping(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
		count       int64
		fields      map[string]string
	}{
		{
			name:   "typed builtins and function",
			tools:  `[{"type":"url_context","url_context":{"limit":2}},{"type":"code_execution","codeExecution":{"timeout":3}},{"type":"google_search"},{"type":"web_search","web_search":{"limit":4}},{"type":"function","name":"lookup","parameters":{"type":"object"}}]`,
			count:  5,
			fields: map[string]string{"0.urlContext.limit": "2", "1.codeExecution.timeout": "3", "2.googleSearch": "{}", "3.googleSearch.limit": "4", "4.functionDeclarations.0.name": `"lookup"`},
		},
		{
			name:   "native functions do not stop later tools",
			tools:  `[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}],"url_context":{"limit":2}},{"type":"code_execution"},{"futureTool":{"version":9007199254740993}},{"type":"url_context"}]`,
			count:  4,
			fields: map[string]string{"0.functionDeclarations.0.name": `"lookup"`, "0.urlContext.limit": "2", "1.codeExecution": "{}", "2.futureTool.version": "9007199254740993", "3.urlContext": "{}"},
		},
		{
			name:   "snake functions preserve neighboring builtin",
			tools:  `[{"function_declarations":[{"name":"lookup"}],"google_search":{"limit":2}},{"url_context":{},"code_execution":{}}]`,
			count:  2,
			fields: map[string]string{"0.functionDeclarations.0.name": `"lookup"`, "0.googleSearch.limit": "2", "1.urlContext": "{}", "1.codeExecution": "{}"},
		},
		{
			name:   "unknown fields and numbers are unchanged",
			tools:  `[{"futureTool":{"value":1e999}},{"type":"future","options":{"version":9007199254740993}}]`,
			count:  2,
			fields: map[string]string{"0.futureTool.value": "1e999", "1.type": `"future"`, "1.options.version": "9007199254740993"},
		},
		{name: "empty", tools: `[]`, count: 0},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				input := []byte(`{"model":"fixture","input":"hello","tools":` + tc.tools + `}`)
				original := bytes.Clone(input)
				out, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatGemini, "gemini-fixture", input, stream)
				if err != nil {
					t.Fatal(err)
				}
				if !gjson.ValidBytes(out) || gjson.GetBytes(out, "tools.#").Int() != tc.count {
					t.Fatalf("invalid tools: %s", out)
				}
				for path, want := range tc.fields {
					if got := gjson.GetBytes(out, "tools."+path).Raw; got != want {
						t.Fatalf("tools.%s = %s, want %s; output=%s", path, got, want, out)
					}
				}
				for _, tool := range gjson.GetBytes(out, "tools").Array() {
					for _, alias := range []string{"url_context", "code_execution", "google_search", "web_search", "function_declarations"} {
						if tool.Get(alias).Exists() {
							t.Fatalf("unconverted alias %s: %s", alias, out)
						}
					}
				}
				if !bytes.Equal(input, original) {
					t.Fatal("input was modified")
				}
			})
		}
	}
}

func TestGeminiInteractionsRegisteredToolsRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		input := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}],"urlContext":{"limit":2},"codeExecution":{"timeout":9007199254740993}},{"googleSearch":{"exclude_domains":["example.test"]}},{"futureTool":{"version":9007199254740993}},{"web_search":{}}]}`)
		interaction, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatInteractions, "gemini-fixture", input, stream)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(interaction, "tools.#").Int() != 6 {
			t.Fatalf("Interactions tools = %s", interaction)
		}
		for path, want := range map[string]string{"0.type": `"url_context"`, "0.url_context.limit": "2", "1.type": `"code_execution"`, "1.code_execution.timeout": "9007199254740993", "2.type": `"function"`, "2.name": `"lookup"`, "3.type": `"google_search"`, "3.google_search.exclude_domains.0": `"example.test"`, "4.futureTool.version": "9007199254740993", "5.type": `"google_search"`} {
			if got := gjson.GetBytes(interaction, "tools."+path).Raw; got != want {
				t.Fatalf("Interactions tools.%s = %s, want %s; output=%s", path, got, want, interaction)
			}
		}
		gemini, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatGemini, "gemini-fixture", interaction, stream)
		if err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string]string{"0.urlContext.limit": "2", "1.codeExecution.timeout": "9007199254740993", "2.functionDeclarations.0.name": `"lookup"`, "3.googleSearch.exclude_domains.0": `"example.test"`, "4.futureTool.version": "9007199254740993", "5.googleSearch": "{}"} {
			if got := gjson.GetBytes(gemini, "tools."+path).Raw; got != want {
				t.Fatalf("Gemini tools.%s = %s, want %s; output=%s", path, got, want, gemini)
			}
		}
	}
}
