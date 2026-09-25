package executor

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityBuildRequest_SanitizesGeminiToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "gemini-2.5-pro")

	decl := extractFirstFunctionDeclaration(t, body)
	if _, ok := decl["parametersJsonSchema"]; ok {
		t.Fatalf("parametersJsonSchema should be renamed to parameters")
	}

	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequest_SanitizesAntigravityToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "claude-opus-4-6")

	decl := extractFirstFunctionDeclaration(t, body)
	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequestRoutesImageGeneration(t *testing.T) {
	tests := []struct {
		name            string
		payload         []byte
		wantRequestType string
	}{
		{
			name:            "default image request type",
			payload:         []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"draw a cat"}]}]}}`),
			wantRequestType: "image_gen",
		},
		{
			name:            "explicit request type",
			payload:         []byte(`{"requestType":"custom_image","request":{"contents":[{"role":"user","parts":[{"text":"draw a cat"}]}]}}`),
			wantRequestType: "custom_image",
		},
	}
	requestIDPattern := regexp.MustCompile(`^image_gen/[0-9]+/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}/12$`)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &AntigravityExecutor{}
			auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "test-project"}}
			req, err := executor.buildRequest(
				context.Background(),
				auth,
				"token",
				"gemini-3.1-flash-image",
				test.payload,
				false,
				"",
				"https://example.com",
				"derived-session-id",
			)
			if err != nil {
				t.Fatalf("buildRequest error: %v", err)
			}
			if req.URL.Path != antigravityGeneratePath || req.URL.RawQuery != "" {
				t.Fatalf("request URL = %s, want non-stream generation path", req.URL.String())
			}
			body, errRead := io.ReadAll(req.Body)
			if errRead != nil {
				t.Fatalf("read request body: %v", errRead)
			}
			if got := gjson.GetBytes(body, "requestType").String(); got != test.wantRequestType {
				t.Fatalf("requestType = %q, want %q; body=%s", got, test.wantRequestType, body)
			}
			if requestID := gjson.GetBytes(body, "requestId").String(); !requestIDPattern.MatchString(requestID) {
				t.Fatalf("requestId = %q, want image_gen/<millis>/<uuid>/12", requestID)
			}
			if sessionID := gjson.GetBytes(body, "request.sessionId"); sessionID.Exists() {
				t.Fatalf("image request generated sessionId = %s; body=%s", sessionID.Raw, body)
			}
		})
	}
}

func TestValidateAntigravityRequestSignatures_DropsInvalidClaudeThinkingSignature(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"bad","signature":"skip_thought_signature_validator"},{"type":"text","text":"visible"}]}]}`)

	out, err := validateAntigravityRequestSignatures(
		"claude-opus-4-6",
		sdktranslator.FromString("claude"),
		payload,
	)
	if err != nil {
		t.Fatalf("validateAntigravityRequestSignatures error: %v", err)
	}
	content := gjson.GetBytes(out, "messages.0.content")
	if got := len(content.Array()); got != 1 {
		t.Fatalf("content length = %d, want 1. Payload: %s", got, out)
	}
	if got := content.Array()[0].Get("text").String(); got != "visible" {
		t.Fatalf("text = %q, want visible. Payload: %s", got, out)
	}
}

func buildRequestBodyFromPayload(t *testing.T, modelName string) map[string]any {
	t.Helper()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "test-project"}}
	payload := []byte(`{
		"request": {
			"tools": [
				{
					"function_declarations": [
						{
							"name": "tool_1",
							"parametersJsonSchema": {
								"$schema": "http://json-schema.org/draft-07/schema#",
								"$id": "root-schema",
								"type": "object",
								"properties": {
									"$id": {"type": "string"},
									"arg": {
										"type": "object",
										"prefill": "hello",
										"properties": {
											"mode": {
												"type": "string",
												"enum": ["a", "b"],
												"enumTitles": ["A", "B"]
											}
										}
									}
								},
								"patternProperties": {
									"^x-": {"type": "string"}
								}
							}
						}
					]
				}
			]
		}
	}`)

	req, err := executor.buildRequest(
		context.Background(), auth, "token", modelName, payload, false, "", "https://example.com",
	)
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body error: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal request body error: %v, body=%s", err, string(raw))
	}
	return body
}

func extractFirstFunctionDeclaration(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or empty")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("first tool invalid type")
	}
	decls, ok := tool["function_declarations"].([]any)
	if !ok || len(decls) == 0 {
		t.Fatalf("function_declarations missing or empty")
	}
	decl, ok := decls[0].(map[string]any)
	if !ok {
		t.Fatalf("first function declaration invalid type")
	}
	return decl
}

func assertSchemaSanitizedAndPropertyPreserved(t *testing.T, params map[string]any) {
	t.Helper()

	if _, ok := params["$id"]; ok {
		t.Fatalf("root $id should be removed from schema")
	}
	if _, ok := params["patternProperties"]; ok {
		t.Fatalf("patternProperties should be removed from schema")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or invalid type")
	}
	if _, ok := props["$id"]; !ok {
		t.Fatalf("property named $id should be preserved")
	}

	arg, ok := props["arg"].(map[string]any)
	if !ok {
		t.Fatalf("arg property missing or invalid type")
	}
	if _, ok := arg["prefill"]; ok {
		t.Fatalf("prefill should be removed from nested schema")
	}

	argProps, ok := arg["properties"].(map[string]any)
	if !ok {
		t.Fatalf("arg.properties missing or invalid type")
	}
	mode, ok := argProps["mode"].(map[string]any)
	if !ok {
		t.Fatalf("mode property missing or invalid type")
	}
	if _, ok := mode["enumTitles"]; ok {
		t.Fatalf("enumTitles should be removed from nested schema")
	}
}
