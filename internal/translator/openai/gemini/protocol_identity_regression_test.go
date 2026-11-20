package gemini_test

import (
	"context"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini-cli/openai/chat-completions"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/gemini"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/gemini-cli"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiSDKMediaSelectionAndUnknownToolResult(t *testing.T) {
	body := `{"systemInstruction":{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"https://example.` +
		`invalid/system.png"}}]},"contents":[{"role":"user","parts":[{"file_data":{"mime_type":"image/png","` +
		`file_uri":"https://example.invalid/user.png"}}]},{"role":"model","parts":[{"functionCall":{"id":"` +
		`call-a","name":"alpha","args":{}}},{"functionCall":{"id":"call-b","name":"beta","args":{}}}]},{"` +
		`role":"user","parts":[{"functionResponse":{"name":"unknown","response":{"result":"unknown"}}},{"` +
		`functionResponse":{"name":"beta","response":{"result":"beta"}}},{"functionResponse":{"name":"alpha",` +
		`"response":{"result":"alpha"}}}]}],"tools":[{"functionDeclarations":[{"name":"alpha"},{"name":"beta"` +
		`}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["beta"]}}}`
	for _, format := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		request := []byte(body)
		if format == sdktranslator.FormatGeminiCLI {
			request = []byte(`{"request":` + body + `}`)
		}
		out, err := sdktranslator.TranslateRequestChecked(
			context.Background(), format, sdktranslator.FormatOpenAI, "fixture", request, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string]string{
			"messages.0.content.0.image_url.url": "https://example.invalid/system.png",
			"messages.1.content.0.image_url.url": "https://example.invalid/user.png", "tool_choice.type": "function",
			"tool_choice.function.name": "beta",
		} {
			if got := gjson.GetBytes(out, path).String(); got != want {
				t.Fatalf("%s %s=%q want %q; %s", format, path, got, want, out)
			}
		}
		var ids []string
		for _, message := range gjson.GetBytes(out, "messages").Array() {
			if message.Get("role").String() == "tool" {
				ids = append(ids, message.Get("tool_call_id").String())
			}
		}
		if len(ids) != 3 || ids[0] == "call-a" || ids[0] == "call-b" || ids[1] != "call-b" || ids[2] != "call-a" {
			t.Fatalf("%s unknown name consumed another call: %v", format, ids)
		}
	}
}

func TestOpenAISDKGeminiResponseToolIDs(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		prefix := ""
		if format == sdktranslator.FormatGeminiCLI {
			prefix = "response."
		}
		var state any
		body := []byte(`{"choices":[{"index":0,"message":{"tool_calls":[{"id":"native-id","typ` +
			`e":"function","function":{"name":"lookup","arguments":"{\"value\":1}"}` +
			`}]},"finish_reason":"tool_calls"}]}`)
		out := sdktranslator.TranslateNonStream(
			context.Background(), sdktranslator.FormatOpenAI, format, "fixture", nil, nil, body, &state,
		)
		if gjson.GetBytes(out, prefix+"candidates.0.content.parts.0.functionCall.id").String() != "native-id" {
			t.Fatalf("%s nonstream ID missing: %s", format, out)
		}
		state = nil
		frames := []string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"native-id","type":"` +
				`function","function":{"name":"lookup","arguments":"{\"value\":1}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		found := false
		for _, frame := range frames {
			for _, chunk := range sdktranslator.Default().TranslateStream(
				context.Background(), sdktranslator.FormatOpenAI, format, "fixture", nil, nil, []byte(frame), &state,
			) {
				if gjson.GetBytes(
					chunk, prefix+"candidates.0.content.parts.0.functionCall.id",
				).String() == "native-id" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("%s stream ID missing", format)
		}
	}
}

func TestGeminiCLISDKMapsMaxTokensToLength(t *testing.T) {
	var state any
	chunks := sdktranslator.Default().TranslateStream(
		context.Background(), sdktranslator.FormatGeminiCLI, sdktranslator.FormatOpenAI, "fixture", nil, nil,
		[]byte(
			`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"candidatesTokenCount":3}}}`,
		),
		&state,
	)
	if len(chunks) != 1 || gjson.GetBytes(
		chunks[0], "choices.0.finish_reason",
	).String() != "length" || gjson.GetBytes(chunks[0], "choices.0.native_finish_reason").String() != "max_tokens" {
		t.Fatalf("nonstandard finish reason: %s", chunks)
	}
}
