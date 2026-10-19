package claude

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToGeminiCleansToolSchema(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{
				"name": "1 search/tool",
				"description": "Search",
				"strict": true,
				"input_examples": [{"query": "example"}],
				"type": "custom",
				"cache_control": {"type": "ephemeral"},
				"defer_loading": true,
				"eager_input_streaming": true,
				"input_schema": {
					"type": "object",
					"title": "SearchInput",
					"properties": {
						"query": {"type": "string", "nullable": true, "title": "Query"}
					}
				}
			}
		]
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", inputJSON, false)

	declaration := gjson.GetBytes(output, "tools.0.functionDeclarations.0")
	if got := declaration.Get("name").String(); got != "_1_search_tool" {
		t.Fatalf("tool name = %q, want _1_search_tool: %s", got, output)
	}
	for _, path := range []string{
		"strict",
		"input_examples",
		"type",
		"cache_control",
		"defer_loading",
		"eager_input_streaming",
	} {
		if declaration.Get(path).Exists() {
			t.Fatalf("%s should be removed: %s", path, output)
		}
	}
	if gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema.title").Exists() {
		t.Fatalf("schema title should be removed: %s", output)
	}
	if gjson.GetBytes(
		output, "tools.0.functionDeclarations.0.parametersJsonSchema.properties.query.nullable",
	).Exists() {
		t.Fatalf("schema nullable should be removed: %s", output)
	}
}

func TestConvertClaudeRequestToGemini_ToolChoice_SpecificTool(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hi"}
				]
			}
		],
		"tools": [
			{
				"name": "1 json/tool",
				"description": "A JSON tool",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		],
		"tool_choice": {"type": "tool", "name": "1 json/tool"}
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "toolConfig.functionCallingConfig.mode").String(); got != "ANY" {
		t.Fatalf("Expected toolConfig.functionCallingConfig.mode 'ANY', got '%s'", got)
	}
	allowed := gjson.GetBytes(output, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "_1_json_tool" {
		t.Fatalf(
			"Expected allowedFunctionNames ['_1_json_tool'], got %s",
			gjson.GetBytes(output, "toolConfig.functionCallingConfig.allowedFunctionNames").Raw,
		)
	}
}

func TestConvertClaudeRequestToGemini_StringSystemInstruction(t *testing.T) {
	input := []byte(`{
		"system": "Be concise",
		"messages": [{"role": "user", "content": "Hello"}]
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", input, false)
	if got := gjson.GetBytes(output, "systemInstruction.parts.0.text").String(); got != "Be concise" {
		t.Fatalf("systemInstruction text = %q, want Be concise: %s", got, output)
	}
	if gjson.GetBytes(output, "system_instruction").Exists() {
		t.Fatalf("legacy system_instruction should not be emitted: %s", output)
	}
}

func TestConvertClaudeRequestToGemini_ArraySystemInstruction(t *testing.T) {
	input := []byte(`{
		"system": [
			{"type": "text", "text": "Follow policy"},
			{"type": "text", "text": "Be concise"}
		],
		"messages": [{"role": "user", "content": "Hello"}]
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", input, false)
	systemInstruction := gjson.GetBytes(output, "systemInstruction")
	if got := systemInstruction.Get("role").String(); got != "user" {
		t.Fatalf("systemInstruction.role = %q, want user: %s", got, output)
	}
	parts := systemInstruction.Get("parts").Array()
	if len(parts) != 2 {
		t.Fatalf("systemInstruction parts = %d, want 2: %s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "Follow policy" {
		t.Fatalf("first system instruction = %q, want Follow policy", got)
	}
	if got := parts[1].Get("text").String(); got != "Be concise" {
		t.Fatalf("second system instruction = %q, want Be concise", got)
	}
	if gjson.GetBytes(output, "system_instruction").Exists() {
		t.Fatalf("legacy system_instruction should not be emitted: %s", output)
	}
}

func TestConvertClaudeRequestToGemini_RemovesTrailingUnansweredFunctionCall(t *testing.T) {
	inputJSON := []byte(`{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": {"query": "x"}}
			]}
		]
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", inputJSON, false)

	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("contents length = %d, want 1, output = %s", len(contents), output)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("contents.0.role = %q, want user", got)
	}
}

func TestConvertClaudeRequestToGemini_ImageContent(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "describe this image"},
					{
						"type": "image",
						"source": {
							"type": "base64",
							"media_type": "image/png",
							"data": "aGVsbG8="
						}
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "describe this image" {
		t.Fatalf("Expected first part text 'describe this image', got '%s'", got)
	}
	if got := parts[1].Get("inline_data.mime_type").String(); got != "image/png" {
		t.Fatalf("Expected image mime type 'image/png', got '%s'", got)
	}
	if got := parts[1].Get("inline_data.data").String(); got != "aGVsbG8=" {
		t.Fatalf("Expected image data 'aGVsbG8=', got '%s'", got)
	}
}

func TestConvertClaudeRequestToGemini_SkipsEmptyTextParts(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": ""},
					{"type": "text", "text": "hello"},
					{"type": "text", "text": ""}
				]
			}
		]
	}`)

	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part after skipping empty text, got %d: %s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "hello" {
		t.Fatalf("Expected part text 'hello', got '%s'", got)
	}
}

func TestConvertClaudeRequestToGemini_StrictToolPreservesJSONSchemaConstraints(t *testing.T) {
	for _, choice := range []struct {
		name  string
		field string
	}{
		{"auto", `,"tool_choice":{"type":"auto"}`},
		{"null", `,"tool_choice":null`},
		{"missing", ""},
	} {
		t.Run(choice.name, func(t *testing.T) {
			input := []byte(`{
				"messages":[{"role":"user","content":"query"}],
				"tools":[{"name":"lookup","strict":true,"input_schema":{
					"type":"object","additionalProperties":false,
					"properties":{"key":{"type":"string","minLength":2,"pattern":"^[a-z]+$"}}
				}}]` + choice.field + `}`)
			output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", input, false)
			if got := gjson.GetBytes(output, "toolConfig.functionCallingConfig.mode").String(); got != "VALIDATED" {
				t.Fatalf("strict tool choice = %q, want VALIDATED: %s", got, output)
			}
			schema := gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema")
			if got := schema.Get("additionalProperties"); got.Type != gjson.False {
				t.Fatalf("additionalProperties = %s, want false: %s", got.Raw, output)
			}
			if got := schema.Get("properties.key.minLength").Int(); got != 2 {
				t.Fatalf("minLength = %d, want 2: %s", got, output)
			}
			if got := schema.Get("properties.key.pattern").String(); got != "^[a-z]+$" {
				t.Fatalf("pattern = %q, want ^[a-z]+$: %s", got, output)
			}
		})
	}
}

func TestConvertClaudeRequestToGemini_ToolResultWithJSONRef(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"key":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":{"$ref":"#/definitions/Item","status":"ok"}}]}
		]
	}`)
	output := convertClaudeRequestToGeminiDefault("gemini-3-flash-preview", input, false)
	result := gjson.GetBytes(output, "contents.1.parts.0.functionResponse.response.result")
	if result.Type != gjson.String || result.String() != `{"$ref":"#/definitions/Item","status":"ok"}` {
		t.Fatalf("JSON Schema reference result = %s, want opaque JSON text: %s", result.Raw, output)
	}
}

func TestConvertClaudeRequestToGemini_AlignsPermutedParallelToolResultsWithMixedText(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gemini-3.7-flash-high",
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/tmp/1"}},
				{"type":"tool_use","id":"call_2","name":"Read","input":{"file_path":"/tmp/2"}},
				{"type":"tool_use","id":"call_3","name":"Read","input":{"file_path":"/tmp/3"}}
			]},
			{"role":"user","content":[
				{"type":"text","text":"Results arrived."},
				{"type":"tool_result","tool_use_id":"call_3","content":"three"},
				{"type":"tool_result","tool_use_id":"call_1","content":"one"},
				{"type":"tool_result","tool_use_id":"call_2","content":"two"},
				{"type":"text","text":"Continue."}
			]}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3.7-flash-high", inputJSON, false, false)
	callParts := gjson.GetBytes(output, "contents.0.parts").Array()
	responseParts := gjson.GetBytes(output, "contents.1.parts").Array()
	if len(callParts) != 3 || len(responseParts) != 5 {
		t.Fatalf("translated parts = %d calls and %d response-turn parts; output=%s", len(callParts), len(responseParts), output)
	}
	for index, wantID := range []string{"call_1", "call_2", "call_3"} {
		if gotID := callParts[index].Get("functionCall.id").String(); gotID != wantID {
			t.Fatalf("functionCall[%d].id = %q, want %q; output=%s", index, gotID, wantID, output)
		}
	}
	if got := responseParts[0].Get("text").String(); got != "Results arrived." {
		t.Fatalf("leading text = %q; output=%s", got, output)
	}
	if got := responseParts[1].Get("text").String(); got != "Continue." {
		t.Fatalf("trailing text reordered before functionResponse = %q; output=%s", got, output)
	}
	for index, wantID := range []string{"call_1", "call_2", "call_3"} {
		responsePart := responseParts[index+2]
		if gotID := responsePart.Get("functionResponse.id").String(); gotID != wantID {
			t.Fatalf("functionResponse[%d].id = %q, want %q; output=%s", index, gotID, wantID, output)
		}
		if gotName := responsePart.Get("functionResponse.name").String(); gotName != "Read" {
			t.Fatalf("functionResponse[%d].name = %q, want Read; output=%s", index, gotName, output)
		}
	}
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(output); errPairing != nil {
		t.Fatalf("translated parallel tool history is invalid: %v; output=%s", errPairing, output)
	}
}

func TestConvertClaudeRequestToGemini_ContainerUploadKeepsOtherText(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "keep me"},
			{"type": "container_upload", "file_id": "file-example"}
		]}]
	}`)
	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)
	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "keep me" {
		t.Fatalf("text was dropped with the file: %s", output)
	}
}

func TestConvertClaudeRequestToGemini_ConvertsMessageSystemRoleToUserContent(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"system": [{"type": "text", "text": "Top-level rules"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]},
			{"role": "system", "content": "String mid-conversation rule"},
			{"role": "system", "content": [{"type": "text", "text": "Array mid-conversation rule"}]}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)

	if systemContent := gjson.GetBytes(output, `contents.#(role=="system")`); systemContent.Exists() {
		t.Fatalf("system role should not be emitted in contents: %s", systemContent.Raw)
	}

	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("Expected consecutive user and message-level system turns to be merged into a single user turn, got %d: %s", len(contents), gjson.GetBytes(output, "contents").Raw)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected first content role user, got %q", got)
	}
	parts := contents[0].Get("parts").Array()
	if len(parts) != 3 {
		t.Fatalf("Expected 3 parts in merged user content, got %d: %s", len(parts), contents[0].Get("parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "Hello" {
		t.Fatalf("Unexpected initial user prompt text: %q", got)
	}
	if got := parts[1].Get("text").String(); got != "<system-reminder>\nString mid-conversation rule\n</system-reminder>" {
		t.Fatalf("Unexpected string message-level system content text: %q", got)
	}
	if got := parts[2].Get("text").String(); got != "<system-reminder>\nArray mid-conversation rule\n</system-reminder>" {
		t.Fatalf("Unexpected array message-level system content text: %q", got)
	}

	systemInstructionParts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	if len(systemInstructionParts) != 1 {
		t.Fatalf("Expected only top-level system parts, got %d: %s", len(systemInstructionParts), gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
	if got := systemInstructionParts[0].Get("text").String(); got != "Top-level rules" {
		t.Fatalf("Unexpected first system part: %q", got)
	}
}

func TestConvertClaudeRequestToGemini_FunctionResponseJSONRef(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3.8-flash",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "toolu_schema_1", "name": "get_schema", "input": {}}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "toolu_schema_1",
						"content": {
							"schema": {
								"$ref": "#/components/schemas/ErrorModel"
							}
						}
					}
				]
			}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3.8-flash", inputJSON, false, false)
	result := gjson.GetBytes(output, "contents.1.parts.0.functionResponse.response.result")
	if result.Type != gjson.String {
		t.Fatalf("expected functionResponse.response.result to be string, got %s (raw: %s)", result.Type, result.Raw)
	}
	if !strings.Contains(result.String(), "#/components/schemas/ErrorModel") {
		t.Fatalf("expected result to contain ref target, got %q", result.String())
	}
}

func TestConvertClaudeRequestToGemini_ImageURLAndRedactedThinkingStaySkipped(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "keep me"},
			{"type": "image", "source": {"type": "url", "url": "https://example.test/a.png"}},
			{"type": "redacted_thinking", "data": "abc"}
		]}]
	}`)
	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)
	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "keep me" {
		t.Fatalf("old skip paths changed the turn: %s", output)
	}
}

func TestConvertClaudeRequestToGemini_Issue5960_DocumentPreservation(t *testing.T) {
	// 用户消息中的 base64 PDF
	pdfInput := []byte(`{
		"model": "gemini-2.5-pro",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "document",
						"source": {
							"type": "base64",
							"media_type": "application/pdf",
							"data": "JVBERi0xLjQK"
						}
					}
				]
			}
		]
	}`)
	pdfOut, _ := convertClaudeRequestToGeminiChecked("gemini-2.5-pro", pdfInput, false, false)
	pdfParts := gjson.GetBytes(pdfOut, "contents.0.parts").Array()
	if len(pdfParts) == 0 {
		t.Fatalf("expected non-empty parts for base64 document, got: %s", string(pdfOut))
	}
	if gotMime := gjson.GetBytes(pdfOut, "contents.0.parts.0.inline_data.mime_type").String(); gotMime != "application/pdf" {
		t.Fatalf("expected inline_data.mime_type application/pdf, got %q (output: %s)", gotMime, string(pdfOut))
	}
	if gotData := gjson.GetBytes(pdfOut, "contents.0.parts.0.inline_data.data").String(); gotData != "JVBERi0xLjQK" {
		t.Fatalf("expected inline_data.data JVBERi0xLjQK, got %q (output: %s)", gotData, string(pdfOut))
	}

	// 文本和 PDF 按原顺序保留
	mixedInput := []byte(`{
		"model": "gemini-2.5-pro",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Please summarize this document:"},
					{
						"type": "document",
						"source": {
							"type": "base64",
							"media_type": "application/pdf",
							"data": "JVBERi0xLjQK"
						}
					}
				]
			}
		]
	}`)
	mixedOut, _ := convertClaudeRequestToGeminiChecked("gemini-2.5-pro", mixedInput, false, false)
	mixedParts := gjson.GetBytes(mixedOut, "contents.0.parts").Array()
	if len(mixedParts) != 2 {
		t.Fatalf("expected 2 parts for mixed text + document, got %d (output: %s)", len(mixedParts), string(mixedOut))
	}
	if mixedParts[0].Get("text").String() != "Please summarize this document:" {
		t.Fatalf("expected first part to be text, got %s", mixedParts[0].Raw)
	}
	if mixedParts[1].Get("inline_data.mime_type").String() != "application/pdf" {
		t.Fatalf("expected second part to be inline_data PDF, got %s", mixedParts[1].Raw)
	}

	// 多个 PDF 与文本交替排列
	interleavedInput := []byte(`{
		"model": "gemini-2.5-pro",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "document",
						"source": {
							"type": "base64",
							"media_type": "application/pdf",
							"data": "PDF_ONE"
						}
					},
					{"type": "text", "text": "compare with"},
					{
						"type": "document",
						"source": {
							"type": "base64",
							"media_type": "application/pdf",
							"data": "PDF_TWO"
						}
					}
				]
			}
		]
	}`)
	interleavedOut, _ := convertClaudeRequestToGeminiChecked("gemini-2.5-pro", interleavedInput, false, false)
	interleavedParts := gjson.GetBytes(interleavedOut, "contents.0.parts").Array()
	if len(interleavedParts) != 3 {
		t.Fatalf("expected 3 parts for interleaved documents, got %d (output: %s)", len(interleavedParts), string(interleavedOut))
	}
	if interleavedParts[0].Get("inline_data.data").String() != "PDF_ONE" || interleavedParts[2].Get("inline_data.data").String() != "PDF_TWO" {
		t.Fatalf("expected PDF data to be preserved in order, got: %s", string(interleavedOut))
	}

	// 全部内容无法转换时不生成空消息
	unsupportedInput := []byte(`{
		"model": "gemini-2.5-pro",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "unsupported_unknown_block", "foo": "bar"}
				]
			}
		]
	}`)
	unsupportedOut, _ := convertClaudeRequestToGeminiChecked("gemini-2.5-pro", unsupportedInput, false, false)
	contents := gjson.GetBytes(unsupportedOut, "contents").Array()
	if len(contents) != 0 {
		t.Fatalf("expected 0 contents turns instead of empty parts turn, got: %s", string(unsupportedOut))
	}
}

func TestConvertClaudeRequestToGemini_MessageLevelDeveloperInstructionsBecomeMergedUserReminder(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"system": "Top-level rules",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]},
			{"role": "developer", "content": "String mid-conversation developer rule"},
			{"role": "developer", "content": [{"type": "text", "text": "Array mid-conversation developer rule"}]}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)

	if devContent := gjson.GetBytes(output, `contents.#(role=="developer")`); devContent.Exists() {
		t.Fatalf("developer role should not be emitted in contents: %s", devContent.Raw)
	}

	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("Expected consecutive user and developer turns to be merged into a single user turn, got %d: %s", len(contents), gjson.GetBytes(output, "contents").Raw)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected first content role user, got %q", got)
	}
	parts := contents[0].Get("parts").Array()
	if len(parts) != 3 {
		t.Fatalf("Expected 3 parts in merged user content, got %d: %s", len(parts), contents[0].Get("parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "Hello" {
		t.Fatalf("Unexpected initial user prompt text: %q", got)
	}
	if got := parts[1].Get("text").String(); got != "<system-reminder>\nString mid-conversation developer rule\n</system-reminder>" {
		t.Fatalf("Unexpected string developer content text: %q", got)
	}
	if got := parts[2].Get("text").String(); got != "<system-reminder>\nArray mid-conversation developer rule\n</system-reminder>" {
		t.Fatalf("Unexpected array developer content text: %q", got)
	}

	systemInstructionParts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	if len(systemInstructionParts) != 1 {
		t.Fatalf("Expected only top-level system parts, got %d: %s", len(systemInstructionParts), gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
	if got := systemInstructionParts[0].Get("text").String(); got != "Top-level rules" {
		t.Fatalf("Unexpected first system part: %q", got)
	}
}

func TestConvertClaudeRequestToGemini_ParametersJsonSchema_PreservesAdditionalPropertiesAndPattern_Issue5959(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Use the submit tool."}],
		"tools": [
			{
				"name": "submit",
				"description": "Submit a bounded schema test value.",
				"input_schema": {
					"$schema": "https://json-schema.org/draft/2020-12/schema",
					"type": "object",
					"additionalProperties": false,
					"properties": {
						"recipient": {
							"type": "string",
							"pattern": "^(alice|bob)$"
						},
						"amount": {
							"type": "number"
						}
					},
					"required": ["recipient", "amount"]
				}
			}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-2.5-flash", inputJSON, false, false)
	schema := gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema")
	if !schema.Exists() {
		t.Fatalf("parametersJsonSchema missing. Output: %s", output)
	}
	if got := schema.Get("additionalProperties"); !got.Exists() || got.Type != gjson.False {
		t.Fatalf("additionalProperties should be preserved as false, got: %v. Schema: %s", got, schema.Raw)
	}
	if got := schema.Get("properties.recipient.pattern"); !got.Exists() || got.String() != "^(alice|bob)$" {
		t.Fatalf("pattern should be preserved, got: %v. Schema: %s", got, schema.Raw)
	}
	if schema.Get("description").Exists() && schema.Get("description").String() == "No extra properties allowed" {
		t.Fatalf("additionalProperties: false should not be converted to description hint. Schema: %s", schema.Raw)
	}
	if got := schema.Get("properties.recipient.description"); got.Exists() && strings.Contains(got.String(), "pattern:") {
		t.Fatalf("pattern should not be converted to description hint. Schema: %s", schema.Raw)
	}
}

func TestConvertClaudeRequestToGemini_PreservesToolPairingWithInterveningSystemMessage(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Run two tools"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "toolu_1", "name": "tool_one", "input": {"a": 1}},
					{"type": "tool_use", "id": "toolu_2", "name": "tool_two", "input": {"b": 2}}
				]
			},
			{
				"role": "system",
				"content": "Context reminder between tool_use and tool_result"
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "toolu_2", "content": "result 2"},
					{"type": "tool_result", "tool_use_id": "toolu_1", "content": "result 1"}
				]
			}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)

	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(output); errPairing != nil {
		t.Fatalf("ValidateGeminiFunctionCallPairing failed: %v\noutput: %s", errPairing, output)
	}

	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("expected 3 merged contents (user, model, user), got %d: %s", len(contents), output)
	}

	// 第一条消息为 user
	if contents[0].Get("role").String() != "user" {
		t.Fatalf("expected turn 0 role user, got %s", contents[0].Get("role").String())
	}
	// 第二条消息为 model，包含两个工具调用
	if contents[1].Get("role").String() != "model" {
		t.Fatalf("expected turn 1 role model, got %s", contents[1].Get("role").String())
	}
	// 第三条消息包含提醒文本及两个按调用顺序排列的工具结果
	if contents[2].Get("role").String() != "user" {
		t.Fatalf("expected turn 2 role user, got %s", contents[2].Get("role").String())
	}

	// 工具结果按 toolu_1、toolu_2 的调用顺序排列
	parts := contents[2].Get("parts").Array()
	var respIDs []string
	for _, p := range parts {
		if id := p.Get("functionResponse.id").String(); id != "" {
			respIDs = append(respIDs, id)
		}
	}
	if len(respIDs) != 2 || respIDs[0] != "toolu_1" || respIDs[1] != "toolu_2" {
		t.Fatalf("expected responses ordered [toolu_1, toolu_2], got %v", respIDs)
	}
}

func TestConvertClaudeRequestToGemini_StringToolResult(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "json-call-1", "name": "json", "input": {"ok": true}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "json-call-1", "content": "alpha"}
				]
			}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)

	fr := gjson.GetBytes(output, "contents.1.parts.0.functionResponse")
	if !fr.Exists() {
		t.Fatalf("expected functionResponse part, contents=%s", gjson.GetBytes(output, "contents").Raw)
	}
	// 字符串结果不重复编码
	if got := fr.Get("response.result").String(); got != "alpha" {
		t.Fatalf("expected result 'alpha', got '%s' (raw=%s)", got, fr.Get("response.result").Raw)
	}
}

func TestConvertClaudeRequestToGemini_StripsClaudeCodeAttribution(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},
			{"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK."},
			{"type": "text", "text": "User system prompt"}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)

	parts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 system parts after attribution strip, got %d: %s", len(parts), gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "You are a Claude agent, built on Anthropic's Claude Agent SDK." {
		t.Fatalf("Unexpected first system part: %q", got)
	}
	if got := parts[1].Get("text").String(); got != "User system prompt" {
		t.Fatalf("Unexpected second system part: %q", got)
	}
	if gjson.GetBytes(output, `systemInstruction.parts.#(text%"x-anthropic-billing-header:*")`).Exists() {
		t.Fatalf("Claude Code attribution block was forwarded: %s", gjson.GetBytes(output, "systemInstruction.parts").Raw)
	}
}

func TestConvertClaudeRequestToGemini_StructuredToolResult(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "json-call-1", "name": "json", "input": {"ok": true}}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "json-call-1",
						"content": [
							{"type": "text", "text": "alpha"},
							{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
						]
					}
				]
			}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3-flash-preview", inputJSON, false, false)

	fr := gjson.GetBytes(output, "contents.1.parts.0.functionResponse")
	if !fr.Exists() {
		t.Fatalf("expected functionResponse part, contents=%s", gjson.GetBytes(output, "contents").Raw)
	}
	// 文本结果保留为 JSON 对象
	if got := fr.Get("response.result.text").String(); got != "alpha" {
		t.Fatalf("expected structured result text 'alpha', got result=%s", fr.Get("response.result").Raw)
	}
	// 图片通过独立 inline_data part 传递
	img := gjson.GetBytes(output, "contents.1.parts.1.inline_data")
	if got := img.Get("mime_type").String(); got != "image/png" {
		t.Fatalf("expected image mime type 'image/png', got '%s'", got)
	}
	if got := img.Get("data").String(); got != "aGVsbG8=" {
		t.Fatalf("expected image data 'aGVsbG8=', got '%s'", got)
	}
}

func TestConvertClaudeRequestToGemini_ToolResultWithTrailingSystemReminderReordersParts(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3.8-flash",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Read the file"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "toolu_01_read", "name": "Read", "input": {"path": "main.go"}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "toolu_01_read", "content": "package main"},
					{"type": "text", "text": "<system-reminder>\n<total_tokens>1234</total_tokens>\n</system-reminder>"}
				]
			}
		]
	}`)

	output, _ := convertClaudeRequestToGeminiChecked("gemini-3.8-flash", inputJSON, false, false)

	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("expected 3 contents turns, got %d: %s", len(contents), output)
	}

	userParts := contents[2].Get("parts").Array()
	if len(userParts) != 2 {
		t.Fatalf("expected 2 parts in user response turn, got %d: %s", len(userParts), contents[2].Raw)
	}

	// 文本位于 functionResponse 之前，避免 Vertex AI 拒绝消息顺序
	if !userParts[0].Get("text").Exists() {
		t.Fatalf("expected parts[0] to be text part, got: %s", userParts[0].Raw)
	}
	if gotText := userParts[0].Get("text").String(); gotText != "<system-reminder>\n<total_tokens>1234</total_tokens>\n</system-reminder>" {
		t.Fatalf("unexpected text in parts[0]: %q", gotText)
	}
	if !userParts[1].Get("functionResponse").Exists() {
		t.Fatalf("expected parts[1] to be functionResponse part, got: %s", userParts[1].Raw)
	}
	if gotID := userParts[1].Get("functionResponse.id").String(); gotID != "toolu_01_read" {
		t.Fatalf("unexpected functionResponse.id in parts[1]: %q", gotID)
	}
}

func TestConvertClaudeRequestToGemini_ToolStrictMapsToValidatedMode(t *testing.T) {
	tests := []struct {
		name         string
		toolChoice   string
		expectedMode string
		allowedNames []string
	}{
		{
			name:         "absent tool_choice maps to VALIDATED",
			toolChoice:   "",
			expectedMode: "VALIDATED",
		},
		{
			name:         "explicit auto tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": {"type": "auto"},`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "null tool_choice maps to VALIDATED",
			toolChoice:   `"tool_choice": null,`,
			expectedMode: "VALIDATED",
		},
		{
			name:         "none tool_choice maps to NONE",
			toolChoice:   `"tool_choice": {"type": "none"},`,
			expectedMode: "NONE",
		},
		{
			name:         "any tool_choice maps to ANY",
			toolChoice:   `"tool_choice": {"type": "any"},`,
			expectedMode: "ANY",
		},
		{
			name:         "specific tool maps to ANY with allowedFunctionNames",
			toolChoice:   `"tool_choice": {"type": "tool", "name": "tool_a"},`,
			expectedMode: "ANY",
			allowedNames: []string{"tool_a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := fmt.Sprintf(`{
				"model": "gemini-3.8-flash",
				"messages": [{"role": "user", "content": "hi"}],
				%s
				"tools": [
					{
						"name": "tool_a",
						"description": "Controlled tool.",
						"strict": true,
						"input_schema": {"type": "object", "properties": {}}
					}
				]
			}`, tt.toolChoice)

			result, _ := convertClaudeRequestToGeminiChecked("gemini-3.8-flash", []byte(inputJSON), false, false)
			if gjson.GetBytes(result, "tools.0.functionDeclarations.0.strict").Exists() {
				t.Fatalf("strict must be removed from functionDeclarations: %s", result)
			}
			mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
			if mode != tt.expectedMode {
				t.Fatalf("expected toolConfig.functionCallingConfig.mode = %q, got %q. Output: %s", tt.expectedMode, mode, result)
			}
			if len(tt.allowedNames) > 0 {
				allowed := gjson.GetBytes(result, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
				if len(allowed) != len(tt.allowedNames) {
					t.Fatalf("expected %d allowedFunctionNames, got %d", len(tt.allowedNames), len(allowed))
				}
				for i, name := range tt.allowedNames {
					if allowed[i].String() != name {
						t.Fatalf("allowedFunctionNames[%d] = %q, want %q", i, allowed[i].String(), name)
					}
				}
			}
		})
	}

	t.Run("mixed tools where one is strict maps to VALIDATED", func(t *testing.T) {
		inputJSON := []byte(`{
			"model": "gemini-3.8-flash",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"name": "tool_a",
					"description": "Loose tool.",
					"strict": false,
					"input_schema": {"type": "object", "properties": {}}
				},
				{
					"name": "tool_b",
					"description": "Strict tool.",
					"strict": true,
					"input_schema": {"type": "object", "properties": {}}
				}
			]
		}`)
		result, _ := convertClaudeRequestToGeminiChecked("gemini-3.8-flash", inputJSON, false, false)
		mode := gjson.GetBytes(result, "toolConfig.functionCallingConfig.mode").String()
		if mode != "VALIDATED" {
			t.Fatalf("expected mode = 'VALIDATED' for mixed tools, got %q", mode)
		}
	})

	t.Run("non-strict tools omit toolConfig mode", func(t *testing.T) {
		inputJSON := []byte(`{
			"model": "gemini-3.8-flash",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [
				{
					"name": "tool_a",
					"description": "Loose tool.",
					"strict": false,
					"input_schema": {"type": "object", "properties": {}}
				},
				{
					"name": "tool_b",
					"description": "Unspecified tool.",
					"input_schema": {"type": "object", "properties": {}}
				}
			]
		}`)
		result, _ := convertClaudeRequestToGeminiChecked("gemini-3.8-flash", inputJSON, false, false)
		if gjson.GetBytes(result, "toolConfig").Exists() {
			t.Fatalf("expected toolConfig not to be set when no strict tools and no tool_choice, got: %s", result)
		}
	})
}

func TestConvertClaudeRequestToGemini_UnsendableFileNamesTheDroppedPart(t *testing.T) {
	for partType, block := range map[string]string{
		"container_upload": `{"type": "container_upload", "file_id": "file-example"}`,
		"document":         `{"type": "document", "source": {"type": "file", "file_id": "file-example"}}`,
	} {
		inputJSON := []byte(`{
			"model": "gemini-3-flash-preview",
			"messages": [{"role": "user", "content": [` + block + `]}]
		}`)
		output, err := ConvertClaudeRequestToGeminiWithCompatReturningError("gemini-3-flash-preview", inputJSON, false)
		if err == nil {
			t.Fatalf("%s: expected unsupported part error, output=%s", partType, output)
		}
		if got, want := err.Error(), "unsupported content part: "+partType; got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
		var unsupported *translatorcommon.UnsupportedPartError
		if !errors.As(err, &unsupported) || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() {
			t.Fatalf("%s: error = %#v", partType, err)
		}
	}
}
