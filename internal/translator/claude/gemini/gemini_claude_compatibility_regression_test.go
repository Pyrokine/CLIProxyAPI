package gemini_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	claudegemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/gemini"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/gemini-cli"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiClaudeSystemAndToolCompatibility(t *testing.T) {
	request := []byte(`{"systemInstruction":{"parts":[{"text":"camel directive"}]},"system_instruction":{"parts":[{"text":"snake directive"}]},"tools":[{"functionDeclarations":[{"name":"tool/name"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["tool/name"]}},"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool/name","id":"call_a","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"tool/name","id":"call_a","response":{"result":"result"}}}]}]}`)
	out := claudegemini.ConvertGeminiRequestToClaude("fixture-model", request, false)
	for path, want := range map[string]string{
		"messages.0.content.0.text": "camel directive",
		"tools.0.name":              "tool_name",
		"tools.0.input_schema.type": "object",
		"messages.1.content.0.name": "tool_name",
		"tool_choice.type":          "tool",
		"tool_choice.name":          "tool_name",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; %s", path, got, want, out)
		}
	}
	if !gjson.GetBytes(out, "tools.0.input_schema.properties").IsObject() {
		t.Fatalf("default properties absent: %s", out)
	}
}

func TestGeminiClaudeToolNamesRemainDistinct(t *testing.T) {
	request := []byte(`{"tools":[{"functionDeclarations":[{"name":"tool/name"},{"name":"tool:name"}]}],"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool:name","id":"call_a","args":{}}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["tool:name"]}}}`)
	out := claudegemini.ConvertGeminiRequestToClaude("fixture-model", request, false)
	first, second := gjson.GetBytes(out, "tools.0.name").String(), gjson.GetBytes(out, "tools.1.name").String()
	if first == second || len(first) > 64 || len(second) > 64 {
		t.Fatalf("distinct declarations became ambiguous: %s", out)
	}
	if gjson.GetBytes(out, "messages.0.content.0.name").String() != second || gjson.GetBytes(out, "tool_choice.name").String() != second {
		t.Fatalf("tool history or choice differs from declaration: %s", out)
	}
}

func TestGeminiClaudeToolNamesRoundTrip(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		t.Run(string(format), func(t *testing.T) {
			request := []byte(`{"tools":[{"functionDeclarations":[{"name":"tool/name"},{"name":"tool:name"}]}]}`)
			prefix := ""
			if format == sdktranslator.FormatGeminiCLI {
				request = []byte(`{"model":"fixture-model","request":` + string(request) + `}`)
				prefix = "response."
			}
			translated, err := sdktranslator.TranslateRequestChecked(context.Background(), format, sdktranslator.FormatClaude, "fixture-model", request, true)
			if err != nil {
				t.Fatal(err)
			}
			alias := gjson.GetBytes(translated, "tools.1.name").String()
			events := []string{
				`data: {"type":"message_start","message":{"id":"msg_b","model":"fixture-model"}}`,
				fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_b","name":%q,"input":{}}}`, alias),
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				`data: {"type":"content_block_stop","index":0}`,
			}
			var param any
			out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, format, "fixture-model", request, translated, []byte(strings.Join(events, "\n\n")), &param)
			path := prefix + "candidates.0.content.parts.0.functionCall.name"
			if got := gjson.GetBytes(out, path).String(); got != "tool:name" {
				t.Fatalf("nonstream tool name = %q, want original: %s", got, out)
			}
			param = nil
			found := false
			for _, event := range events {
				for _, frame := range sdktranslator.Default().TranslateStream(context.Background(), sdktranslator.FormatClaude, format, "fixture-model", request, translated, []byte(event), &param) {
					if name := gjson.GetBytes(frame, path); name.Exists() {
						found = true
						if name.String() != "tool:name" {
							t.Fatalf("stream tool name = %q, want original: %s", name.String(), frame)
						}
					}
				}
			}
			if !found {
				t.Fatal("stream tool call missing")
			}
		})
	}
}

func TestGeminiClaudeCheckedMediaRejection(t *testing.T) {
	for _, part := range []string{`{"inlineData":{"mimeType":"audio/wav","data":"YQ=="}}`, `{"fileData":{"mimeType":"audio/wav","fileUri":"https://example.com/audio.wav"}}`, `{"fileData":{"mimeType":"image/png"}}`} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
			t.Run(fmt.Sprintf("%s %s", format, part), func(t *testing.T) {
				body := []byte(`{"contents":[{"role":"user","parts":[{"text":"earlier valid turn"}]},{"role":"model","parts":[{"text":"answer"}]},{"role":"user","parts":[` + part + `]}]}`)
				if format == sdktranslator.FormatGeminiCLI {
					body = []byte(`{"model":"fixture-model","request":` + string(body) + `}`)
				}
				out, err := sdktranslator.TranslateRequestChecked(context.Background(), format, sdktranslator.FormatClaude, "fixture-model", body, false)
				var unsupported *translatorcommon.UnsupportedPartError
				if !errors.As(err, &unsupported) || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || len(out) != 0 {
					t.Fatalf("unsupported attachment was accepted: err=%v body=%s", err, out)
				}
			})
		}
	}
	for _, part := range []string{`{"text":"sendable"},{"inlineData":{"mimeType":"audio/wav","data":"YQ=="}}`, `{"inlineData":{"mimeType":"application/pdf","data":"YQ=="}}`} {
		out, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatClaude, "fixture-model", []byte(`{"contents":[{"role":"user","parts":[`+part+`]}]}`), false)
		if err != nil || gjson.GetBytes(out, "messages.#").Int() != 1 {
			t.Fatalf("sendable user turn rejected: err=%v body=%s", err, out)
		}
	}
}
