package gemini

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
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

func TestGeminiCodexRegisteredMedia(t *testing.T) {
	cases := []struct {
		name, part, partType, field, value string
	}{
		{"image", `{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}`, "input_image", "image_url", "data:image/png;base64,aGVsbG8="},
		{"snake image", `{"inline_data":{"mime_type":"image/png","data":"aGVsbG8="}}`, "input_image", "image_url", "data:image/png;base64,aGVsbG8="},
		{"audio", `{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}}`, "input_audio", "input_audio.format", "wav"},
		{"pdf", `{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}`, "input_file", "file_data", "data:application/pdf;base64,JVBERi0="},
		{"video", `{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}}`, "input_file", "file_data", "data:video/mp4;base64,AAAAIGZ0eXA="},
		{"image reference", `{"fileData":{"mimeType":"image/png","fileUri":"https://example.test/image.png"}}`, "input_image", "image_url", "https://example.test/image.png"},
		{"file reference", `{"file_data":{"mime_type":"application/pdf","file_uri":"https://example.test/file.pdf"}}`, "input_file", "file_url", "https://example.test/file.pdf"},
		{"blank aliases", `{"fileData":{"mimeType":" ","mime_type":"application/pdf","fileUri":" ","file_uri":"https://example.test/file.pdf"}}`, "input_file", "file_url", "https://example.test/file.pdf"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			request := []byte(fmt.Sprintf(`{"contents":[{"parts":[%s]}]}`, tt.part))
			body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatCodex, "codex-test", request, false)
			if err != nil || !gjson.ValidBytes(body) {
				t.Fatalf("media translation = (%s, %v)", body, err)
			}
			part := gjson.GetBytes(body, "input.0.content.0")
			if part.Get("type").String() != tt.partType || part.Get(tt.field).String() != tt.value || gjson.GetBytes(body, "input.0.role").String() != "user" {
				t.Fatalf("media translation = %s", body)
			}
		})
	}
}

func TestGeminiCodexRejectsEmptiedMediaTurns(t *testing.T) {
	cases := []struct{ part, partType string }{
		{`{"inlineData":{"mimeType":"image/png","data":""}}`, "inlineData"},
		{`{"inline_data":{"mime_type":"image/png","data":7}}`, "inline_data"},
		{`{"inlineData":{"data":"aGVsbG8="}}`, "inlineData"},
		{`{"fileData":{"mimeType":"audio/wav","fileUri":"https://example.test/a.wav"}}`, "fileData"},
		{`{"file_data":{"mime_type":"application/pdf","file_uri":" "}}`, "file_data"},
		{`{"inlineData":{"mimeType":7,"data":"aGVsbG8="}}`, "inlineData"},
		{`{"inline_data":{"mime_type":{},"data":"aGVsbG8="}}`, "inline_data"},
		{`{"fileData":{"mimeType":"application/pdf","fileUri":7}}`, "fileData"},
		{`{"inlineData":{"mimeType":"image/png","data":" "}}`, "inlineData"},
		{`{"fileData":false}`, "fileData"},
	}
	for index, tt := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			request := []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"before"}]},{"role":"model","parts":[{"text":"reply"}]},{"role":"user","parts":[{"text":" "},%s]},{"role":"model","parts":[{"text":"reply"}]},{"role":"user","parts":[{"text":"after"}]}]}`, tt.part))
			for _, stream := range []bool{false, true} {
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatCodex, "codex-test", request, stream)
				var rejected *translatorcommon.UnsupportedPartError
				if !errors.As(err, &rejected) || rejected.Type != tt.partType || rejected.StatusCode() != 400 || !rejected.IsRequestScoped() || len(body) != 0 {
					t.Fatalf("empty media turn = (%s, %v), want 400 for %s", body, err, tt.partType)
				}
			}
			if body := ConvertGeminiRequestToCodex("codex-test", request, false); len(body) != 0 {
				t.Fatalf("byte request retained rejected input: %s", body)
			}
			mixed := []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[%s,{"text":"keep"}]}]}`, tt.part))
			body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatCodex, "codex-test", mixed, false)
			if err != nil || gjson.GetBytes(body, "input.0.content.0.text").String() != "keep" {
				t.Fatalf("mixed media turn = (%s, %v)", body, err)
			}
			toolResult := []byte(fmt.Sprintf(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{}}}]},{"role":"user","parts":[%s,{"functionResponse":{"id":"call-1","name":"lookup","response":{"result":"keep"}}}]}]}`, tt.part))
			body, err = sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatCodex, "codex-test", toolResult, false)
			if err != nil || gjson.GetBytes(body, "input.1.output").String() != "keep" || gjson.GetBytes(body, "input.1.call_id").String() != "call-1" {
				t.Fatalf("media beside tool result = (%s, %v)", body, err)
			}
		})
	}
}
