package gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToCodexDropsGeminiThinkingText(t *testing.T) {
	out := ConvertGeminiRequestToCodex(
		"gpt-5.4", []byte(`{
		"contents":[{
			"role":"model",
			"parts":[
				{"thought":true,"text":"hidden chain of thought","thoughtSignature":"skip_thought_signature_validator"},
				{"text":"visible answer"}
			]
		}]
	}`), false,
	)

	if strings.Contains(string(out), "hidden chain of thought") {
		t.Fatalf("output leaked Gemini thinking text: %s", out)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q, want visible answer; output: %s", got, out)
	}
}

func TestConvertGeminiRequestToCodexSupportsCamelCaseSystemInstruction(t *testing.T) {
	out := ConvertGeminiRequestToCodex(
		"gpt-5.4", []byte(`{
		"systemInstruction":{"parts":[{"text":"follow policy"}]},
		"contents":[{"role":"user","parts":[{"text":"hello"}]}]
	}`), false,
	)

	first := gjson.GetBytes(out, "input.0")
	if first.Get("role").String() != "developer" {
		t.Fatalf("input.0 role = %q, want developer; output: %s", first.Get("role").String(), out)
	}
	if got := first.Get("content.0.text").String(); got != "follow policy" {
		t.Fatalf("system instruction text = %q, want follow policy; output: %s", got, out)
	}
}

func TestConvertGeminiRequestToCodexPreservesSchemaPropertyNamedType(t *testing.T) {
	out := ConvertGeminiRequestToCodex(
		"gpt-5.4", []byte(`{
		"tools":[{
			"functionDeclarations":[{
				"name":"ask_user",
				"description":"Ask the user one or more questions.",
				"parametersJsonSchema":{
					"type":"object",
					"properties":{
						"questions":{
							"type":"array",
							"items":{
								"type":"object",
								"properties":{
									"header":{"type":"string"},
									"type":{
										"default":"choice",
										"description":"Question type.",
										"enum":["choice","text","yesno"],
										"type":"string"
									}
								},
								"required":["question","header","type"]
							}
						}
					},
					"required":["questions"]
				}
			}]
		}]
	}`), false,
	)

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function; output: %s", got, out)
	}
	typeProperty := tool.Get("parameters.properties.questions.items.properties.type")
	if !typeProperty.IsObject() {
		t.Fatalf("schema property named type should stay an object; output: %s", out)
	}
	if got := typeProperty.Get("type").String(); got != "string" {
		t.Fatalf("schema property type = %q, want string; output: %s", got, out)
	}
	if got := typeProperty.Get("default").String(); got != "choice" {
		t.Fatalf("schema property default = %q, want choice; output: %s", got, out)
	}
	if got := typeProperty.Get("enum.2").String(); got != "yesno" {
		t.Fatalf("schema property enum[2] = %q, want yesno; output: %s", got, out)
	}
}
