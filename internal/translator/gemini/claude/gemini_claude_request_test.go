package claude

import (
	"testing"

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
