package gemini

import (
	"reflect"
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

func TestSetCodexToolChoiceFromGeminiToolConfigReusesAutoChoice(t *testing.T) {
	input := []byte(`{"tool_choice":"auto","input":[]}`)
	config := gjson.Parse(`{"mode":"AUTO"}`)

	output := setCodexToolChoiceFromGeminiToolConfig(input, config)

	if &output[0] != &input[0] {
		t.Fatal("AUTO tool choice caused a payload copy")
	}
}

func TestConvertGeminiRequestToCodexMapsFunctionCallingConfig(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}`)

	output := ConvertGeminiRequestToCodex("gpt-5.4", input, false)

	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function; output: %s", got, output)
	}
	if got := gjson.GetBytes(output, "tool_choice.name").String(); got != "lookup" {
		t.Fatalf("tool_choice.name = %q, want lookup; output: %s", got, output)
	}
}

func TestConvertGeminiRequestToCodexPreservesAndMatchesToolCallIDs(t *testing.T) {
	input := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"alpha","args":{}}},
				{"functionCall":{"call_id":"beta-id","name":"beta","args":{}}},
				{"functionCall":{"id":"alpha-two","name":"alpha","args":{}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"callId":"alpha-two","name":"alpha","response":{"result":"explicit"}}},
				{"functionResponse":{"name":"beta","response":{"result":"named"}}},
				{"functionResponse":{"response":{"result":"fifo"}}}
			]}
		]
	}`)

	output := ConvertGeminiRequestToCodex("gpt-5.4", input, false)
	var callIDs []string
	var outputIDs []string
	for _, item := range gjson.GetBytes(output, "input").Array() {
		switch item.Get("type").String() {
		case "function_call":
			callIDs = append(callIDs, item.Get("call_id").String())
		case "function_call_output":
			outputIDs = append(outputIDs, item.Get("call_id").String())
		}
	}
	wantCalls := []string{"call_gemini_0000000000000001", "beta-id", "alpha-two"}
	if !reflect.DeepEqual(callIDs, wantCalls) {
		t.Fatalf("function call IDs = %#v, want %#v; output: %s", callIDs, wantCalls, output)
	}
	wantOutputs := []string{"alpha-two", "beta-id", "call_gemini_0000000000000001"}
	if !reflect.DeepEqual(outputIDs, wantOutputs) {
		t.Fatalf("function output IDs = %#v, want %#v; output: %s", outputIDs, wantOutputs, output)
	}
}
