package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	codexclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/claude"
	codexchat "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/chat-completions"
	codexresponses "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/responses"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type toolFixture struct {
	ID        string
	Name      string
	Arguments string
	Result    string
}

type protocolFixture struct {
	name            string
	request         func([]byte) []byte
	stream          func([]byte, []byte, *any) [][]byte
	nonStream       func([]byte, []byte) []byte
	requestJSON     func([]toolFixture) []byte
	normalizeStream func(*testing.T, [][]byte) normalizedResponse
	normalizeFinal  func(*testing.T, []byte) normalizedResponse
	stopReason      string
}

type normalizedCall struct {
	ID        string
	Name      string
	Arguments string
}

type normalizedResponse struct {
	Calls             []normalizedCall
	Text              string
	StopReason        string
	TerminalCount     int
	StopMetadataCount int
}

func TestClaudeGeminiDropsEmptyTurns(t *testing.T) {
	for _, target := range []struct {
		name    string
		format  sdktranslator.Format
		path    string
		content string
	}{
		{"Gemini", sdktranslator.FormatGemini, "contents", `[{"type":"text","text":""}]`},
		{"GeminiCLI", sdktranslator.FormatGeminiCLI, "request.contents", `[]`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", target.name, stream), func(t *testing.T) {
				input := []byte(fmt.Sprintf(`{"messages":[
					{"role":"user","content":"hello"},
					{"role":"assistant","content":%s},
					{"role":"user","content":[]},
					{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"opaque-client-signature"}]},
					{"role":"user","content":"continue"}
				]}`, target.content))
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatClaude, target.format, "gemini-3-pro", input, stream)
				if err != nil {
					t.Fatalf("empty-turn translation failed: %v", err)
				}
				contents := gjson.GetBytes(body, target.path).Array()
				if len(contents) != 2 {
					t.Fatalf("contents length = %d, want 2 nonempty turns: %s", len(contents), body)
				}
				for index, text := range []string{"hello", "continue"} {
					if contents[index].Get("role").String() != "user" || contents[index].Get("parts.#").Int() != 1 || contents[index].Get("parts.0.text").String() != text {
						t.Fatalf("contents.%d = %s, want user turn with text %q", index, contents[index].Raw, text)
					}
				}
			})
		}
	}
}

func TestClaudeGeminiRequestContract(t *testing.T) {
	cases := []struct {
		name, input string
		values      map[string]string
		reminders   map[string]string
	}{
		{
			name:   "tool input JSON is not a schema",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"url":{"type":"string","format":"uri","value":"keep"}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}]}`,
			values: map[string]string{"contents.0.parts.0.functionCall.args.url.type": "string", "contents.0.parts.0.functionCall.args.url.format": "uri", "contents.0.parts.0.functionCall.args.url.value": "keep"},
		},
		{
			name:   "parallel calls with mixed text",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call.a","name":"first","input":{}},{"type":"tool_use","id":"call_a","name":"second","input":{}}]},{"role":"user","content":[{"type":"text","text":"before"},{"type":"tool_result","tool_use_id":"call_a","content":"two"},{"type":"tool_result","tool_use_id":"call.a","content":"one"},{"type":"text","text":"after"}]}]}`,
			values: map[string]string{"contents.#": "2", "contents.0.parts.0.functionCall.id": "call.a", "contents.0.parts.1.functionCall.id": "call_a", "contents.1.parts.0.text": "before", "contents.1.parts.1.text": "after", "contents.1.parts.2.functionResponse.id": "call.a", "contents.1.parts.2.functionResponse.name": "first", "contents.1.parts.2.functionResponse.response.result": "one", "contents.1.parts.3.functionResponse.id": "call_a", "contents.1.parts.3.functionResponse.name": "second"},
		},
		{
			name:      "instructions between calls and results",
			input:     `{"system":"top","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"system","content":"system rule"},{"role":"developer","content":[{"type":"text","text":"developer rule"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}]}`,
			values:    map[string]string{"systemInstruction.parts.0.text": "top", "contents.#": "3", "contents.2.role": "user", "contents.2.parts.#": "3", "contents.2.parts.2.functionResponse.id": "call_1", "contents.2.parts.2.functionResponse.name": "lookup"},
			reminders: map[string]string{"contents.2.parts.0.text": "system rule", "contents.2.parts.1.text": "developer rule"},
		},
		{
			name:      "consecutive user and instructions",
			input:     `{"messages":[{"role":"user","content":"hello"},{"role":"system","content":"system rule"},{"role":"developer","content":"developer rule"}]}`,
			values:    map[string]string{"contents.#": "1", "contents.0.role": "user", "contents.0.parts.#": "3", "contents.0.parts.0.text": "hello"},
			reminders: map[string]string{"contents.0.parts.1.text": "system rule", "contents.0.parts.2.text": "developer rule"},
		},
		{
			name:   "structured result and trailing text keep image slot",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"alpha"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},{"type":"text","text":"after"}]}]}`,
			values: map[string]string{"contents.1.parts.#": "3", "contents.1.parts.0.text": "after", "contents.1.parts.1.{image}.mime_type": "image/png", "contents.1.parts.1.{image}.data": "aGVsbG8=", "contents.1.parts.2.functionResponse.id": "toolu_1", "contents.1.parts.2.functionResponse.name": "lookup", "contents.1.parts.2.functionResponse.response.result.text": "alpha"},
		},
		{
			name:   "string result is not double encoded",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"old-prefix-1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"old-prefix-1","content":"alpha"}]}]}`,
			values: map[string]string{"contents.0.parts.0.functionCall.name": "lookup", "contents.1.parts.0.functionResponse.name": "lookup", "contents.1.parts.0.functionResponse.response.result": "alpha"},
		},
		{
			name:   "JSON result stays structured",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":{"value":42}}]}]}`,
			values: map[string]string{"contents.1.parts.0.functionResponse.response.result.value": "42"},
		},
		{
			name:   "JSON reference stays opaque",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":{"schema":{"$ref":"#/Item"}}}]}]}`,
			values: map[string]string{"contents.1.parts.0.functionResponse.response.result": `{"schema":{"$ref":"#/Item"}}`},
		},
		{
			name:   "opaque ID and sanitized tool name",
			input:  `{"tools":[{"name":"1 search/tool","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"tool","name":"1 search/tool"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"cpa_gemini_0123456789abcdef0123456789abcdef","name":"1 search/tool","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"cpa_gemini_0123456789abcdef0123456789abcdef","content":"done"}]}]}`,
			values: map[string]string{"tools.0.functionDeclarations.0.name": "_1_search_tool", "toolConfig.functionCallingConfig.allowedFunctionNames.0": "_1_search_tool", "contents.0.parts.0.functionCall.id": "cpa_gemini_0123456789abcdef0123456789abcdef", "contents.0.parts.0.functionCall.name": "_1_search_tool", "contents.1.parts.0.functionResponse.id": "cpa_gemini_0123456789abcdef0123456789abcdef", "contents.1.parts.0.functionResponse.name": "_1_search_tool"},
		},
		{
			name:   "reused ID resolves against current call group",
			input:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"first","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"one"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"second","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"two"}]}]}`,
			values: map[string]string{"contents.#": "4", "contents.1.parts.0.functionResponse.name": "first", "contents.3.parts.0.functionResponse.name": "second", "contents.3.parts.0.functionResponse.response.result": "two"},
		},
		{
			name:      "parallel results across user messages",
			input:     `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"first","input":{}},{"type":"tool_use","id":"call_2","name":"second","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2","content":"two"}]},{"role":"system","content":"middle"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"one"}]}]}`,
			values:    map[string]string{"contents.#": "2", "contents.1.parts.1.functionResponse.id": "call_1", "contents.1.parts.1.functionResponse.name": "first", "contents.1.parts.2.functionResponse.id": "call_2", "contents.1.parts.2.functionResponse.name": "second"},
			reminders: map[string]string{"contents.1.parts.0.text": "middle"},
		},
	}
	for _, target := range []struct {
		name, root, image string
		format            sdktranslator.Format
	}{
		{"Gemini", "", "inline_data", sdktranslator.FormatGemini},
		{"GeminiCLI", "request", "inlineData", sdktranslator.FormatGeminiCLI},
	} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", target.name, stream, tc.name), func(t *testing.T) {
					input := []byte(tc.input)
					body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatClaude, target.format, "gemini-3-flash-preview", input, stream)
					if err != nil || !gjson.ValidBytes(body) {
						t.Fatalf("registered checked translation=(%s, %v)", body, err)
					}
					legacy := sdktranslator.TranslateRequest(sdktranslator.FormatClaude, target.format, "gemini-3-flash-preview", input, stream)
					if string(legacy) != string(body) {
						t.Fatalf("registered byte and checked bodies differ: byte=%s, checked=%s", legacy, body)
					}
					if err := internalsignature.ValidateGeminiFunctionCallPairing(body); err != nil {
						t.Fatalf("registered translation produced invalid pairing: %v; body=%s", err, body)
					}
					root := gjson.ParseBytes(body)
					if target.root != "" {
						root = root.Get(target.root)
					}
					for path, want := range tc.values {
						path = strings.ReplaceAll(path, "{image}", target.image)
						if got := root.Get(path).String(); got != want {
							t.Fatalf("%s=%q, want %q; body=%s", path, got, want, body)
						}
					}
					for path, text := range tc.reminders {
						want := text
						if target.format == sdktranslator.FormatGemini {
							want = "<system-reminder>\n" + text + "\n</system-reminder>"
						}
						if got := root.Get(path).String(); got != want {
							t.Fatalf("%s=%q, want %q; body=%s", path, got, want, body)
						}
					}
				})
			}
		}
	}
}

func TestClaudeGeminiUserTurnAttachmentContract(t *testing.T) {
	cases := []struct {
		name, content, errorType, data string
	}{
		{"container upload", `{"type":"container_upload","file_id":"file-1"}`, "container_upload", ""},
		{"file document", `{"type":"document","source":{"type":"file","file_id":"file-1"}}`, "document", ""},
		{"URL image", `{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}`, "image", ""},
		{"file image", `{"type":"image","source":{"type":"file","file_id":"file-1"}}`, "image", ""},
		{"empty text", `{"type":"text","text":""},{"type":"image","source":{"type":"file","file_id":"file-1"}}`, "image", ""},
		{"whitespace text", `{"type":"text","text":" \n\t "},{"type":"image","source":{"type":"file","file_id":"file-1"}}`, "image", ""},
		{"NBSP text", `{"type":"text","text":" "},{"type":"image","source":{"type":"file","file_id":"file-1"}}`, "image", ""},
		{"trailing whitespace", `{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}},{"type":"text","text":"  "}`, "image", ""},
		{"incomplete base64", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}}`, "image", ""},
		{"base64 image", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`, "", "aGVsbG8="},
		{"base64 PDF", `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}}`, "", "JVBERi0xLjQK"},
		{"real text beside whitespace and attachment", `{"type":"text","text":"  "},{"type":"text","text":"keep"},{"type":"image","source":{"type":"file","file_id":"file-1"}}`, "", ""},
	}
	for _, target := range []struct {
		name, root, image string
		format            sdktranslator.Format
	}{
		{"Gemini", "", "inline_data", sdktranslator.FormatGemini},
		{"GeminiCLI", "request", "inlineData", sdktranslator.FormatGeminiCLI},
	} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", target.name, stream, tc.name), func(t *testing.T) {
					input := []byte(`{"system":"top","messages":[{"role":"user","content":"history"},{"role":"assistant","content":"answer"},{"role":"system","content":"system reminder"},{"role":"developer","content":"developer reminder"},{"role":"user","content":[` + tc.content + `]}]}`)
					body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatClaude, target.format, "gemini-3-flash-preview", input, stream)
					if tc.errorType != "" {
						var unsupported *translatorcommon.UnsupportedPartError
						if !errors.As(err, &unsupported) || unsupported.Type != tc.errorType || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || len(body) != 0 {
							t.Fatalf("emptied user turn=(%s, %v), want request-scoped 400 for %s", body, err, tc.errorType)
						}
						if want := "unsupported content part: " + tc.errorType; err.Error() != want {
							t.Fatalf("error=%q, want %q", err.Error(), want)
						}
						return
					}
					if err != nil || !gjson.ValidBytes(body) {
						t.Fatalf("sendable user turn=(%s, %v)", body, err)
					}
					root := gjson.ParseBytes(body)
					if target.root != "" {
						root = root.Get(target.root)
					}
					parts := root.Get("contents.2.parts")
					if tc.data != "" {
						if got := parts.Get("#(" + target.image + ")." + target.image + ".data").String(); got != tc.data {
							t.Fatalf("inline bytes=%q, want %q; body=%s", got, tc.data, body)
						}
					} else if got := parts.Get(`#(text=="keep").text`).String(); got != "keep" {
						t.Fatalf("real user text was not preserved: %s", body)
					}
				})
			}
		}
	}
}

func TestUnsupportedUserAttachmentConformance(t *testing.T) {
	claudeFileID := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"earlier"}]},{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file-1"}}]},{"role":"user","content":[{"type":"text","text":"later"}]}]}`)
	openAIFileID := []byte(`{"messages":[{"role":"user","content":"earlier"},{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}}]},{"role":"user","content":"later"}]}`)
	openAIEmptyFile := []byte(`{"messages":[{"role":"user","content":"earlier"},{"role":"user","content":[{"type":"file","file":{"filename":"missing.pdf"}}]},{"role":"user","content":"later"}]}`)
	responsesFileID := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"earlier"}]},{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-1"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"later"}]}]}`)
	tests := []struct {
		name       string
		from, to   sdktranslator.Format
		input      []byte
		partType   string
		validInput []byte
		validPath  string
	}{
		{"Claude/Codex", sdktranslator.FormatClaude, sdktranslator.FormatCodex, claudeFileID, "document", []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}]}]}`), "input.0.content.0.file_data"},
		{"Claude/Gemini", sdktranslator.FormatClaude, sdktranslator.FormatGemini, claudeFileID, "document", []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}]}]}`), "contents.0.parts.0.inline_data.data"},
		{"Claude/OpenAI", sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, claudeFileID, "document", []byte(`{"messages":[{"role":"user","content":[{"type":"document","filename":"document.pdf","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}]}]}`), "messages.0.content.0.file.file_data"},
		{"Claude/Interactions", sdktranslator.FormatClaude, sdktranslator.FormatInteractions, claudeFileID, "document", []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}]}]}`), "input.0.content.0.data"},
		{"OpenAI/Claude", sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, openAIFileID, "file", []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`), "messages.0.content.0.source.data"},
		{"OpenAI/Gemini", sdktranslator.FormatOpenAI, sdktranslator.FormatGemini, openAIFileID, "file", []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`), "contents.0.parts.0.inlineData.data"},
		{"OpenAI/Interactions", sdktranslator.FormatOpenAI, sdktranslator.FormatInteractions, openAIFileID, "file", []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`), "input.0.content.0.data"},
		{"Responses/Claude", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, responsesFileID, "input_file", []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}]}]}`), "messages.0.content.0.source.data"},
		{"Responses/Interactions", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatInteractions, responsesFileID, "input_file", []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}]}]}`), "input.0.content.0.data"},
		{"Claude/Antigravity", sdktranslator.FormatClaude, sdktranslator.FormatAntigravity, claudeFileID, "document", []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}]}]}`), "request.contents.0.parts.0.inlineData.data"},
		{"Claude/GeminiCLI", sdktranslator.FormatClaude, sdktranslator.FormatGeminiCLI, claudeFileID, "document", []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}]}]}`), "request.contents.0.parts.0.inlineData.data"},
		{"OpenAI/Codex", sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, openAIEmptyFile, "file", []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`), "input.0.content.0.file_data"},
		{"OpenAI/Antigravity", sdktranslator.FormatOpenAI, sdktranslator.FormatAntigravity, openAIFileID, "file", []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`), "request.contents.0.parts.0.inlineData.data"},
		{"OpenAI/GeminiCLI", sdktranslator.FormatOpenAI, sdktranslator.FormatGeminiCLI, openAIFileID, "file", []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`), "request.contents.0.parts.0.inlineData.data"},
		{"Responses/Gemini", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, responsesFileID, "input_file", []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}]}]}`), "contents.0.parts.0.inline_data.data"},
		{"Responses/Antigravity", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatAntigravity, responsesFileID, "input_file", []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}]}]}`), "request.contents.0.parts.0.inline_data.data"},
		{"Responses/GeminiCLI", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGeminiCLI, responsesFileID, "input_file", []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}]}]}`), "request.contents.0.parts.0.inline_data.data"},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), tt.from, tt.to, "test-model", tt.input, stream)
				var unsupported *translatorcommon.UnsupportedPartError
				if !errors.As(err, &unsupported) || unsupported.Type != tt.partType || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || len(body) != 0 {
					t.Fatalf("unsupported user turn = (%s, %v), want request-scoped 400 for %s", body, err, tt.partType)
				}
				body, err = sdktranslator.TranslateRequestChecked(context.Background(), tt.from, tt.to, "test-model", tt.validInput, stream)
				if err != nil || gjson.GetBytes(body, tt.validPath).String() == "" {
					t.Fatalf("valid inline document = (%s, %v), want preserved %s", body, err, tt.validPath)
				}
			})
		}
	}
}

func TestMixedUserAttachmentPreservesText(t *testing.T) {
	claude := []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file-1"}},{"type":"text","text":"keep"}]}]}`)
	chat := []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}},{"type":"text","text":"keep"}]}]}`)
	responses := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"},{"type":"input_text","text":"keep"}]}]}`)
	for _, tt := range []struct {
		name     string
		from, to sdktranslator.Format
		input    []byte
		textPath string
	}{
		{"Claude/Antigravity", sdktranslator.FormatClaude, sdktranslator.FormatAntigravity, claude, "request.contents.0.parts.0.text"},
		{"Claude/GeminiCLI", sdktranslator.FormatClaude, sdktranslator.FormatGeminiCLI, claude, "request.contents.0.parts.0.text"},
		{"OpenAI/Codex", sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, chat, "input.0.content.1.text"},
		{"OpenAI/Interactions", sdktranslator.FormatOpenAI, sdktranslator.FormatInteractions, chat, "input.0.content.0.text"},
		{"OpenAI/Antigravity", sdktranslator.FormatOpenAI, sdktranslator.FormatAntigravity, chat, "request.contents.0.parts.0.text"},
		{"OpenAI/GeminiCLI", sdktranslator.FormatOpenAI, sdktranslator.FormatGeminiCLI, chat, "request.contents.0.parts.0.text"},
		{"Responses/Gemini", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, responses, "contents.0.parts.0.text"},
		{"Responses/Interactions", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatInteractions, responses, "input.0.content.0.text"},
		{"Responses/Antigravity", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatAntigravity, responses, "request.contents.0.parts.0.text"},
		{"Responses/GeminiCLI", sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGeminiCLI, responses, "request.contents.0.parts.0.text"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), tt.from, tt.to, "test-model", tt.input, stream)
				if err != nil || gjson.GetBytes(body, tt.textPath).String() != "keep" {
					t.Fatalf("mixed user text = (%s, %v), want keep at %s", body, err, tt.textPath)
				}
				if tt.name == "OpenAI/Codex" {
					parts := gjson.GetBytes(body, "input.0.content").Array()
					if len(parts) != 2 || parts[0].Get("type").String() != "input_file" || parts[0].Get("file_id").String() != "file-1" {
						t.Fatalf("supported file reference or content order changed: %s", body)
					}
				}
			})
		}
	}
}

func TestUnsupportedUserAttachmentRejectsBlankText(t *testing.T) {
	claude := []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file-1"}},{"type":"text","text":" \n\t "}]}]}`)
	chat := []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}},{"type":"text","text":" \n\t "}]}]}`)
	for _, tt := range []struct {
		name     string
		from, to sdktranslator.Format
		input    []byte
		partType string
	}{
		{"Claude/Gemini", sdktranslator.FormatClaude, sdktranslator.FormatGemini, claude, "document"},
		{"Claude/GeminiCLI", sdktranslator.FormatClaude, sdktranslator.FormatGeminiCLI, claude, "document"},
		{"Claude/Antigravity", sdktranslator.FormatClaude, sdktranslator.FormatAntigravity, claude, "document"},
		{"OpenAI/Gemini", sdktranslator.FormatOpenAI, sdktranslator.FormatGemini, chat, "file"},
		{"OpenAI/Antigravity", sdktranslator.FormatOpenAI, sdktranslator.FormatAntigravity, chat, "file"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), tt.from, tt.to, "test-model", tt.input, stream)
				var unsupported *translatorcommon.UnsupportedPartError
				if !errors.As(err, &unsupported) || unsupported.Type != tt.partType || len(body) != 0 {
					t.Fatalf("blank text must not hide unsupported attachment: body=%s, err=%v", body, err)
				}
			})
		}
	}
}

func TestResponsesAudioToInteractions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			input := []byte(`{"input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav","data":"aGVsbG8="}}]}]}`)
			body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatInteractions, "test-model", input, stream)
			if err != nil || gjson.GetBytes(body, "input.0.content.0.mime_type").String() != "audio/wav" ||
				gjson.GetBytes(body, "input.0.content.0.data").String() != "aGVsbG8=" {
				t.Fatalf("audio translation = (%s, %v), want inline WAV", body, err)
			}
		})
	}
}

func TestInteractionsUserAttachmentsConformance(t *testing.T) {
	targets := []struct {
		name     string
		format   sdktranslator.Format
		textPath string
		filePath string
	}{
		{"Codex", sdktranslator.FormatCodex, "input.0.content.0.text", "input.0.content.0.file_data"},
		{"Gemini", sdktranslator.FormatGemini, "contents.0.parts.0.text", "contents.0.parts.0.inlineData.data"},
		{"Antigravity", sdktranslator.FormatAntigravity, "request.contents.0.parts.0.text", "request.contents.0.parts.0.inlineData.data"},
	}
	unsupported := []struct {
		name, input, partType string
	}{
		{"separate user turn", `{"input":[{"type":"user_input","content":[{"type":"text","text":"before"}]},{"type":"model_output","content":[{"type":"text","text":"reply"}]},{"type":"user_input","content":[{"type":"file","file":{"file_id":"file-1"}}]},{"type":"model_output","content":[{"type":"text","text":"reply"}]},{"type":"user_input","content":[{"type":"text","text":"after"}]}]}`, "file"},
		{"nested user after model", `{"input":{"role":"model","steps":[{"type":"model_output","content":[{"type":"image_url","image_url":{"url":""}}]},{"role":"user","steps":[{"type":"user_input","content":[{"type":"image_url","image_url":{"url":""}}]}]}]}}`, "image_url"},
		{"unknown attachment type", `{"input":[{"type":"user_input","content":[{"type":"binary","mime_type":"application/pdf","data":"aGVsbG8="}]}]}`, "binary"},
		{"unknown after instruction", `{"input":[{"type":"user_input","content":[{"type":"text","text":"before"}]},{"role":"system","content":[{"type":"text","text":"instruction"}]},{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="}]}]}`, "binary"},
		{"unknown before instruction", `{"input":[{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="}]},{"role":"developer","content":[{"type":"text","text":"instruction"}]},{"type":"user_input","content":[{"type":"text","text":"after"}]}]}`, "binary"},
		{"nested type system", `{"input":[{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="}]},{"type":"system","steps":[{"type":"user_input","content":[{"type":"text","text":"instruction"}]}]}]}`, "binary"},
		{"nested type developer", `{"input":[{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="}]},{"type":"developer","steps":[{"type":"user_input","content":[{"type":"text","text":"instruction"}]}]}]}`, "binary"},
		{"instruction type wins over user role", `{"input":[{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="}]},{"type":"system","role":"user","content":[{"type":"text","text":"instruction"}]}]}`, "binary"},
		{"empty audio", `{"input":[{"type":"user_input","content":[{"type":"input_audio","input_audio":{"format":"wav"}}]}]}`, "input_audio"},
		{"empty native inline data", `{"input":[{"type":"user_input","parts":[{"inlineData":{"mimeType":"application/pdf","data":""}}]}]}`, "inline_data"},
		{"native object ignored", `{"input":[{"type":"user_input","parts":{"inlineData":{"mimeType":"application/pdf","data":"aGVsbG8="}}}]}`, "inline_data"},
	}
	for _, target := range targets {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", target.name, stream), func(t *testing.T) {
				translate := func(input string) ([]byte, error) {
					return sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, target.format, "test-model", []byte(input), stream)
				}
				for _, tt := range unsupported {
					t.Run(tt.name, func(t *testing.T) {
						body, err := translate(tt.input)
						var rejected *translatorcommon.UnsupportedPartError
						if !errors.As(err, &rejected) || rejected.Type != tt.partType || rejected.StatusCode() != 400 || !rejected.IsRequestScoped() || len(body) != 0 {
							t.Fatalf("unsupported user turn = (%s, %v), want request-scoped 400 for %s", body, err, tt.partType)
						}
					})
				}
				body, err := translate(`{"input":[{"type":"user_input","content":[{"type":"file","file":{"file_id":"file-1"}},{"type":"text","text":"keep"}]}]}`)
				if err != nil || gjson.GetBytes(body, target.textPath).String() != "keep" {
					t.Fatalf("mixed user turn = (%s, %v), want retained text", body, err)
				}
				body, err = translate(`{"input":[{"type":"user_input","content":[{"type":"file","file":{"file_id":"file-1"}}]},{"type":"user_input","content":[{"type":"text","text":"keep"}]}]}`)
				if err != nil || gjson.GetBytes(body, target.textPath).String() != "keep" {
					t.Fatalf("adjacent user steps = (%s, %v), want retained text", body, err)
				}
				body, err = translate(`{"input":[{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="},{"type":"text","text":"keep"}]}]}`)
				if err != nil || gjson.GetBytes(body, target.textPath).String() != "keep" {
					t.Fatalf("unknown attachment with text = (%s, %v), want retained text", body, err)
				}
				body, err = translate(`{"input":[{"type":"user_input","content":[{"type":"binary","data":"aGVsbG8="}]},{"type":"user_input","content":[{"type":"text","text":"keep"}]}]}`)
				if err != nil || gjson.GetBytes(body, target.textPath).String() != "keep" {
					t.Fatalf("adjacent unknown attachment and text = (%s, %v), want retained text", body, err)
				}
				body, err = translate(`{"input":[{"type":"user_input","content":[{"type":"file","file":{"filename":"document.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}]}]}`)
				if err != nil || gjson.GetBytes(body, target.filePath).String() == "" {
					t.Fatalf("valid file = (%s, %v), want preserved document", body, err)
				}
				body, err = translate(`{"input":[{"type":"user_input","content":[{"type":"text","text":"keep"}],"parts":[{"type":"image","mime_type":"image/png","data":"aGVsbG8="}]}]}`)
				if target.name == "Codex" {
					if err != nil || gjson.GetBytes(body, target.textPath).String() != "keep" {
						t.Fatalf("Codex content precedence = (%s, %v), want retained text", body, err)
					}
					body, err = translate(`{"input":[{"type":"user_input","parts":[{"type":"image","mime_type":"image/png","data":"aGVsbG8="}],"text":"keep"}]}`)
					if err != nil || gjson.GetBytes(body, target.textPath).String() != "keep" {
						t.Fatalf("Codex text fallback = (%s, %v), want retained text", body, err)
					}
				} else {
					var rejected *translatorcommon.UnsupportedPartError
					if !errors.As(err, &rejected) || rejected.Type != "image" || len(body) != 0 {
						t.Fatalf("native parts precedence = (%s, %v), want request-scoped image rejection", body, err)
					}
				}
			})
		}
	}
	for _, target := range targets[1:] {
		t.Run(target.name+"/native files", func(t *testing.T) {
			for _, tt := range []struct{ input, path string }{
				{`{"input":[{"type":"user_input","parts":[{"inlineData":{"mimeType":"application/pdf","data":"aGVsbG8="}}]}]}`, target.filePath},
				{`{"input":[{"type":"user_input","parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"gs://bucket/document.pdf"}}]}]}`, strings.Replace(target.filePath, "inlineData.data", "fileData.fileUri", 1)},
			} {
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, target.format, "test-model", []byte(tt.input), false)
				if err != nil || gjson.GetBytes(body, tt.path).String() == "" {
					t.Fatalf("native file = (%s, %v), want preserved %s", body, err, tt.path)
				}
			}
		})
	}
}

func TestToolOnlyRequestConformance(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		for _, test := range toolFixtureCases() {
			t.Run(
				protocol.name+"/"+test.name, func(t *testing.T) {
					request := protocol.requestJSON(test.calls)
					assertCanonicalToolRequest(t, protocol.request(request), test.calls)
				},
			)
		}
	}
}

func TestToolOnlyResponseStreamMatchesNonStream(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		for _, test := range toolFixtureCases() {
			t.Run(
				protocol.name+"/"+test.name, func(t *testing.T) {
					originalRequest := protocol.requestJSON(test.calls)
					var state any
					var translated [][]byte
					for _, event := range codexToolStream(test.calls, "response.completed") {
						translated = append(translated, protocol.stream(originalRequest, event, &state)...)
					}

					stream := protocol.normalizeStream(t, translated)
					final := protocol.normalizeFinal(
						t, protocol.nonStream(originalRequest, codexToolTerminal(test.calls, "response.completed")),
					)
					assertToolOnlyResponse(t, stream, test.calls, protocol.stopReason)
					assertToolOnlyResponse(t, final, test.calls, protocol.stopReason)
					if fmt.Sprint(stream.Calls) != fmt.Sprint(final.Calls) {
						t.Fatalf("stream calls = %#v, non-stream calls = %#v", stream.Calls, final.Calls)
					}
				},
			)
		}
	}
}

func TestToolOnlyStreamTerminalAliases(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		for _, terminalType := range []string{"response.completed", "response.done", "response.incomplete"} {
			t.Run(
				protocol.name+"/"+terminalType, func(t *testing.T) {
					calls := toolFixtureCases()[0].calls
					originalRequest := protocol.requestJSON(calls)
					var state any
					var translated [][]byte
					for _, event := range codexToolStream(calls, terminalType) {
						translated = append(translated, protocol.stream(originalRequest, event, &state)...)
					}
					assertSingleStreamTerminal(t, protocol.normalizeStream(t, translated), translated)
				},
			)
		}
	}
}

func TestToolOnlyStreamSuppressesDuplicateTerminal(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		t.Run(
			protocol.name, func(t *testing.T) {
				calls := toolFixtureCases()[0].calls
				originalRequest := protocol.requestJSON(calls)
				var state any
				var translated [][]byte
				for _, event := range codexToolStream(calls, "response.completed") {
					translated = append(translated, protocol.stream(originalRequest, event, &state)...)
				}
				translated = append(
					translated,
					protocol.stream(
						originalRequest, codexSSE(string(codexToolTerminal(calls, "response.done"))), &state,
					)...,
				)
				assertSingleStreamTerminal(t, protocol.normalizeStream(t, translated), translated)
			},
		)
	}
}

func assertSingleStreamTerminal(t *testing.T, response normalizedResponse, output [][]byte) {
	t.Helper()
	if response.TerminalCount != 1 {
		t.Fatalf("terminal count = %d, want 1; output=%q", response.TerminalCount, output)
	}
	if response.StopMetadataCount != 1 {
		t.Fatalf("stop metadata count = %d, want 1; output=%q", response.StopMetadataCount, output)
	}
}

func protocolFixtures() []protocolFixture {
	return []protocolFixture{
		{
			name: "openai-chat",
			request: func(raw []byte) []byte {
				return codexchat.ConvertOpenAIRequestToCodex("gpt-5.6-sol", raw, true)
			},
			stream: func(original, event []byte, state *any) [][]byte {
				return codexchat.ConvertCodexResponseToOpenAI(
					context.Background(), "gpt-5.6-sol", original, nil, event, state,
				)
			},
			nonStream: func(original, terminal []byte) []byte {
				return codexchat.ConvertCodexResponseToOpenAINonStream(
					context.Background(), "gpt-5.6-sol", original, nil, terminal, nil,
				)
			},
			requestJSON:     openAIChatRequest,
			normalizeStream: normalizeOpenAIChatStream,
			normalizeFinal:  normalizeOpenAIChatFinal,
			stopReason:      "tool_calls",
		},
		{
			name: "openai-responses",
			request: func(raw []byte) []byte {
				return codexresponses.ConvertOpenAIResponsesRequestToCodex("gpt-5.6-sol", raw, true)
			},
			stream: func(original, event []byte, state *any) [][]byte {
				return codexresponses.ConvertCodexResponseToOpenAIResponses(
					context.Background(), "gpt-5.6-sol", original, nil, event, state,
				)
			},
			nonStream: func(original, terminal []byte) []byte {
				return codexresponses.ConvertCodexResponseToOpenAIResponsesNonStream(
					context.Background(), "gpt-5.6-sol", original, nil, terminal, nil,
				)
			},
			requestJSON:     openAIResponsesRequest,
			normalizeStream: normalizeOpenAIResponsesStream,
			normalizeFinal:  normalizeOpenAIResponsesFinal,
			stopReason:      "completed",
		},
		{
			name: "anthropic-messages",
			request: func(raw []byte) []byte {
				return codexclaude.ConvertClaudeRequestToCodex("gpt-5.6-sol", raw, true)
			},
			stream: func(original, event []byte, state *any) [][]byte {
				return codexclaude.ConvertCodexResponseToClaude(
					context.Background(), "gpt-5.6-sol", original, nil, event, state,
				)
			},
			nonStream: func(original, terminal []byte) []byte {
				return codexclaude.ConvertCodexResponseToClaudeNonStream(
					context.Background(), "gpt-5.6-sol", original, nil, terminal, nil,
				)
			},
			requestJSON:     anthropicMessagesRequest,
			normalizeStream: normalizeAnthropicStream,
			normalizeFinal:  normalizeAnthropicFinal,
			stopReason:      "tool_use",
		},
	}
}

func toolFixtureCases() []struct {
	name  string
	calls []toolFixture
} {
	calls := []toolFixture{
		{ID: "call_weather", Name: "weather", Arguments: `{"city":"北京"}`, Result: "sunny"},
		{ID: "call_time", Name: "time", Arguments: `{"zone":"UTC"}`, Result: "12:00"},
	}
	return []struct {
		name  string
		calls []toolFixture
	}{
		{name: "single", calls: calls[:1]},
		{name: "parallel", calls: calls},
	}
}

func openAIChatRequest(calls []toolFixture) []byte {
	var toolCalls strings.Builder
	var results strings.Builder
	var tools strings.Builder
	for index, call := range calls {
		if index > 0 {
			toolCalls.WriteByte(',')
			results.WriteByte(',')
			tools.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&toolCalls,
			`{"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Name),
			strconv.Quote(call.Arguments),
		)
		_, _ = fmt.Fprintf(
			&results,
			`{"role":"tool","tool_call_id":%s,"content":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Result),
		)
		_, _ = fmt.Fprintf(
			&tools,
			`{"type":"function","function":{"name":%s,"parameters":{"type":"object"}}}`,
			strconv.Quote(call.Name),
		)
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"Use tools"},{"role":"assistant","content":null,"tool_calls":[%s]},%s],"tools":[%s]}`,
		toolCalls.String(), results.String(), tools.String(),
	))
}

func openAIResponsesRequest(calls []toolFixture) []byte {
	var input strings.Builder
	var tools strings.Builder
	input.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Use tools"}]}`)
	for index, call := range calls {
		_, _ = fmt.Fprintf(
			&input,
			`,{"type":"function_call","call_id":%s,"name":%s,"arguments":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Name),
			strconv.Quote(call.Arguments),
		)
		if index > 0 {
			tools.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&tools,
			`{"type":"function","name":%s,"parameters":{"type":"object"}}`,
			strconv.Quote(call.Name),
		)
	}
	for _, call := range calls {
		_, _ = fmt.Fprintf(
			&input,
			`,{"type":"function_call_output","call_id":%s,"output":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Result),
		)
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5.6-sol","stream":true,"input":[%s],"tools":[%s]}`,
		input.String(), tools.String(),
	))
}

func anthropicMessagesRequest(calls []toolFixture) []byte {
	var toolUse strings.Builder
	var toolResults strings.Builder
	var tools strings.Builder
	for index, call := range calls {
		if index > 0 {
			toolUse.WriteByte(',')
			toolResults.WriteByte(',')
			tools.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&toolUse,
			`{"type":"tool_use","id":%s,"name":%s,"input":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Name),
			call.Arguments,
		)
		_, _ = fmt.Fprintf(
			&toolResults,
			`{"type":"tool_result","tool_use_id":%s,"content":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Result),
		)
		_, _ = fmt.Fprintf(
			&tools,
			`{"name":%s,"input_schema":{"type":"object"}}`,
			strconv.Quote(call.Name),
		)
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"Use tools"}]},{"role":"assistant","content":[%s]},{"role":"user","content":[%s]}],"tools":[%s]}`,
		toolUse.String(), toolResults.String(), tools.String(),
	))
}

func assertCanonicalToolRequest(t *testing.T, raw []byte, expected []toolFixture) {
	t.Helper()
	items := gjson.GetBytes(raw, "input").Array()
	calls := make([]normalizedCall, 0, len(expected))
	outputs := make(
		[]struct {
			ID     string
			Result string
		}, 0, len(expected),
	)
	seenOutput := false
	for _, item := range items {
		switch item.Get("type").String() {
		case "message":
			if item.Get("role").String() == "assistant" && len(item.Get("content").Array()) == 0 {
				t.Fatalf("empty assistant message was emitted: %s", item.Raw)
			}
		case "function_call":
			if seenOutput {
				t.Fatalf("function call was emitted after a tool result: %s", raw)
			}
			calls = append(
				calls, normalizedCall{
					ID:        item.Get("call_id").String(),
					Name:      item.Get("name").String(),
					Arguments: item.Get("arguments").String(),
				},
			)
		case "function_call_output":
			seenOutput = true
			outputs = append(
				outputs, struct {
					ID     string
					Result string
				}{ID: item.Get("call_id").String(), Result: item.Get("output").String()},
			)
		}
	}
	if len(calls) != len(expected) || len(outputs) != len(expected) {
		t.Fatalf("calls=%#v outputs=%#v, want %d pairs; request=%s", calls, outputs, len(expected), raw)
	}
	for index, want := range expected {
		if calls[index] != (normalizedCall{ID: want.ID, Name: want.Name, Arguments: want.Arguments}) {
			t.Fatalf("call %d = %#v, want %#v", index, calls[index], want)
		}
		if outputs[index].ID != want.ID || outputs[index].Result != want.Result {
			t.Fatalf("output %d = %#v, want id=%q result=%q", index, outputs[index], want.ID, want.Result)
		}
	}
}

func codexToolStream(calls []toolFixture, terminalType string) [][]byte {
	events := [][]byte{
		codexSSE(`{"type":"response.created","response":{"id":"resp_tools","model":"gpt-5.6-sol","created_at":1700000000}}`),
	}
	for index, call := range calls {
		events = append(
			events, codexSSE(
				fmt.Sprintf(
					`{"type":"response.output_item.added","output_index":%d,"item":{"id":"item_%d","type":"function_call","call_id":%s,"name":%s,"arguments":""}}`,
					index, index, strconv.Quote(call.ID), strconv.Quote(call.Name),
				),
			),
		)
	}
	for fragmentIndex := range 2 {
		for index, call := range calls {
			fragments := splitArguments(call.Arguments)
			events = append(
				events, codexSSE(
					fmt.Sprintf(
						`{"type":"response.function_call_arguments.delta","item_id":"item_%d","output_index":%d,"delta":%s}`,
						index, index, strconv.Quote(fragments[fragmentIndex]),
					),
				),
			)
		}
	}
	for index, call := range calls {
		events = append(
			events,
			codexSSE(
				fmt.Sprintf(
					`{"type":"response.function_call_arguments.done","item_id":"item_%d","output_index":%d,"arguments":%s}`,
					index, index, strconv.Quote(call.Arguments),
				),
			),
			codexSSE(
				fmt.Sprintf(
					`{"type":"response.output_item.done","output_index":%d,"item":{"id":"item_%d","type":"function_call","call_id":%s,"name":%s,"arguments":%s}}`,
					index, index, strconv.Quote(call.ID), strconv.Quote(call.Name), strconv.Quote(call.Arguments),
				),
			),
		)
	}
	return append(events, codexSSE(string(codexToolTerminal(calls, terminalType))))
}

func codexToolTerminal(calls []toolFixture, terminalType string) []byte {
	status := "completed"
	if terminalType == "response.incomplete" {
		status = "incomplete"
	}
	var output strings.Builder
	for index, call := range calls {
		if index > 0 {
			output.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&output,
			`{"id":"item_%d","type":"function_call","call_id":%s,"name":%s,"arguments":%s}`,
			index, strconv.Quote(call.ID), strconv.Quote(call.Name), strconv.Quote(call.Arguments),
		)
	}
	incomplete := ""
	if terminalType == "response.incomplete" {
		incomplete = `,"incomplete_details":{"reason":"max_output_tokens"}`
	}
	return []byte(fmt.Sprintf(
		`{"type":%s,"response":{"id":"resp_tools","model":"gpt-5.6-sol","status":%s,"output":[%s],"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}%s}}`,
		strconv.Quote(terminalType), strconv.Quote(status), output.String(), incomplete,
	))
}

func codexSSE(payload string) []byte {
	return []byte("data: " + payload)
}

func splitArguments(arguments string) [2]string {
	middle := len(arguments) / 2
	for middle < len(arguments) && arguments[middle]&0xc0 == 0x80 {
		middle++
	}
	return [2]string{arguments[:middle], arguments[middle:]}
}

func normalizeOpenAIChatStream(t *testing.T, chunks [][]byte) normalizedResponse {
	t.Helper()
	response := normalizedResponse{}
	calls := make(map[int]*normalizedCall)
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if !gjson.ValidBytes(chunk) {
			t.Fatalf("invalid Chat chunk: %q", chunk)
		}
		root := gjson.ParseBytes(chunk)
		if content := root.Get("choices.0.delta.content"); content.Exists() && content.String() != "" {
			response.Text += content.String()
		}
		for _, item := range root.Get("choices.0.delta.tool_calls").Array() {
			index := int(item.Get("index").Int())
			call := calls[index]
			if call == nil {
				call = &normalizedCall{}
				calls[index] = call
			}
			if value := item.Get("id").String(); value != "" {
				call.ID = value
			}
			if value := item.Get("function.name").String(); value != "" {
				call.Name = value
			}
			call.Arguments += item.Get("function.arguments").String()
		}
		if finishReason := root.Get("choices.0.finish_reason").String(); finishReason != "" {
			response.TerminalCount++
			response.StopMetadataCount++
			response.StopReason = finishReason
		}
	}
	for index := 0; index < len(calls); index++ {
		if calls[index] == nil {
			t.Fatalf("missing Chat tool call index %d", index)
		}
		response.Calls = append(response.Calls, *calls[index])
	}
	return response
}

func normalizeOpenAIChatFinal(t *testing.T, raw []byte) normalizedResponse {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid Chat response: %q", raw)
	}
	response := normalizedResponse{
		Text:              gjson.GetBytes(raw, "choices.0.message.content").String(),
		StopReason:        gjson.GetBytes(raw, "choices.0.finish_reason").String(),
		TerminalCount:     1,
		StopMetadataCount: 1,
	}
	for _, item := range gjson.GetBytes(raw, "choices.0.message.tool_calls").Array() {
		response.Calls = append(
			response.Calls, normalizedCall{
				ID:        item.Get("id").String(),
				Name:      item.Get("function.name").String(),
				Arguments: item.Get("function.arguments").String(),
			},
		)
	}
	return response
}

func normalizeOpenAIResponsesStream(t *testing.T, chunks [][]byte) normalizedResponse {
	t.Helper()
	response := normalizedResponse{}
	calls := make(map[int]*normalizedCall)
	for _, chunk := range chunks {
		for _, payload := range sseDataPayloads(t, chunk) {
			root := gjson.ParseBytes(payload)
			switch root.Get("type").String() {
			case "response.output_item.added":
				item := root.Get("item")
				if item.Get("type").String() != "function_call" {
					continue
				}
				index := int(root.Get("output_index").Int())
				calls[index] = &normalizedCall{
					ID:   item.Get("call_id").String(),
					Name: item.Get("name").String(),
				}
			case "response.function_call_arguments.delta":
				index := int(root.Get("output_index").Int())
				if calls[index] == nil {
					t.Fatalf("Responses arguments delta targets missing index %d", index)
				}
				calls[index].Arguments += root.Get("delta").String()
			case "response.output_text.delta":
				response.Text += root.Get("delta").String()
			case "response.completed", "response.done":
				response.TerminalCount++
				response.StopMetadataCount++
				response.StopReason = "completed"
			case "response.incomplete":
				response.TerminalCount++
				response.StopMetadataCount++
				response.StopReason = "incomplete"
			}
		}
	}
	for index := 0; index < len(calls); index++ {
		if calls[index] == nil {
			t.Fatalf("missing Responses tool call index %d", index)
		}
		response.Calls = append(response.Calls, *calls[index])
	}
	return response
}

func normalizeOpenAIResponsesFinal(t *testing.T, raw []byte) normalizedResponse {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid Responses response: %q", raw)
	}
	response := normalizedResponse{
		StopReason:        "completed",
		TerminalCount:     1,
		StopMetadataCount: 1,
	}
	for _, item := range gjson.GetBytes(raw, "output").Array() {
		switch item.Get("type").String() {
		case "function_call":
			response.Calls = append(
				response.Calls, normalizedCall{
					ID:        item.Get("call_id").String(),
					Name:      item.Get("name").String(),
					Arguments: item.Get("arguments").String(),
				},
			)
		case "message":
			response.Text += item.Get("content.0.text").String()
		}
	}
	return response
}

func normalizeAnthropicStream(t *testing.T, chunks [][]byte) normalizedResponse {
	t.Helper()
	response := normalizedResponse{}
	calls := make(map[int]*normalizedCall)
	for _, chunk := range chunks {
		for _, payload := range sseDataPayloads(t, chunk) {
			root := gjson.ParseBytes(payload)
			switch root.Get("type").String() {
			case "content_block_start":
				index := int(root.Get("index").Int())
				switch root.Get("content_block.type").String() {
				case "tool_use":
					calls[index] = &normalizedCall{
						ID:   root.Get("content_block.id").String(),
						Name: root.Get("content_block.name").String(),
					}
				case "text":
					response.Text += root.Get("content_block.text").String()
				}
			case "content_block_delta":
				index := int(root.Get("index").Int())
				switch root.Get("delta.type").String() {
				case "input_json_delta":
					if calls[index] == nil {
						t.Fatalf("Anthropic arguments delta targets missing index %d", index)
					}
					calls[index].Arguments += root.Get("delta.partial_json").String()
				case "text_delta":
					response.Text += root.Get("delta.text").String()
				}
			case "message_delta":
				response.StopMetadataCount++
				response.StopReason = root.Get("delta.stop_reason").String()
			case "message_stop":
				response.TerminalCount++
			}
		}
	}
	for index := 0; index < len(calls); index++ {
		if calls[index] == nil {
			t.Fatalf("missing Anthropic tool call index %d", index)
		}
		response.Calls = append(response.Calls, *calls[index])
	}
	return response
}

func normalizeAnthropicFinal(t *testing.T, raw []byte) normalizedResponse {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid Anthropic response: %q", raw)
	}
	response := normalizedResponse{
		StopReason:        gjson.GetBytes(raw, "stop_reason").String(),
		TerminalCount:     1,
		StopMetadataCount: 1,
	}
	for _, block := range gjson.GetBytes(raw, "content").Array() {
		switch block.Get("type").String() {
		case "tool_use":
			response.Calls = append(
				response.Calls, normalizedCall{
					ID:        block.Get("id").String(),
					Name:      block.Get("name").String(),
					Arguments: block.Get("input").Raw,
				},
			)
		case "text":
			response.Text += block.Get("text").String()
		}
	}
	return response
}

func sseDataPayloads(t *testing.T, chunk []byte) [][]byte {
	t.Helper()
	var payloads [][]byte
	for block := range strings.SplitSeq(strings.ReplaceAll(string(chunk), "\r\n", "\n"), "\n\n") {
		for line := range strings.SplitSeq(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if len(payload) == 0 || string(payload) == "[DONE]" {
				continue
			}
			if !gjson.ValidBytes(payload) {
				t.Fatalf("invalid SSE data payload: %q", payload)
			}
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

func assertToolOnlyResponse(
	t *testing.T,
	response normalizedResponse,
	expected []toolFixture,
	stopReason string,
) {
	t.Helper()
	if response.Text != "" {
		t.Fatalf("tool-only response contains text %q", response.Text)
	}
	if response.StopReason != stopReason {
		t.Fatalf("stop reason = %q, want %q", response.StopReason, stopReason)
	}
	if response.TerminalCount != 1 || response.StopMetadataCount != 1 {
		t.Fatalf("terminal count=%d stop metadata=%d, want 1/1", response.TerminalCount, response.StopMetadataCount)
	}
	if len(response.Calls) != len(expected) {
		t.Fatalf("calls = %#v, want %d", response.Calls, len(expected))
	}
	for index, want := range expected {
		expectedCall := normalizedCall{ID: want.ID, Name: want.Name, Arguments: want.Arguments}
		if response.Calls[index] != expectedCall {
			t.Fatalf("call %d = %#v, want %#v", index, response.Calls[index], expectedCall)
		}
	}
}
