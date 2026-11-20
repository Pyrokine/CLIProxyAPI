package executor

import (
	"context"
	"io"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAntigravityNativeSchemaCarrierReachesFinalRequestUnchanged(t *testing.T) {
	const schema = `{"type":"object","encrypted":true,"properties":{
		"action":{"anyOf":[
			{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},
			{"type":"null"}
		]},
		"conviction":{"type":"number","enum":[0.25,0.5,1],"minimum":0.25,"maximum":1},
		"count":{"type":"integer","enum":[1,2],"maximum":9007199254740993},
		"enabled":{"type":"boolean","enum":[true,false]}
	},"required":["action","count"],"additionalProperties":false,
	"propertyOrdering":["enabled","count","conviction"]}`
	for _, model := range []string{"gemini-2.5-pro", "claude-opus-4-6"} {
		for _, path := range []string{
			"request.tools.0.functionDeclarations.0.parametersJsonSchema",
			"request.tools.0.function_declarations.0.parameters_json_schema",
			"request.tools.0.functionDeclarations.0.responseJsonSchema",
			"request.tools.0.function_declarations.0.response_json_schema",
			"request.generationConfig.responseJsonSchema",
			"request.generationConfig.response_json_schema",
			"request.generation_config.responseJsonSchema",
			"request.generation_config.response_json_schema",
		} {
			t.Run(
				model+"/"+path, func(t *testing.T) {
					payload, err := sjson.SetRaw(
						`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
						path, schema,
					)
					if err != nil {
						t.Fatal(err)
					}
					provider := &AntigravityExecutor{}
					auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "test-project"}}
					prepared, err := provider.prepareAntigravityRequestPayload(
						t.Context(), auth,
						cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}, []byte(payload),
						[]byte(payload),
					)
					if err != nil {
						t.Fatal(err)
					}
					request, err := provider.buildRequest(
						context.Background(), auth, "synthetic-token",
						prepared, false, "", "https://example.invalid",
					)
					if err != nil {
						t.Fatal(err)
					}
					defer func(closeResource func() error) { _ = closeResource() }(request.Body.Close)
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					got := gjson.GetBytes(body, path)
					if !got.Exists() {
						t.Fatalf("native carrier was renamed or removed: %s", body)
					}
					for relative, want := range map[string]string{
						"properties.action.anyOf.1.type": "null",
						"properties.conviction.enum":     "[0.25,0.5,1]",
						"properties.conviction.minimum":  "0.25",
						"properties.count.enum":          "[1,2]",
						"properties.count.maximum":       "9007199254740993",
						"properties.enabled.enum":        "[true,false]",
						"required":                       `["action","count"]`,
						"additionalProperties":           "false",
						"propertyOrdering":               `["enabled","count","conviction"]`,
					} {
						if value := got.Get(relative); value.String() != want {
							t.Errorf("%s=%s want=%s; schema=%s", relative, value.Raw, want, got.Raw)
						}
					}
					if got.Get("encrypted").Exists() {
						t.Errorf("unsupported schema metadata survived: %s", got.Raw)
					}
				},
			)
		}
	}
}
