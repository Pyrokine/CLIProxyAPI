package gemini_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/gemini"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravitySDKToolEnumTypes(t *testing.T) {
	request := []byte((`{"messages":[{"role":"user","content":"query"}],"tools":[{"name":"lookup","` +
		`input_schema":{"type":"object","properties":{"count":{"type":"integer","enum":[1,2]},"enabled":{"` +
		`type":"boolean","enum":[true,false]}},"required":["count","enabled"]}}]}`))
	out, err := sdktranslator.TranslateRequestChecked(
		context.Background(),
		sdktranslator.FormatClaude,
		sdktranslator.FormatAntigravity,
		"fixture",
		request,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	schema := gjson.GetBytes(out, "request.tools.0.functionDeclarations.0.parametersJsonSchema")
	for name, want := range map[string]string{"count": "integer", "enabled": "boolean"} {
		property := schema.Get("properties." + name)
		original := gjson.GetBytes(request, "tools.0.input_schema.properties."+name)
		if property.Get("type").String() != want || property.Get("enum").Raw != original.Get("enum").Raw {
			t.Fatalf("native tool type or enum lost: %s", out)
		}
	}
	if schema.Get("required.#").Int() != 2 {
		t.Fatalf("required fields lost: %s", out)
	}
}

func TestAntigravitySDKResponseSchemaSemantics(t *testing.T) {
	schema := `{"type":"object","$defs":{"Payload":{"type":"object","properties":{"id":{"type":"integer"}},"` +
		`required":["id"]}},"properties":{"payload":{"$ref":"#/$defs/Payload"},"value":{"type":["number","` +
		`null"]},"action":{"type":"object","properties":{"full":{"type":"string"}},"required":["full"]}},"` +
		`required":["payload","value","action"],"allOf":[{"if":{"properties":{"kind":{"const":"x"}}},"then":{` +
		`"properties":{"action":{"type":"null"},"branch_only":{"type":"string"}}}}]}`
	request := []byte(
		`{"contents":[{"role":"user","parts":[{"text":"query"}]}],` +
			`"generationConfig":{"responseSchema":` +
			schema +
			`}}`,
	)
	out, err := sdktranslator.TranslateRequestChecked(
		context.Background(),
		sdktranslator.FormatGemini,
		sdktranslator.FormatAntigravity,
		"fixture",
		request,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	result := gjson.GetBytes(out, "request.generationConfig.responseSchema")
	for path, want := range map[string]string{
		"properties.payload.properties.id.type": "integer", "properties.payload.required.0": "id",
		"properties.value.type": "number", "properties.action.properties.full.type": "string",
		"properties.action.required.0": "full", "properties.branch_only.type": "string", "required.1": "value",
	} {
		if got := result.Get(path).String(); got != want {
			t.Fatalf("%s=%q want %q; %s", path, got, want, out)
		}
	}
	if !result.Get("properties.value.nullable").Bool() || result.Get("$defs").Exists() {
		t.Fatalf("nullable or reference cleanup incorrect: %s", out)
	}
}

func TestAntigravitySDKNativeSearchRejectsDroppedAttachmentWithWhitespace(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient(
		"protocol-native-search-fixture", "antigravity",
		[]*registry.ModelInfo{{ID: "gemini-3.1-flash-lite", SupportsWebSearch: true}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("protocol-native-search-fixture") })
	for _, text := range []string{"", "   ", "\n\t", "current query"} {
		body := []byte(fmt.Sprintf(
			`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"message`+
				`s":[{"role":"user","content":"earlier query"},{"role":"assistant","con`+
				`tent":"answer"},{"role":"user","content":[{"type":"text","text":%q},{"`+
				`type":"image","source":{"type":"url","url":"https://example.invalid/im`+
				`age.png"}}]}]}`,
			text,
		))
		out, err := sdktranslator.TranslateRequestChecked(
			context.Background(),
			sdktranslator.FormatClaude,
			sdktranslator.FormatAntigravity,
			"gemini-3.1-flash-lite",
			body,
			false,
		)
		if strings.TrimSpace(text) == "" {
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(
				err, &unsupported,
			) || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || len(out) != 0 {
				t.Fatalf("whitespace accepted after losing attachment: err=%v body=%s", err, out)
			}
		} else if err != nil || gjson.GetBytes(out, "requestType").String() != "web_search" {
			t.Fatalf("native search text control rejected: err=%v body=%s", err, out)
		}
	}
}
