package chat_completions

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToGeminiStrictAndAllowedTools(t *testing.T) {
	const tool = `{"type":"function","function":{"name":"send/message","strict":true,"parameters":{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string","pattern":"^ok$","minLength":2}}}}}`
	tests := []struct {
		name          string
		choice        string
		extra         string
		wantMode      string
		wantName      string
		wantToolCount int
	}{
		{"default strict", "", "", "VALIDATED", "", 1},
		{"auto strict", `,"tool_choice":"auto"`, "", "VALIDATED", "", 1},
		{"null strict", `,"tool_choice":null`, "", "VALIDATED", "", 1},
		{"required", `,"tool_choice":"required"`, "", "ANY", "", 1},
		{"none", `,"tool_choice":"none"`, "", "NONE", "", 1},
		{"specific", `,"tool_choice":{"type":"function","function":{"name":"send/message"}}`, "", "ANY", "send_message", 1},
		{"unknown specific", `,"tool_choice":{"type":"function","function":{"name":"absent"}}`, "", "NONE", "", 1},
		{"unknown choice", `,"tool_choice":{"type":"other"}`, "", "NONE", "", 1},
		{"disable parallel", `,"tool_choice":"required"`, `,"parallel_tool_calls":false`, "NONE", "", 1},
		{"allowed subset", `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"send/message"}}]}}`, "", "ANY", "send_message", 1},
		{"empty allowed subset", `,"tool_choice":{"type":"allowed_tools","tools":[]}`, "", "NONE", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`{"messages":[{"role":"user","content":"send"}],"tools":[` + tool + `]` + tc.choice + tc.extra + `}`)
			output := convertOpenAIRequestToGemini("gemini-3-flash", input, false)
			if got := gjson.GetBytes(output, "toolConfig.functionCallingConfig.mode").String(); got != tc.wantMode {
				t.Fatalf("mode = %q, want %q: %s", got, tc.wantMode, output)
			}
			declarations := gjson.GetBytes(output, "tools.0.functionDeclarations").Array()
			if len(declarations) != tc.wantToolCount {
				t.Fatalf("declarations = %d, want %d: %s", len(declarations), tc.wantToolCount, output)
			}
			if tc.wantName != "" {
				allowed := gjson.GetBytes(output, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
				if len(allowed) != 1 || allowed[0].String() != tc.wantName {
					t.Fatalf("allowedFunctionNames = %v, want %q: %s", allowed, tc.wantName, output)
				}
			}
			if len(declarations) > 0 {
				schema := declarations[0].Get("parametersJsonSchema")
				if got := schema.Get("additionalProperties"); got.Type != gjson.False {
					t.Fatalf("additionalProperties = %s, want false: %s", got.Raw, output)
				}
				if got := schema.Get("properties.text.pattern").String(); got != "^ok$" {
					t.Fatalf("pattern = %q, want ^ok$: %s", got, output)
				}
				if got := schema.Get("properties.text.minLength").Int(); got != 2 {
					t.Fatalf("minLength = %d, want 2: %s", got, output)
				}
			}
		})
	}
}

func TestConvertOpenAIRequestToGeminiRejectsSanitizedNameCollision(t *testing.T) {
	input := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"send/message","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"send@message","parameters":{"type":"object"}}}
		],"tool_choice":"required"
	}`)
	output := convertOpenAIRequestToGemini("gemini-3-flash", input, false)
	if mode := gjson.GetBytes(output, "toolConfig.functionCallingConfig.mode").String(); mode != "NONE" {
		t.Fatalf("colliding names must disable function calling, got %q: %s", mode, output)
	}
}

func TestConvertOpenAIRequestToGeminiToolResultsAreScopedAndOpaque(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[
			{"type":"function","id":"call_1","function":{"name":"lookup","arguments":"{}"}},
			{"type":"function","id":"call_2","function":{"name":"search","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call_2","content":{"schema":{"$ref":"#/defs/Error"}}},
		{"role":"tool","tool_call_id":"call_1","content":"ok"},
		{"role":"assistant","tool_calls":[{"type":"function","id":"call_1","function":{"name":"lookup_again","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"second"}
	]}`)
	output := convertOpenAIRequestToGemini("gemini-3-flash", input, false)
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents = %d, want 4: %s", len(contents), output)
	}
	responses := contents[1].Get("parts").Array()
	if len(responses) != 2 || responses[0].Get("functionResponse.name").String() != "lookup" ||
		responses[1].Get("functionResponse.name").String() != "search" {
		t.Fatalf("responses must match their own model turn: %s", output)
	}
	if got := responses[0].Get("functionResponse.response.result").String(); got != "ok" {
		t.Fatalf("first response = %q, want ok: %s", got, output)
	}
	ref := responses[1].Get("functionResponse.response.result")
	if ref.Type != gjson.String || ref.String() != `{"schema":{"$ref":"#/defs/Error"}}` {
		t.Fatalf("JSON Schema reference result = %s, want opaque JSON text: %s", ref.Raw, output)
	}
	if got := contents[3].Get("parts.0.functionResponse.name").String(); got != "lookup_again" {
		t.Fatalf("reused ID must resolve within the current assistant turn, got %q: %s", got, output)
	}
	if got := contents[3].Get("parts.0.functionResponse.response.result").String(); got != "second" {
		t.Fatalf("second response = %q, want second: %s", got, output)
	}
}

func TestConvertOpenAIRequestToGeminiDoesNotInventIncompleteToolResults(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		wantText string
	}{
		{"missing", `[{"role":"assistant","tool_calls":[{"type":"function","id":"call_1","function":{"name":"lookup","arguments":"{}"}}]}]`, ""},
		{"orphan", `[{"role":"tool","tool_call_id":"orphan","content":"unmatched"}]`, "unmatched"},
		{"repeated", `[{"role":"assistant","tool_calls":[{"type":"function","id":"call_1","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"first"},{"role":"tool","tool_call_id":"call_1","content":"second"}]`, "second"},
		{"intervening user", `[{"role":"assistant","tool_calls":[{"type":"function","id":"call_1","function":{"name":"lookup","arguments":"{}"}}]},{"role":"user","content":"other"},{"role":"tool","tool_call_id":"call_1","content":"late"}]`, "late"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := convertOpenAIRequestToGemini("gemini-3-flash", []byte(`{"messages":`+tc.messages+`}`), false)
			if strings.Contains(string(output), `"functionResponse":`) {
				t.Fatalf("ambiguous or missing results must not become function responses: %s", output)
			}
			if tc.wantText != "" && !strings.Contains(string(output), tc.wantText) {
				t.Fatalf("unmatched tool content %q was lost: %s", tc.wantText, output)
			}
		})
	}
}

func TestConvertOpenAIRequestToGeminiMediaDoesNotClaimThoughtSignature(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}},
			{"type":"video_url","video_url":{"url":"data:video/mp4;base64,aGVsbG8="}}
		]},
		{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}
	]}`)
	output := convertOpenAIRequestToGemini("gemini-3-flash", input, false)
	if gjson.GetBytes(output, "contents.0.parts.0.thoughtSignature").Exists() ||
		gjson.GetBytes(output, "contents.0.parts.1.thoughtSignature").Exists() ||
		gjson.GetBytes(output, "contents.1.parts.0.thoughtSignature").Exists() {
		t.Fatalf("image and video parts must not include model thought signatures: %s", output)
	}
}

func TestConvertOpenAIRequestToGeminiDemotesMidConversationSystem(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"system","content":"initial"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":"later"}
	]}`)
	output := convertOpenAIRequestToGemini("gemini-3-flash", input, false)
	if got := gjson.GetBytes(output, "systemInstruction.parts.0.text").String(); got != "initial" {
		t.Fatalf("initial system instruction = %q, want initial: %s", got, output)
	}
	if gjson.GetBytes(output, "systemInstruction.parts.1").Exists() {
		t.Fatalf("mid-session developer message must not be hoisted: %s", output)
	}
	if got := gjson.GetBytes(output, "contents.1.parts.0.text").String(); got != "<system-reminder>\nlater\n</system-reminder>" {
		t.Fatalf("mid-session developer reminder = %q: %s", got, output)
	}
}
