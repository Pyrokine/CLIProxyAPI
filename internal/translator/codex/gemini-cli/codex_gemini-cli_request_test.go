package geminiCLI

import (
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestConvertGeminiCLIRequestToCodex_PreservesSchemaPropertyNamedType(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	input := []byte(("{\n\t\t\"request\": {\n\t\t\t\"tools\": [{\n\t\t\t\t\"functionDeclarations\": [{\n\t\t\t" +
		"\t\t\"name\": \"ask_user\",\n\t\t\t\t\t\"description\": \"Ask the user one or more questions.\",\n\t" +
		"\t\t\t\t\"parametersJsonSchema\": {\n\t\t\t\t\t\t\"type\": \"object\",\n\t\t\t\t\t\t\"properties\": " +
		"{\n\t\t\t\t\t\t\t\"questions\": {\n\t\t\t\t\t\t\t\t\"type\": \"array\",\n\t\t\t\t\t\t\t\t\"items\": " +
		"{\n\t\t\t\t\t\t\t\t\t\"type\": \"object\",\n\t\t\t\t\t\t\t\t\t\"properties\": {\n\t\t\t\t\t\t\t\t\t" +
		"\t\"header\": {\"type\": \"string\"},\n\t\t\t\t\t\t\t\t\t\t\"type\": {\n\t\t\t\t\t\t\t\t\t\t\t\"" +
		"default\": \"choice\",\n\t\t\t\t\t\t\t\t\t\t\t\"description\": \"Question type.\",\n\t\t\t\t\t\t\t\t" +
		"\t\t\t\"enum\": [\"choice\", \"text\", \"yesno\"],\n\t\t\t\t\t\t\t\t\t\t\t\"type\": \"string\"\n\t\t" +
		"\t\t\t\t\t\t\t\t}\n\t\t\t\t\t\t\t\t\t},\n\t\t\t\t\t\t\t\t\t\"required\": [\"question\", \"header\", " +
		"\"type\"]\n\t\t\t\t\t\t\t\t}\n\t\t\t\t\t\t\t}\n\t\t\t\t\t\t},\n\t\t\t\t\t\t\"required\": [\"" +
		"questions\"]\n\t\t\t\t\t}\n\t\t\t\t}]\n\t\t\t}]\n\t\t}\n\t}"))
	output, err := sdktranslator.TranslateRequestChecked(
		sdktranslator.WithPluginHooks(t.Context(), nil),
		sdktranslator.FormatGeminiCLI, sdktranslator.FormatCodex, "gpt-5.2", input, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	tool := gjson.GetBytes(output, "tools.0")
	if got := tool.Get("type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function; output=%s", got, output)
	}
	typeProperty := tool.Get("parameters.properties.questions.items.properties.type")
	if !typeProperty.IsObject() {
		t.Fatalf("schema property named type lost its object shape; output=%s", output)
	}
	if got := typeProperty.Get("type").String(); got != "string" {
		t.Fatalf("schema property type = %q, want string; output=%s", got, output)
	}
	if got := typeProperty.Get("default").String(); got != "choice" {
		t.Fatalf("schema default = %q, want choice; output=%s", got, output)
	}
	//goland:noinspection SpellCheckingInspection
	if got := typeProperty.Get("enum.2").String(); got != "yesno" {
		//goland:noinspection SpellCheckingInspection
		t.Fatalf("schema enum[2] = %q, want yesno; output=%s", got, output)
	}
}
