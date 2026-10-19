package common

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiToolFromInteractionsPreservesConfigurations(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"url default", `{"type":"url_context"}`, `{"urlContext":{}}`},
		{"url snake", `{"type":"url_context","url_context":{"allowed_urls":["https://example.test"]}}`, `{"urlContext":{"allowed_urls":["https://example.test"]}}`},
		{"url native", `{"type":"url_context","urlContext":{"limit":3}}`, `{"urlContext":{"limit":3}}`},
		{"code", `{"type":"code_execution","code_execution":{"timeout":9007199254740993}}`, `{"codeExecution":{"timeout":9007199254740993}}`},
		{"search", `{"type":"google_search","google_search":{"exclude_domains":["example.test"]}}`, `{"googleSearch":{"exclude_domains":["example.test"]}}`},
		{"web search own config", `{"type":"web_search","web_search":{"limit":2}}`, `{"googleSearch":{"limit":2}}`},
		{"web search alias config", `{"type":"web_search","google_search":{"limit":2}}`, `{"googleSearch":{"limit":2}}`},
		{"web search native config", `{"type":"web_search","googleSearch":{"limit":2}}`, `{"googleSearch":{"limit":2}}`},
		{"invalid config uses native", `{"type":"url_context","url_context":null,"urlContext":{"limit":2}}`, `{"urlContext":{"limit":2}}`},
		{"mixed native", `{"functionDeclarations":[{"name":"lookup"}],"url_context":{},"code_execution":{},"futureTool":{"version":9007199254740993}}`, `{"functionDeclarations":[{"name":"lookup"}],"urlContext":{},"codeExecution":{},"futureTool":{"version":9007199254740993}}`},
		{"native wins", `{"urlContext":{"limit":1},"url_context":{"limit":2}}`, `{"urlContext":{"limit":1}}`},
		{"search aliases", `{"google_search":{"limit":1},"web_search":{"limit":2}}`, `{"googleSearch":{"limit":1}}`},
		{"unknown type", `{"type":"future","options":{"value":9007199254740993}}`, `{"type":"future","options":{"value":9007199254740993}}`},
		{"unknown native", `{"futureTool":{"value":1e999}}`, `{"futureTool":{"value":1e999}}`},
		{"empty", `{}`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := gjson.Parse(tc.input)
			out, err := json.Marshal(GeminiToolFromInteractions(input))
			if err != nil {
				t.Fatal(err)
			}
			decode := func(raw string) any {
				t.Helper()
				decoder := json.NewDecoder(strings.NewReader(raw))
				decoder.UseNumber()
				var value any
				if err := decoder.Decode(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			if !reflect.DeepEqual(decode(string(out)), decode(tc.want)) {
				t.Fatalf("tool = %s, want %s", out, tc.want)
			}
			if input.Raw != tc.input {
				t.Fatal("input was modified")
			}
		})
	}
	for _, input := range []string{`null`, `[]`, `"url_context"`, `1`} {
		if out := GeminiToolFromInteractions(gjson.Parse(input)); out != nil {
			t.Fatalf("non-object %s produced %v", input, out)
		}
	}
}
