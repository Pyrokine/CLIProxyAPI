package executor

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestParseDevinInteractionsPayloadPreservesHistoryAndTools(t *testing.T) {
	payload := []byte(`{
		"system_instruction":"You are a coding assistant.",
		"generation_config":{
			"temperature":0,
			"max_output_tokens":16000,
			"thinking_level":"high",
			"thinking_config":{"thinking_budget":32000}
		},
		"session_id":"session-1",
		"cascade_id":"cascade-1",
		"input":[
			{"type":"user_input","content":[{"type":"text","text":"hello"}]},
			{"type":"thought","text":"first"},
			{"type":"thought","text":"second","signature":"c2VhbGVkLnYxLnNpZw=="},
			{"type":"model_output","content":[{"type":"text","text":"answer"}]},
			{"type":"function_call","id":"call-1","name":"read_file","arguments":{"path":"main.go","task_id":"keep"}},
			{"type":"function_result","call_id":"call-1","result":{"content":"package main"}}
		],
		"tools":[{
			"name":"read_file",
			"description":"Read a file without rewriting task_id.",
			"parameters":{"type":"object","properties":{"task_id":{"type":"string"}}}
		}]
	}`)

	parsed := parseDevinInteractionsPayload(payload, nil)
	if parsed.systemPrompt != "You are a coding assistant." {
		t.Fatalf("system prompt = %q", parsed.systemPrompt)
	}
	if parsed.temperature == nil || *parsed.temperature != 0 {
		t.Fatalf("temperature = %v, want explicit zero", parsed.temperature)
	}
	if parsed.maxTokens != 16000 || parsed.thinkingLevel != "high" || parsed.budgetTokens != 32000 {
		t.Fatalf("generation settings = max %d level %q budget %d", parsed.maxTokens, parsed.thinkingLevel, parsed.budgetTokens)
	}
	if parsed.sessionID != "session-1" || parsed.cascadeID != "cascade-1" {
		t.Fatalf("session/cascade = %q/%q", parsed.sessionID, parsed.cascadeID)
	}
	if len(parsed.prompts) != 3 {
		t.Fatalf("prompts = %#v", parsed.prompts)
	}
	assistant := parsed.prompts[1]
	if assistant.Source != 2 || assistant.Thinking != "first\n\nsecond" || assistant.Content != "answer" {
		t.Fatalf("assistant prompt = %#v", assistant)
	}
	if string(assistant.Signature) != "sealed.v1.sig" || assistant.SignatureType != "sealed" {
		t.Fatalf("assistant signature = %q (%q)", assistant.Signature, assistant.SignatureType)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Arguments != `{"path":"main.go","task_id":"keep"}` {
		t.Fatalf("tool calls = %#v", assistant.ToolCalls)
	}
	result := parsed.prompts[2]
	if result.Source != 4 || result.ToolCallID != "call-1" || result.Content != `{"content":"package main"}` {
		t.Fatalf("tool result = %#v", result)
	}
	if len(parsed.tools) != 1 || parsed.tools[0].Name != "read_file" {
		t.Fatalf("tools = %#v", parsed.tools)
	}
	if parsed.tools[0].Description != "Read a file without rewriting task_id." ||
		!strings.Contains(string(parsed.tools[0].Parameters), `"task_id"`) {
		t.Fatalf("tool definition was rewritten: %#v", parsed.tools[0])
	}
}

func TestDevinInteractionResultPrefersCallIDOverUnrelatedStepID(t *testing.T) {
	payload := []byte(`{"input":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"read_file","arguments":{}},{"type":"function_result","id":"step-id","call_id":"call-1","result":"found"}]}`)
	parsed := parseDevinInteractionsPayload(payload, nil)
	if len(parsed.prompts) != 2 || len(parsed.prompts[0].ToolCalls) != 1 {
		t.Fatalf("tool prompts = %#v", parsed.prompts)
	}
	if parsed.prompts[0].ToolCalls[0].ID != "call-1" || parsed.prompts[1].ToolCallID != "call-1" || parsed.prompts[1].Content != "found" {
		t.Fatalf("tool call/result pairing = %#v", parsed.prompts)
	}
}

func TestDevinToolResultPairingFromTranslatedRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format sdktranslator.Format
		body   string
		id     string
		result string
	}{
		{
			name: "Claude", format: sdktranslator.FormatClaude, id: "toolu_abc_123", result: "file.txt",
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_abc_123","name":"bash","input":{"command":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc_123","content":"file.txt"}]}]}`,
		},
		{
			name: "OpenAI", format: sdktranslator.FormatOpenAI, id: "call_xyz_456", result: "package main",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_xyz_456","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},{"role":"tool","tool_call_id":"call_xyz_456","content":"package main"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			translated, err := sdktranslator.TranslateRequestChecked(context.Background(), tc.format, sdktranslator.FormatInteractions, "devin/swe-2", []byte(tc.body), false)
			if err != nil {
				t.Fatalf("translate tool history: %v", err)
			}
			prompts := parseDevinInteractionsPayload(translated, nil).prompts
			if len(prompts) != 2 || len(prompts[0].ToolCalls) != 1 || prompts[0].ToolCalls[0].ID != tc.id || prompts[1].ToolCallID != tc.id || prompts[1].Content != tc.result {
				t.Fatalf("translated tool call/result pairing = %#v, payload = %s", prompts, translated)
			}
		})
	}
}

func TestParseDevinInteractionsPayloadKeepsNonSystemTextUntouched(t *testing.T) {
	payload := []byte(`{
		"system_instruction":"remove SECRET line",
		"input":[
			{"type":"user_input","content":[{"type":"text","text":"ordinary SECRET history"}]},
			{"type":"function_call","id":"call-1","name":"tool","arguments":{"value":"SECRET"}}
		],
		"tools":[{"name":"tool","description":"SECRET description","parameters":{"const":"SECRET"}}]
	}`)

	parsed := parseDevinInteractionsPayload(payload, nil)
	if parsed.systemPrompt != "remove SECRET line" {
		t.Fatalf("system prompt changed before system-only sanitizer: %q", parsed.systemPrompt)
	}
	if parsed.prompts[0].Content != "ordinary SECRET history" {
		t.Fatalf("history changed: %q", parsed.prompts[0].Content)
	}
	if parsed.prompts[1].ToolCalls[0].Arguments != `{"value":"SECRET"}` {
		t.Fatalf("tool arguments changed: %q", parsed.prompts[1].ToolCalls[0].Arguments)
	}
	if parsed.tools[0].Description != "SECRET description" || string(parsed.tools[0].Parameters) != `{"const":"SECRET"}` {
		t.Fatalf("tool schema changed: %#v", parsed.tools[0])
	}
}

func TestParseDevinInteractionsPayloadImagesAndOriginalSignature(t *testing.T) {
	payload := []byte(`{
		"input":[
			{"type":"user_input","content":[
				{"type":"text","text":"inspect"},
				{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/test"}}
			]},
			{"type":"model_output","text":"done"}
		]
	}`)
	original := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"inspect"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"reasoning","signature":"Q0FRU3NpZw=="},
				{"type":"text","text":"done"}
			]}
		]
	}`)

	parsed := parseDevinInteractionsPayload(payload, original)
	if len(parsed.prompts) != 2 || len(parsed.prompts[0].Images) != 1 {
		t.Fatalf("prompts = %#v", parsed.prompts)
	}
	image := parsed.prompts[0].Images[0]
	if image.MimeType != "image/jpeg" || image.Base64Data != "/9j/test" {
		t.Fatalf("image = %#v", image)
	}
	if !strings.HasPrefix(parsed.prompts[0].Content, "[Image 1: pasted_image_1.jpg]\n\ninspect") {
		t.Fatalf("image header missing: %q", parsed.prompts[0].Content)
	}
	assistant := parsed.prompts[1]
	if assistant.Thinking != "reasoning" || string(assistant.Signature) != "CAQSsig" || assistant.SignatureType != "anthropic" {
		t.Fatalf("supplemented assistant = %#v", assistant)
	}
}

func TestParseDevinMessagesFallbackPreservesToolCallIdentity(t *testing.T) {
	payload := []byte(`{
		"messages":[
			{"role":"system","content":"system one"},
			{"role":"developer","content":"system two"},
			{"role":"user","content":"question"},
			{"role":"assistant","content":"answer","tool_calls":[{
				"id":"call-9",
				"function":{"name":"lookup","arguments":"{\"key\":\"value\"}"}
			}]},
			{"role":"tool","tool_call_id":"call-9","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object"}}}]
	}`)

	parsed := parseDevinInteractionsPayload(payload, nil)
	if parsed.systemPrompt != "system one\n\nsystem two" {
		t.Fatalf("system prompt = %q", parsed.systemPrompt)
	}
	if len(parsed.prompts) != 3 {
		t.Fatalf("prompts = %#v", parsed.prompts)
	}
	call := parsed.prompts[1].ToolCalls[0]
	if call.ID != "call-9" || call.Name != "lookup" || call.Arguments != `{"key":"value"}` {
		t.Fatalf("tool call = %#v", call)
	}
	if parsed.prompts[2].ToolCallID != "call-9" || parsed.prompts[2].Content != "result" {
		t.Fatalf("tool result = %#v", parsed.prompts[2])
	}
	if len(parsed.tools) != 1 || parsed.tools[0].Name != "lookup" || string(parsed.tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("tools = %#v", parsed.tools)
	}
}

func TestDevinDirectToolDeclarationsPreserveAliases(t *testing.T) {
	for _, tools := range []string{
		`[{"function_declarations":[{"name":"lookup","parameters":{"type":"object"}}]}]`,
		`[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"type":"object"}}]}]`,
		`[{"name":"lookup","parametersJsonSchema":{"type":"object"}}]`,
		`[{"function":{"name":"lookup","parametersJsonSchema":{"type":"object"}}}]`,
		`[{"type":"namespace","name":"mcp__codex_app","children":[{"name":"automation_update"},{"name":"lookup","parameters":{"type":"object"}}]}]`,
	} {
		parsed := parseDevinTools(gjson.Parse(tools))
		if len(parsed) != 1 || parsed[0].Name != "lookup" || string(parsed[0].Parameters) != `{"type":"object"}` {
			t.Errorf("tool declarations changed: %s -> %+v", tools, parsed)
		}
	}
}

func TestDevinToolResultProjectionPreservesBusinessValues(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`{"content":"package main"}`, `{"content":"package main"}`},
		{`{"content":{"file":"main.go"}}`, `{"content":{"file":"main.go"}}`},
		{`{"output":"permission denied","exit_code":1}`, `{"output":"permission denied","exit_code":1}`},
		{`{"type":"text","text":"business","exit_code":1}`, `{"type":"text","text":"business","exit_code":1}`},
		{`{"content":["first","second"]}`, `{"content":["first","second"]}`},
		{`[{"type":"text","text":"block"},"quoted"]`, "block\n\"quoted\""},
		{`{"content":{"type":"text","text":"wrapped"},"cache_control":{"type":"ephemeral"}}`, "wrapped"},
		{`{"output":{"type":"text","text":"wrapped"}}`, "wrapped"},
		{`[{"type":"tool_result","content":""}]`, ""},
		{`"   "`, "   "},
		{`""`, ""},
	} {
		got, images := devinToolResultContent(gjson.Parse(test.input))
		if got != test.want || len(images) != 0 {
			t.Errorf("result %s -> %q, want %q; images=%d", test.input, got, test.want, len(images))
		}
	}
}

func TestDevinOriginalToolResultImagesFollowCallIdentity(t *testing.T) {
	encoded := devinContractPNG(t, 9)
	payload := []byte(`{"input":[{"type":"function_call","id":"call_image","name":"screenshot","arguments":{}},{"type":"function_result","call_id":"call_image","result":"captured"}]}`)
	original := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_image","name":"screenshot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_image","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}}]}]}]}`)
	parsed := parseDevinInteractionsPayload(payload, original)
	if len(parsed.prompts) != 2 || parsed.prompts[1].ToolCallID != "call_image" || len(parsed.prompts[1].Images) != 1 || parsed.prompts[1].Images[0].Base64Data != encoded {
		t.Fatalf("original tool image association changed: %+v", parsed.prompts)
	}
}
