package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBuiltinPatchNamespaceAliasesAndHistory(t *testing.T) {
	const input = "历史中文🧪\n<>&"
	for _, tc := range []struct {
		format   sdktranslator.Format
		namePath string
		history  string
		encoded  bool
	}{
		{sdktranslator.FormatOpenAI, "tools.0.function.name", `messages.#(role=="assistant").tool_calls.0.function.arguments`, true},
		{sdktranslator.FormatClaude, "tools.0.name", `messages.#(role=="assistant").content.#(type=="tool_use").input.input`, false},
		{sdktranslator.FormatGemini, "tools.0.functionDeclarations.0.name", `contents.#(role=="model").parts.0.functionCall.args.input`, false},
		{sdktranslator.FormatGeminiCLI, "request.tools.0.functionDeclarations.0.name", `request.contents.#(role=="model").parts.0.functionCall.args.input`, false},
		{sdktranslator.FormatAntigravity, "request.tools.0.functionDeclarations.0.name", `request.contents.#(role=="model").parts.0.functionCall.args.input`, false},
		{sdktranslator.FormatInteractions, "tools.0.name", "input.0.arguments.input", false},
	} {
		for _, namespace := range []string{"editor", strings.Repeat("editor", 12)} {
			t.Run(fmt.Sprintf("%s/%s", tc.format, namespace), func(t *testing.T) {
				request := []byte(translatedPatchRequest)
				tools := []byte(`[{"type":"namespace","name":"","tools":[{"type":"custom","name":"apply_patch"}]},{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"value":{"type":"number"}}}}]`)
				tools, err := sjson.SetBytes(tools, "0.name", namespace)
				if err != nil {
					t.Fatal(err)
				}
				request, _ = sjson.SetRawBytes(request, "tools", tools)
				request, _ = sjson.SetBytes(request, "tool_choice.namespace", namespace)
				history := []byte(`[{"type":"custom_tool_call","call_id":"call_history","name":"apply_patch","namespace":"","input":""},{"type":"custom_tool_call_output","call_id":"call_history","output":"offline result"}]`)
				history, _ = sjson.SetBytes(history, "0.namespace", namespace)
				history, _ = sjson.SetBytes(history, "0.input", input)
				request, _ = sjson.SetRawBytes(request, "input", history)
				translated, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, tc.format, "offline-model", request, false)
				if err != nil {
					t.Fatal(err)
				}
				gotInput := gjson.GetBytes(translated, tc.history).String()
				if tc.encoded {
					gotInput = gjson.Get(gotInput, "input").String()
				}
				if gotInput != input || !strings.Contains(string(translated), "offline result") {
					t.Fatalf("history was not consumed at %s: %s", tc.history, translated)
				}
				name := gjson.GetBytes(translated, tc.namePath).String()
				if name == "" || name == "apply_patch" {
					t.Fatalf("namespace was lost at %s: %s", tc.namePath, translated)
				}
				ordinaryBody, _ := patchProtocolResponse(t, tc.format, "apply_patch", `{"value":42}`)
				var ordinaryParameter any
				ordinary := sdktranslator.TranslateNonStream(context.Background(), tc.format, sdktranslator.FormatOpenAIResponse, "offline-model", request, translated, []byte(ordinaryBody), &ordinaryParameter)
				ordinaryItem := gjson.GetBytes(ordinary, "output.0")
				if sdktranslator.ResponseTranslationError(&ordinaryParameter) != nil || ordinaryItem.Get("type").String() != "function_call" || ordinaryItem.Get("namespace").Exists() || gjson.Get(ordinaryItem.Get("arguments").String(), "value").Int() != 42 {
					t.Fatalf("ordinary same-name function changed: %s", ordinary)
				}
				arguments, _ := sjson.Set(`{}`, "input", input)
				body, stream := patchProtocolResponse(t, tc.format, name, arguments)
				for _, streaming := range []bool{false, true} {
					var parameter any
					var response []byte
					if streaming {
						for _, frame := range strings.Split(stream, "\n\n") {
							for _, converted := range sdktranslator.TranslateStream(context.Background(), tc.format, sdktranslator.FormatOpenAIResponse, "offline-model", request, translated, []byte(frame), &parameter) {
								for _, line := range strings.Split(string(converted), "\n") {
									payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
									if gjson.Get(payload, "type").String() == "response.completed" {
										response = []byte(gjson.Get(payload, "response").Raw)
									}
								}
							}
						}
						for _, converted := range sdktranslator.TranslateStream(context.Background(), tc.format, sdktranslator.FormatOpenAIResponse, "offline-model", request, translated, []byte("[DONE]"), &parameter) {
							for _, line := range strings.Split(string(converted), "\n") {
								payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
								if gjson.Get(payload, "type").String() == "response.completed" {
									response = []byte(gjson.Get(payload, "response").Raw)
								}
							}
						}
					} else {
						response = sdktranslator.TranslateNonStream(context.Background(), tc.format, sdktranslator.FormatOpenAIResponse, "offline-model", request, translated, []byte(body), &parameter)
					}
					if errTranslate := sdktranslator.ResponseTranslationError(&parameter); errTranslate != nil {
						t.Fatal(errTranslate)
					}
					item := gjson.GetBytes(response, "output.0")
					if item.Get("type").String() != "custom_tool_call" || item.Get("name").String() != "apply_patch" || item.Get("namespace").String() != namespace || item.Get("input").String() != input {
						t.Fatalf("stream=%t namespace/alias not restored: %s", streaming, response)
					}
				}
			})
		}
	}
}
