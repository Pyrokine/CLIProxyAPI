package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToGeminiMapsMaxTokens(t *testing.T) {
	input := []byte(`{"model":"gemini-3-flash","max_tokens":321,"messages":[{"role":"user","content":"hi"}]}`)

	out := convertOpenAIRequestToGemini("gemini-3-flash", input, false)

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 321 {
		t.Fatalf("maxOutputTokens = %d, want 321; payload=%s", got, out)
	}
}

func TestConvertOpenAIRequestToGeminiMapsMaxCompletionTokens(t *testing.T) {
	input := []byte(`{"model":"gemini-3-flash","max_completion_tokens":654,"messages":[{"role":"user","content":"hi"}]}`)

	out := convertOpenAIRequestToGemini("gemini-3-flash", input, false)

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 654 {
		t.Fatalf("maxOutputTokens = %d, want 654; payload=%s", got, out)
	}
}

func TestConvertOpenAIRequestToGeminiPreservesVideoURL(t *testing.T) {
	input := []byte(`{
		"model":"gemini-3-flash",
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}},
					{"type":"text","text":"Describe the video"}
				]
			}
		]
	}`)

	out := convertOpenAIRequestToGemini("gemini-3-flash", input, false)

	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.mime_type").String(); got != "video/mp4" {
		t.Fatalf("video mime = %q, want video/mp4; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.data").String(); got != "AAAAIGZ0eXBtcDQy" {
		t.Fatalf("video data = %q, want base64 payload; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.1.text").String(); got != "Describe the video" {
		t.Fatalf("text part = %q, want prompt text; payload=%s", got, out)
	}
}

func TestConvertOpenAIRequestToGeminiNormalizesFunctionDeclarations(t *testing.T) {
	input := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"1 search/tool",
					"strict":true,
					"parameters":{
						"type":"object",
						"title":"SearchInput",
						"properties":{"query":{"type":"string","nullable":true}}
					}
				}
			},
			{"google_search":{"search_types":["web"]}},
			{"code_execution":{}},
			{"url_context":{}}
		]
	}`)

	out := convertOpenAIRequestToGemini("gemini-3-flash", input, false)
	declaration := gjson.GetBytes(out, "tools.0.functionDeclarations.0")
	if got := declaration.Get("name").String(); got != "_1_search_tool" {
		t.Fatalf("tool name = %q, want _1_search_tool: %s", got, out)
	}
	if declaration.Get("strict").Exists() {
		t.Fatalf("strict should be removed: %s", out)
	}
	if declaration.Get("parametersJsonSchema.title").Exists() ||
		declaration.Get("parametersJsonSchema.properties.query.nullable").Exists() {
		t.Fatalf("tool schema should be cleaned: %s", out)
	}
	if !gjson.GetBytes(out, "tools.1.googleSearch").Exists() ||
		!gjson.GetBytes(out, "tools.2.codeExecution").Exists() ||
		!gjson.GetBytes(out, "tools.3.urlContext").Exists() {
		t.Fatalf("native tools should be preserved: %s", out)
	}
}

func TestConvertOpenAIRequestToGeminiNormalizesToolNameAndStrict(t *testing.T) {
	input := []byte(`{"messages":[],"tools":[{"type":"function","function":{"name":true,"strict":true,"parameters":{"type":"object"}}}]}`)

	output := convertOpenAIRequestToGemini("gemini-test", input, false)

	name := gjson.GetBytes(output, "tools.0.functionDeclarations.0.name")
	if name.Type != gjson.String || name.String() != "true" {
		t.Fatalf("tool name = %s, want string true", name.Raw)
	}
	if gjson.GetBytes(output, "tools.0.functionDeclarations.0.strict").Exists() {
		t.Fatal("strict should be removed")
	}
}

func TestConvertOpenAIRequestToGeminiPreservesReasoningContent(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "reasoning_content": "thinking only"},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := convertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents length = %d, want 3. Output: %s", len(contents), result)
	}
	part := contents[1].Get("parts.0")
	if got := contents[1].Get("role").String(); got != "model" {
		t.Fatalf("contents.1.role = %q, want model. Output: %s", got, result)
	}
	if got := part.Get("text").String(); got != "thinking only" {
		t.Fatalf("reasoning text = %q, want thinking only. Output: %s", got, result)
	}
	if !part.Get("thought").Bool() {
		t.Fatalf("reasoning part should be marked as thought. Output: %s", result)
	}
	if got := part.Get("thoughtSignature").String(); got != geminiFunctionThoughtSignature {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, result)
	}
}

func TestConvertOpenAIRequestToGeminiPreservesReasoningBeforeVisibleContentAndToolCall(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "visible answer", "reasoning_content": "thinking only", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "{\"output\":\"ok\"}"},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := convertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4. Output: %s", len(contents), result)
	}
	parts := contents[1].Get("parts").Array()
	if len(parts) != 3 {
		t.Fatalf("model parts length = %d, want 3. Output: %s", len(parts), result)
	}
	if got := parts[0].Get("text").String(); got != "thinking only" || !parts[0].Get("thought").Bool() {
		t.Fatalf("first part should be the reasoning thought. Output: %s", result)
	}
	if got := parts[1].Get("text").String(); got != "visible answer" || parts[1].Get("thought").Bool() {
		t.Fatalf("second part should be visible assistant content. Output: %s", result)
	}
	if got := parts[2].Get("functionCall.name").String(); got != "read_file" {
		t.Fatalf("functionCall.name = %q, want read_file. Output: %s", got, result)
	}
	if got := parts[2].Get("thoughtSignature").String(); got != geminiFunctionThoughtSignature {
		t.Fatalf("functionCall thoughtSignature = %q, want bypass sentinel. Output: %s", got, result)
	}
	if got := contents[2].Get("parts.0.functionResponse.name").String(); got != "read_file" {
		t.Fatalf("functionResponse.name = %q, want read_file. Output: %s", got, result)
	}
}

func TestConvertOpenAIRequestToGeminiSkipsEmptyAssistantMessages(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "tool_calls": [{"type": "function", "function": {"name": "", "arguments": "{}"}}, {"type": "custom"}]},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := convertOpenAIRequestToGemini("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "contents").Array()
	if len(contents) != 2 {
		t.Fatalf("contents length = %d, want 2. Output: %s", len(contents), result)
	}
}

func TestConvertOpenAIRequestToGeminiMapsJSONSchemaResponseFormat(t *testing.T) {
	input := []byte(`{
		"generationConfig": {
			"temperature": 0.2,
			"responseSchema": {"type": "string"},
			"responseJsonSchema": {"type": "number"}
		},
		"messages": [{"role": "user", "content": "Return structured JSON."}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "response",
				"strict": true,
				"schema": {
					"type": "object",
					"properties": {"cleanedContent": {"type": "string"}},
					"required": ["cleanedContent"],
					"additionalProperties": false
				}
			}
		}
	}`)

	output := convertOpenAIRequestToGemini("gemini-3.1-flash-lite", input, false)
	generationConfig := gjson.GetBytes(output, "generationConfig")
	if got := generationConfig.Get("responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json: %s", got, output)
	}
	if generationConfig.Get("responseSchema").Exists() {
		t.Fatalf("responseSchema should be removed: %s", output)
	}
	schema := generationConfig.Get("responseJsonSchema")
	if got := schema.Get("type").String(); got != "object" {
		t.Fatalf("responseJsonSchema.type = %q, want object: %s", got, output)
	}
	if additionalProperties := schema.Get("additionalProperties"); !additionalProperties.Exists() || additionalProperties.Bool() {
		t.Fatalf("additionalProperties = %s, want false: %s", additionalProperties.Raw, output)
	}
	if got := generationConfig.Get("temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2: %s", got, output)
	}
}

func TestConvertOpenAIRequestToGeminiMapsJSONObjectResponseFormat(t *testing.T) {
	input := []byte(`{
		"generationConfig": {
			"temperature": 0.6,
			"responseSchema": {"type": "string"},
			"responseJsonSchema": {"type": "number"}
		},
		"messages": [{"role": "user", "content": "Return a JSON object."}],
		"response_format": {"type": "json_object"}
	}`)

	output := convertOpenAIRequestToGemini("gemini-3.1-flash-lite", input, false)
	generationConfig := gjson.GetBytes(output, "generationConfig")
	if got := generationConfig.Get("responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json: %s", got, output)
	}
	if generationConfig.Get("responseSchema").Exists() ||
		generationConfig.Get("responseJsonSchema").Exists() {
		t.Fatalf("json_object should not keep a response schema: %s", output)
	}
	if got := generationConfig.Get("temperature").Float(); got != 0.6 {
		t.Fatalf("temperature = %v, want 0.6: %s", got, output)
	}
}

func TestConvertOpenAIRequestToGeminiRejectsInvalidJSONSchemaResponseFormat(t *testing.T) {
	tests := []struct {
		name         string
		responseJSON string
	}{
		{name: "missing schema", responseJSON: `{"name":"response"}`},
		{name: "string schema", responseJSON: `{"schema":"invalid"}`},
		{name: "array schema", responseJSON: `{"schema":[]}`},
		{name: "null schema", responseJSON: `{"schema":null}`},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				input := []byte(`{
				"generationConfig": {
					"responseSchema": {"type": "string"},
					"responseJsonSchema": {"type": "number"}
				},
				"messages": [{"role": "user", "content": "Return structured JSON."}],
				"response_format": {"type": "json_schema", "json_schema": ` + tt.responseJSON + `}
			}`)

				output := convertOpenAIRequestToGemini("gemini-3.1-flash-lite", input, false)
				generationConfig := gjson.GetBytes(output, "generationConfig")
				if got := generationConfig.Get("responseMimeType").String(); got != "application/json" {
					t.Fatalf("responseMimeType = %q, want application/json: %s", got, output)
				}
				if generationConfig.Get("responseSchema").Exists() ||
					generationConfig.Get("responseJsonSchema").Exists() {
					t.Fatalf("invalid json_schema should not keep a response schema: %s", output)
				}
			},
		)
	}
}

func TestConvertOpenAIRequestToGeminiResponseFormatNoOp(t *testing.T) {
	tests := []struct {
		name           string
		responseFormat string
	}{
		{name: "absent"},
		{name: "text", responseFormat: `,"response_format":{"type":"text"}`},
		{name: "unknown", responseFormat: `,"response_format":{"type":"yaml"}`},
		{name: "non-object", responseFormat: `,"response_format":"json_object"`},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				input := []byte(`{
				"generationConfig": {
					"temperature": 0.5,
					"responseMimeType": "text/plain",
					"responseJsonSchema": {"type": "string"}
				},
				"messages": [{"role": "user", "content": "plain text"}]
			` + tt.responseFormat + `}`)

				output := convertOpenAIRequestToGemini("gemini-3.1-flash-lite", input, false)
				generationConfig := gjson.GetBytes(output, "generationConfig")
				if got := generationConfig.Get("responseMimeType").String(); got != "text/plain" {
					t.Fatalf("responseMimeType = %q, want text/plain: %s", got, output)
				}
				if got := generationConfig.Get("responseJsonSchema.type").String(); got != "string" {
					t.Fatalf("responseJsonSchema.type = %q, want string: %s", got, output)
				}
				if got := generationConfig.Get("temperature").Float(); got != 0.5 {
					t.Fatalf("temperature = %v, want 0.5: %s", got, output)
				}
			},
		)
	}
}
