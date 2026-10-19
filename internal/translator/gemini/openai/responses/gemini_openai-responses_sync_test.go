package responses

import (
	"context"
	"strings"
	"testing"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRegisteredGeminiResponsesBlankTextDoesNotHideDroppedAttachment(t *testing.T) {
	for _, text := range []string{`""`, `" \t\n "`, `"kept"`} {
		for _, stream := range []bool{false, true} {
			request := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + text + `},{"type":"input_file","file_id":"synthetic-file"}]}]}`)
			body, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatGemini, "gemini-3-flash", request, stream)
			if text == `"kept"` {
				if err != nil || gjson.GetBytes(body, "contents.0.parts.0.text").String() != "kept" {
					t.Fatalf("sendable text rejected: %s, err = %v", body, err)
				}
			} else if err == nil || body != nil {
				t.Fatalf("blank text hid an unsupported attachment: %s, err = %v", body, err)
			}
		}
	}
}

func TestResponsesSyncPreservesToolSchemasAndJSONReferences(t *testing.T) {
	input := []byte(`{
		"tools":[{"type":"function","name":"lookup","parameters":{
			"type":"object","additionalProperties":false,
			"properties":{"key":{"type":"string","pattern":"^ok$","minLength":2}}
		}}],
		"input":[
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":{"schema":{"$ref":"#/defs/Item"}}}
		]
	}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3-flash", input, false)
	if len(output) == 0 {
		t.Fatal("conversion rejected supported tool request")
	}
	schema := gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema")
	if got := schema.Get("additionalProperties"); got.Type != gjson.False {
		t.Fatalf("additionalProperties = %s, want false: %s", got.Raw, output)
	}
	if got := schema.Get("properties.key.pattern").String(); got != "^ok$" {
		t.Fatalf("pattern = %q, want ^ok$: %s", got, output)
	}
	if got := schema.Get("properties.key.minLength").Int(); got != 2 {
		t.Fatalf("minLength = %d, want 2: %s", got, output)
	}
	result := gjson.GetBytes(output, "contents.1.parts.0.functionResponse.response.result")
	if result.Type != gjson.String || result.String() != `{"schema":{"$ref":"#/defs/Item"}}` {
		t.Fatalf("JSON Schema reference result = %s, want opaque JSON text: %s", result.Raw, output)
	}
}

func TestResponsesSyncUnmatchedOutputIsUserText(t *testing.T) {
	input := []byte(`{"input":[
		{"type":"function_call_output","call_id":"unknown","output":"orphan tool data"}
	]}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3-flash", input, false)
	if got := gjson.GetBytes(output, "contents.0.parts.0.text").String(); got != "orphan tool data" {
		t.Fatalf("orphan text = %q: %s", got, output)
	}
	if strings.Contains(string(output), `"functionResponse":`) {
		t.Fatalf("orphan output must not claim a function call: %s", output)
	}
}

func TestResponsesSyncDemotesMidSessionDeveloperMessage(t *testing.T) {
	input := []byte(`{"instructions":"initial","input":[
		{"role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"reply"}]},
		{"role":"developer","content":[{"type":"input_text","text":"one"},{"type":"input_text","text":"two"}]},
		{"role":"user","content":[{"type":"input_text","text":"continue"}]}
	]}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3-flash", input, false)
	if got := gjson.GetBytes(output, "systemInstruction.parts.0.text").String(); got != "initial" {
		t.Fatalf("systemInstruction = %q, want initial: %s", got, output)
	}
	if got := gjson.GetBytes(output, "contents.2.parts.0.text").String(); got != "<system-reminder>\none\ntwo\n</system-reminder>" {
		t.Fatalf("developer reminder = %q: %s", got, output)
	}
	if got := gjson.GetBytes(output, "contents.2.parts.1.text").String(); got != "continue" {
		t.Fatalf("user text following developer reminder = %q: %s", got, output)
	}
}

func TestResponsesSyncMediaInputAndToolOutput(t *testing.T) {
	input := []byte(`{"input":[
		{"type":"input_video","video_url":{"url":"data:video/mp4;codecs=avc1;base64,aGVsbG8="}},
		{"type":"input_file","filename":"report.pdf","file_url":"https://files.example/report.pdf"},
		{"type":"input_audio","input_audio":{"format":"wav","data":"aGVsbG8="}},
		{"type":"input_text","text":"summarize"},
		{"type":"function_call","call_id":"call_1","name":"media","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":[
			{"type":"input_video","data":"aGVsbG8=","format":"mp4"},
			{"type":"input_file","file_url":"https://files.example/report.pdf","filename":"report.pdf"}
		]}
	]}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3-flash", input, false)
	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 4 {
		t.Fatalf("multimodal input parts = %d, want 4: %s", len(parts), output)
	}
	if got := parts[0].Get("inline_data.mime_type").String(); got != "video/mp4" {
		t.Fatalf("video MIME = %q, want video/mp4: %s", got, output)
	}
	if got := parts[1].Get("file_data.file_uri").String(); got != "https://files.example/report.pdf" {
		t.Fatalf("file URI = %q: %s", got, output)
	}
	if got := parts[2].Get("inline_data.mime_type").String(); got != "audio/wav" {
		t.Fatalf("audio MIME = %q, want audio/wav: %s", got, output)
	}
	result := gjson.GetBytes(output, "contents.2.parts.0.functionResponse.parts").Array()
	if len(result) != 2 || result[0].Get("inlineData.mimeType").String() != "video/mp4" ||
		result[1].Get("fileData.fileUri").String() != "https://files.example/report.pdf" {
		t.Fatalf("tool result media = %s, want video and file: %s", gjson.GetBytes(output, "contents.2").Raw, output)
	}
}

func TestResponsesSyncInvalidDataURLIsRejected(t *testing.T) {
	input := []byte(`{"input":[{"role":"user","content":[
		{"type":"input_video","video_url":"data:video/mp4;base64,???"},
		{"type":"input_text","text":"safe"}
	]}]}`)
	if output, err := convertOpenAIResponsesRequestToGemini("gemini-3-flash", input, false); err == nil || len(output) > 0 {
		t.Fatalf("invalid data URL must reject request: output = %s, err = %v", output, err)
	}
}

func TestResponsesSyncNativeWebSearchRequest(t *testing.T) {
	const model = "gemini-3.1-flash-image"
	if !modelSupportsResponsesWebSearch(model) {
		t.Fatalf("test model %s lacks registered static web-search capability", model)
	}
	input := []byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`)
	output := ConvertOpenAIResponsesRequestToGemini(model, input, false)
	if got := gjson.GetBytes(output, "tools.0.googleSearch.includedDomains.0").String(); got != "example.com" {
		t.Fatalf("search domain = %q, want example.com: %s", got, output)
	}
	if gjson.GetBytes(output, "toolConfig").Exists() {
		t.Fatalf("search-only request must not produce functionCallingConfig: %s", output)
	}
	for _, choice := range []string{`"none"`, `{"type":"function","name":"lookup"}`} {
		blocked := []byte(`{"input":"search","tools":[{"type":"web_search"}],"tool_choice":` + choice + `}`)
		response := ConvertOpenAIResponsesRequestToGemini(model, blocked, false)
		if gjson.GetBytes(response, "tools.0.googleSearch").Exists() {
			t.Fatalf("tool_choice %s did not block native search: %s", choice, response)
		}
	}
}

func TestResponsesSyncInvalidSearchDomainsAreRejected(t *testing.T) {
	for _, filter := range []string{
		`[]`,
		`["", "example.com"]`,
		`[null]`,
		`"example.com"`,
	} {
		input := []byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":` + filter + `}}]}`)
		if output, err := convertOpenAIResponsesRequestToGemini("gemini-3.1-flash-image", input, false); err == nil || len(output) > 0 {
			t.Errorf("allowed_domains %s must reject unrestricted search; output = %s, err = %v", filter, output, err)
		}
	}
}

func TestResponsesSyncConflictingSearchToolsAreRejected(t *testing.T) {
	input := []byte(`{"input":"search","tools":[{"type":"web_search"},{"type":"web_search_preview","filters":{"allowed_domains":["example.com"]}}]}`)
	if output, err := convertOpenAIResponsesRequestToGemini("gemini-3.1-flash-image", input, false); err == nil || len(output) > 0 {
		t.Fatalf("ambiguous search tools must not discard the allowed_domains restriction: output = %s, err = %v", output, err)
	}
}

func TestResponsesSyncNonStreamWebSearchOutput(t *testing.T) {
	request := []byte(`{"input":"hello","tools":[{"type":"web_search"}]}`)
	provider := []byte(`{"responseId":"123","candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"answer"}],"role":"model"},
		"groundingMetadata":{"webSearchQueries":["query"],"groundingChunks":[{"web":{"uri":"https://example.com/page","title":"Example"}}]}}]}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-3.1-flash-image", request, request, provider, nil)
	search := gjson.GetBytes(result, "output.0")
	if search.Get("type").String() != "web_search_call" || search.Get("action.query").String() != "query" ||
		search.Get("action.sources.0.url").String() != "https://example.com/page" {
		t.Fatalf("web_search_call must precede answer with sources: %s", result)
	}
	if got := gjson.GetBytes(result, "output.1.content.0.text").String(); got != "answer" {
		t.Fatalf("answer = %q, want answer: %s", got, result)
	}
	if got := gjson.GetBytes(result, "tool_usage.web_search.num_requests").Int(); got != 1 {
		t.Fatalf("web search usage = %d, want 1: %s", got, result)
	}
}

func TestResponsesSyncUnmatchedOutputClosesPendingCall(t *testing.T) {
	for _, callType := range []string{"function_call", "custom_tool_call"} {
		t.Run(callType, func(t *testing.T) {
			input := []byte(`{"input":[
				{"role":"user","content":"run bash"},
				{"type":"` + callType + `","call_id":"call_1","name":"Bash","arguments":"{}","input":"test"},
				{"type":"` + callType + `_output","call_id":"call_other","output":"other_result"}
			]}`)
			output, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatGemini, "gemini-3-flash", input, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := internalsignature.ValidateGeminiFunctionCallPairing(output); err != nil {
				t.Fatalf("pending call must be paired before orphan text: %v; output=%s", err, output)
			}
			responses, orphanTexts := 0, 0
			for _, content := range gjson.GetBytes(output, "contents").Array() {
				for _, part := range content.Get("parts").Array() {
					if response := part.Get("functionResponse"); response.Exists() {
						responses++
						if response.Get("id").String() != "call_1" || response.Get("name").String() != "Bash" ||
							response.Get("response.result").String() != "call interrupted, no output" {
							t.Fatalf("orphan output rebound to pending call: %s", output)
						}
					}
					if content.Get("role").String() == "user" && part.Get("text").String() == "other_result" {
						orphanTexts++
					}
				}
			}
			if responses != 1 || orphanTexts != 1 {
				t.Fatalf("response and orphan text counts = %d, %d; output=%s", responses, orphanTexts, output)
			}
		})
	}
}

func TestResponsesSyncGenericMediaMIMEUsesKindDefault(t *testing.T) {
	for _, tc := range []struct {
		kind string
		mime string
	}{
		{"image", "image/png"}, {"audio", "audio/wav"}, {"video", "video/mp4"}, {"file", "application/octet-stream"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, generic := range []string{"application/octet-stream", "binary/octet-stream"} {
				input := []byte(`{"input":[{"role":"user","content":[{"type":"input_` + tc.kind +
					`","` + tc.kind + `_url":"https://example.com/download.bin","mime_type":"` + generic + `"}]}]}`)
				output, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
					sdktranslator.FormatGemini, "gemini-3-flash", input, false)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(output, "contents.0.parts.0.file_data.mime_type").String(); got != tc.mime {
					t.Fatalf("generic %s MIME = %q, want %q: %s", tc.kind, got, tc.mime, output)
				}
			}
		})
	}
}

func TestResponsesSyncSourceDataURLValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"invalid-base64", " DATA:image/png;base64,!!!", ""},
		{"non-base64", "\tDATA:image/png,hello", ""},
		{"empty", " data:image/png;base64,", ""},
		{"valid-data-url", " DATA:image/png;base64,aGVsbG8= ", "aGVsbG8="},
		{"valid-base64", "aGVsbG8=", "aGVsbG8="},
	} {
		for _, location := range []string{"user", "tool"} {
			t.Run(tc.name+"/"+location, func(t *testing.T) {
				toolOutput := location == "tool"
				part := `{"type":"input_image","source":{"type":"base64","data":"` + strings.ReplaceAll(tc.data, "\t", `\t`) + `"}}`
				input := []byte(`{"input":[{"role":"user","content":[` + part + `]}]}`)
				if toolOutput {
					input = []byte(`{"input":[{"type":"function_call","call_id":"call_1","name":"image","arguments":"{}"},` +
						`{"type":"function_call_output","call_id":"call_1","output":[` + part + `]}]}`)
				}
				output, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
					sdktranslator.FormatGemini, "gemini-3-flash", input, false)
				if tc.want == "" {
					if err == nil || len(output) != 0 {
						t.Fatalf("invalid source data URL accepted: output=%s, err=%v", output, err)
					}
					if body := ConvertOpenAIResponsesRequestToGemini("gemini-3-flash", input, false); len(body) != 0 {
						t.Fatalf("byte API accepted invalid source data URL: %s", body)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				path := "contents.0.parts.0.inline_data.data"
				if toolOutput {
					path = "contents.1.parts.0.functionResponse.parts.0.inlineData.data"
				}
				if got := gjson.GetBytes(output, path).String(); got != tc.want {
					t.Fatalf("decoded source data = %q, want %q: %s", got, tc.want, output)
				}
			})
		}
	}
}

func TestResponsesSyncFormalHistoryPartOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{
			name: "InterveningDeveloperAndUserMessageFlushesInOrder",
			input: `{
		"model": "gemini-3.5-flash",
		"instructions": "Be a helpful assistant",
		"input": [
			{
				"type": "function_call",
				"call_id": "call-1",
				"name": "run_command",
				"arguments": "{\"command\":\"test\"}"
			},
			{
				"type": "message",
				"role": "developer",
				"content": "<permissions instructions>\nApproved: test\n</permissions instructions>"
			},
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Wait, also check this"}
				]
			},
			{
				"type": "function_call_output",
				"call_id": "call-1",
				"output": "done"
			}
		]
	}`,
			want: []string{"C:call-1", "T:<system-reminder>\n<permissions instructions>\nApproved: test\n</permissions instructions>\n</system-reminder>", "T:Wait, also check this", "R:call-1:done"},
		},
		{
			name: "InterruptedFunctionCallPreservesPairing",
			input: `{
		"model": "gemini-3.8-flash-high",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"List the files."}]},
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"ls\"]}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Stop, do something else instead."}]},
			{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":[\"pwd\"]}"},
			{"type":"function_call_output","call_id":"c2","output":"/tmp\n"}
		]
	}`,
			want: []string{"T:List the files.", "C:c1", "T:Stop, do something else instead.", "R:c1:call interrupted, no output", "C:c2", "R:c2:/tmp\n"},
		},
		{
			name: "ParallelInterruptedFunctionCallPreservesPairingAndOrder",
			input: `{
		"model": "gemini-3.8-flash-high",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"List and print."}]},
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"ls\"]}"},
			{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":[\"pwd\"]}"},
			{"type":"function_call_output","call_id":"c2","output":"/tmp\n"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Stop, do something else instead."}]},
			{"type":"function_call","call_id":"c3","name":"shell","arguments":"{\"command\":[\"whoami\"]}"},
			{"type":"function_call_output","call_id":"c3","output":"root\n"}
		]
	}`,
			want: []string{"T:List and print.", "C:c1", "C:c2", "T:Stop, do something else instead.", "R:c1:call interrupted, no output", "R:c2:/tmp\n", "C:c3", "R:c3:root\n"},
		},
		{
			name: "InterruptedMessageBeforeRealOutputPreservesPairingAndOrder",
			input: `{
		"model": "gemini-3.8-flash-high",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"List and print."}]},
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"ls\"]}"},
			{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":[\"pwd\"]}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Stop, do something else instead."}]},
			{"type":"function_call_output","call_id":"c2","output":"/tmp\n"}
		]
	}`,
			want: []string{"T:List and print.", "C:c1", "C:c2", "T:Stop, do something else instead.", "R:c1:call interrupted, no output", "R:c2:/tmp\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatGemini, gjson.Get(tc.input, "model").String(), []byte(tc.input), false)
			if err != nil {
				t.Fatal(err)
			}
			if err := internalsignature.ValidateGeminiFunctionCallPairing(output); err != nil {
				t.Fatalf("history pairing failed: %v; output=%s", err, output)
			}
			var events []string
			for _, content := range gjson.GetBytes(output, "contents").Array() {
				for _, part := range content.Get("parts").Array() {
					switch {
					case part.Get("functionCall").Exists():
						events = append(events, "C:"+part.Get("functionCall.id").String())
					case part.Get("functionResponse").Exists():
						events = append(events, "R:"+part.Get("functionResponse.id").String()+":"+part.Get("functionResponse.response.result").String())
					case part.Get("text").Exists():
						events = append(events, "T:"+part.Get("text").String())
					default:
						t.Fatalf("unexpected history part: %s", part.Raw)
					}
				}
			}
			if strings.Join(events, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("history part order = %q, want %q; output=%s", events, tc.want, output)
			}
		})
	}
}
