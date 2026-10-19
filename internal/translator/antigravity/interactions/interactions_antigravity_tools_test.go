package interactions

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsAntigravityRegisteredToolsMapping(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
		count       int64
		fields      map[string]string
	}{
		{
			name:   "typed builtins and deduplicated functions",
			tools:  `[{"type":"url_context","url_context":{"limit":2}},{"type":"code_execution","codeExecution":{"timeout":3}},{"type":"google_search"},{"type":"web_search","web_search":{"limit":4}},{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"lookup","parameters":{"type":"object"}}]`,
			count:  5,
			fields: map[string]string{"0.functionDeclarations.#": "1", "0.functionDeclarations.0.name": `"lookup"`, "1.urlContext.limit": "2", "2.codeExecution.timeout": "3", "3.googleSearch": "{}", "4.googleSearch.limit": "4"},
		},
		{
			name:   "native functions with neighboring builtin and later tools",
			tools:  `[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}],"url_context":{"limit":2}},{"type":"code_execution"},{"futureTool":{"version":9007199254740993}},{"type":"url_context"}]`,
			count:  5,
			fields: map[string]string{"0.functionDeclarations.0.name": `"lookup"`, "1.urlContext.limit": "2", "2.codeExecution": "{}", "3.futureTool.version": "9007199254740993", "4.urlContext": "{}"},
		},
		{
			name:   "snake functions preserve neighboring builtin",
			tools:  `[{"function_declarations":[{"name":"lookup"}],"google_search":{"limit":2}},{"url_context":{},"code_execution":{}}]`,
			count:  3,
			fields: map[string]string{"0.functionDeclarations.0.name": `"lookup"`, "1.googleSearch.limit": "2", "2.urlContext": "{}", "2.codeExecution": "{}"},
		},
		{
			name:   "unknown and native configurations",
			tools:  `[{"futureTool":{"value":1e999}},{"urlContext":{"limit":9007199254740993}},{"type":"future","options":{"version":9007199254740993}}]`,
			count:  3,
			fields: map[string]string{"0.futureTool.value": "1e999", "1.urlContext.limit": "9007199254740993", "2.type": `"future"`, "2.options.version": "9007199254740993"},
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				input := []byte(`{"input":"hello","tools":` + tc.tools + `}`)
				original := bytes.Clone(input)
				out, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatAntigravity, "gemini-fixture", input, stream)
				if err != nil {
					t.Fatal(err)
				}
				if !gjson.ValidBytes(out) || gjson.GetBytes(out, "request.tools.#").Int() != tc.count {
					t.Fatalf("invalid tools: %s", out)
				}
				for path, want := range tc.fields {
					got := gjson.GetBytes(out, "request.tools."+path)
					actual := got.Raw
					if actual == "" && got.Type == gjson.Number {
						actual = fmt.Sprint(got.Int())
					}
					if actual != want {
						t.Fatalf("tools.%s = %s, want %s; output=%s", path, actual, want, out)
					}
				}
				for _, tool := range gjson.GetBytes(out, "request.tools").Array() {
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
	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		input := []byte(`{"input":"hello","tools":[{"type":"url_context"},{"type":"code_execution"},{"type":"function","name":"lookup"}],"tool_choice":` + choice + `}`)
		out, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatAntigravity, "gemini-fixture", input, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "request.tools").Exists() || gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String() != "NONE" {
			t.Fatalf("NONE retained tools: %s", out)
		}
	}
}
