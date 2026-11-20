package claude

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiCLISDKToolIdentityAndContinuation(t *testing.T) {
	request := []byte((`{"messages":[{"role":"user","content":"query"},{"role":"assistant","content":[{"type":` +
		`"tool_use","id":"call-b","name":"tool:name","input":{"value":1}}]}],"tools":[{"name":"tool/name","` +
		`input_schema":{"type":"object","properties":{}}},{"name":"tool:name","input_schema":{"type":"object"` +
		`,"properties":{}}}],"tool_choice":{"type":"tool","name":"tool:name"}}`))
	translated, err := sdktranslator.TranslateRequestChecked(
		context.Background(),
		sdktranslator.FormatClaude,
		sdktranslator.FormatGeminiCLI,
		"fixture",
		request,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	first, second := gjson.GetBytes(
		translated, "request.tools.0.functionDeclarations.0.name",
	).String(), gjson.GetBytes(translated, "request.tools.0.functionDeclarations.1.name").String()
	if first == second || second == "" || gjson.GetBytes(
		translated, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
	).String() != second || gjson.GetBytes(
		translated, "request.contents.1.parts.0.functionCall.name",
	).String() != second {
		t.Fatalf("ambiguous tool mapping: %s", translated)
	}
	body := []byte(fmt.Sprintf(
		`{"response":{"responseId":"id","modelVersion":"fixture","candidates":[`+
			`{"content":{"parts":[{"functionCall":{"id":"call-b","name":%q,"args":{`+
			`"value":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCo`+
			`unt":2,"candidatesTokenCount":3}}}`,
		second,
	))
	var state any
	out := sdktranslator.TranslateNonStream(
		context.Background(),
		sdktranslator.FormatGeminiCLI,
		sdktranslator.FormatClaude,
		"fixture",
		request,
		translated,
		body,
		&state,
	)
	if gjson.GetBytes(out, "content.0.name").String() != "tool:name" || gjson.GetBytes(
		out, "content.0.id",
	).String() != "call-b" {
		t.Fatalf("nonstream tool identity lost: %s", out)
	}
	state = nil
	frames := [][]byte{
		[]byte(fmt.Sprintf(
			`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":`+
				`"call-b","name":%q,"args":{"value":`+
				`1}}}]}}]}}`,
			second,
		)),
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"next":2}}}]}}]}}`),
		[]byte(
			`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":` +
				`{"promptTokenCount":2,"candidatesTokenCount":` +
				`3}}}`,
		),
		[]byte(`[DONE]`), []byte(`[DONE]`),
	}
	var result strings.Builder
	for _, frame := range frames {
		for _, chunk := range sdktranslator.Default().TranslateStream(
			context.Background(), sdktranslator.FormatGeminiCLI, sdktranslator.FormatClaude, "fixture", request,
			translated, frame, &state,
		) {
			result.Write(chunk)
		}
	}
	got := result.String()
	for _, want := range []string{`"name":"tool:name"`, `"id":"call-b"`, `"stop_reason":"tool_use"`, `\"next\":2`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s: %s", want, got)
		}
	}
	if strings.Count(got, "event: content_block_start") != 1 || strings.Count(
		got, "event: content_block_stop",
	) != 1 || strings.Count(got, "event: message_stop") != 1 {
		t.Fatalf("continuation or termination repeated: %s", got)
	}
}
