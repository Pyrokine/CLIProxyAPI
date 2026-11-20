package gemini

import (
	"context"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiCLISDKPreservesMixedToolResultParts(t *testing.T) {
	request := []byte((`{"model":"fixture","contents":[{"role":"model","parts":[{"functionCall":{"id":"call-a"` +
		`,"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call-a","name":"` +
		`lookup","response":{"result":"ok"}}},{"text":"visible"},{"inlineData":{"mimeType":"image/png","data"` +
		`:"YQ=="}}]}]}`))
	out, err := sdktranslator.TranslateRequestChecked(
		context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI, "fixture", request, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"request.contents.1.parts.0.functionResponse.id": "call-a", "request.contents.2.parts.0.text": "visible",
		"request.contents.2.parts.1.inlineData.data": "YQ==",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s=%q want %q; %s", path, got, want, out)
		}
	}
}
