package gemini

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityClaudeStripsDuplicateFunctionParentSignatures(t *testing.T) {
	part := `{"functionCall":{"name":"lookup","args":{"thoughtSignature":"business data"}},"functionCall":{"` +
		`name":"lookup","args":{"thoughtSignature":"business data"},"thoughtSignature":"nested-provider-` +
		`signature","thought_signature":"nested-alias"}}`
	body := []byte(`{"request":{"contents":[{"role":"model","parts":[` + part + `]}]}}`)
	assertClean := func(out []byte) {
		t.Helper()
		var decoded map[string]any
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatal(err)
		}
		request := decoded["request"].(map[string]any)
		content := request["contents"].([]any)[0].(map[string]any)
		part := content["parts"].([]any)[0].(map[string]any)
		call := part["functionCall"].(map[string]any)
		if _, exists := call["thoughtSignature"]; exists {
			t.Fatalf("last duplicate parent retains signature: %s", out)
		}
		if _, exists := call["thought_signature"]; exists {
			t.Fatalf("last duplicate parent retains alias: %s", out)
		}
		if call["args"].(map[string]any)["thoughtSignature"] != "business data" {
			t.Fatalf("business argument changed: %s", out)
		}
		gjson.GetBytes(out, "request.contents.0.parts.0").ForEach(
			func(key, value gjson.Result) bool {
				if key.String() == "functionCall" &&
					(value.Get("thoughtSignature").Exists() || value.Get("thought_signature").Exists()) {
					t.Fatalf("duplicate parent remains signed: %s", out)
				}
				return true
			},
		)
	}
	assertClean(SanitizeAntigravityClaudeGeminiRequestSignatures("claude-test", body))
	out, err := sdk.TranslateRequestChecked(
		context.Background(),
		sdk.FormatGemini,
		sdk.FormatAntigravity,
		"claude-test",
		[]byte(
			`{"contents":[{"role":"model","parts":[`+part+`]},{"role":"user","parts":[{"text":"continue"}]}]}`,
		),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertClean(out)
}
