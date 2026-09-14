package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToAntigravitySeparatesSummaryFromEffort(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantSummary bool
	}{
		{
			name:        "chat effort keeps compatibility summary",
			input:       `{"reasoning_effort":"high"}`,
			wantSummary: true,
		},
		{
			name:  "explicit false overrides effort",
			input: `{"reasoning_effort":"high","extra_body":{"google":{"thinking_config":{"include_thoughts":false}}}}`,
		},
		{
			name:  "string false is ignored",
			input: `{"extra_body":{"google":{"thinking_config":{"include_thoughts":"false"}}}}`,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(test.input), false)
				summary := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts")
				if test.name == "string false is ignored" {
					if summary.Exists() {
						t.Fatalf("non-boolean selector created summary visibility: %s", out)
					}
					return
				}
				if !summary.Exists() || summary.Bool() != test.wantSummary {
					t.Fatalf("includeThoughts = %s, want %v; output=%s", summary.Raw, test.wantSummary, out)
				}
			},
		)
	}
}

func TestConvertOpenAIRequestToAntigravityDeduplicatesAndDisambiguatesTools(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	inputJSON := `{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + second + `","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"{}"}
		],
		"tools":[
			{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"lookup","description":"duplicate","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"` + first + `","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"` + second + `","parameters":{"type":"object"}}}
		],
		"tool_choice":{"type":"function","function":{"name":"` + second + `"}}
	}`

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	declarations := gjson.GetBytes(out, "request.tools.0.functionDeclarations").Array()
	if len(declarations) != 3 {
		t.Fatalf("declaration count = %d, want 3. Output: %s", len(declarations), out)
	}
	firstMapped := declarations[1].Get("name").String()
	secondMapped := declarations[2].Get("name").String()
	if firstMapped == secondMapped || len(secondMapped) > 64 {
		t.Fatalf("collision names = %q and %q, want distinct names <= 64 chars", firstMapped, secondMapped)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.functionCall.name").String(); got != secondMapped {
		t.Fatalf("functionCall.name = %q, want %q. Output: %s", got, secondMapped, out)
	}
	if got := gjson.GetBytes(out, "request.contents.1.parts.0.functionResponse.name").String(); got != secondMapped {
		t.Fatalf("functionResponse.name = %q, want %q. Output: %s", got, secondMapped, out)
	}
	if got := gjson.GetBytes(
		out, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
	).String(); got != secondMapped {
		t.Fatalf("allowedFunctionNames.0 = %q, want %q. Output: %s", got, secondMapped, out)
	}
}

func TestNormalizeAntigravityOpenAIThinkingConfigReusesCanonicalConfig(t *testing.T) {
	input := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"high","thinkingBudget":8192}}}}`)

	output := normalizeAntigravityOpenAIThinkingConfig(input)

	if &output[0] != &input[0] {
		t.Fatal("canonical thinking config caused a payload copy")
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesReasoningContent(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "reasoning_content": "thinking only"},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "request.contents").Array()
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
	if got := part.Get("thoughtSignature").String(); got != geminiCLIFunctionThoughtSignature {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, result)
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesReasoningBeforeVisibleContentAndToolCall(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "visible answer", "reasoning_content": "thinking only", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "{\"output\":\"ok\"}"},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "request.contents").Array()
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
	if got := parts[2].Get("thoughtSignature").String(); got != geminiCLIFunctionThoughtSignature {
		t.Fatalf("functionCall thoughtSignature = %q, want bypass sentinel. Output: %s", got, result)
	}
	if got := contents[2].Get("parts.0.functionResponse.name").String(); got != "read_file" {
		t.Fatalf("functionResponse.name = %q, want read_file. Output: %s", got, result)
	}
}

func TestConvertOpenAIRequestToAntigravitySkipsEmptyAssistantMessages(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "tool_calls": [{"type": "function", "function": {"name": "", "arguments": "{}"}}, {"type": "custom"}]},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "request.contents").Array()
	if len(contents) != 2 {
		t.Fatalf("contents length = %d, want 2. Output: %s", len(contents), result)
	}
}

func TestConvertOpenAIRequestToAntigravity_ClaudeModelSanitizesUnsignedReasoningContent(t *testing.T) {
	inputJSON := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "visible text", "reasoning_content": "unsigned reasoning"},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("claude-sonnet-4-6", []byte(inputJSON), false)
	contents := gjson.GetBytes(result, "request.contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents length = %d, want 3. Output: %s", len(contents), result)
	}
	parts := contents[1].Get("parts").Array()
	if len(parts) != 1 {
		t.Fatalf("model parts length = %d, want 1 (thinking part dropped). Output: %s", len(parts), result)
	}
	if got := parts[0].Get("text").String(); got != "visible text" {
		t.Fatalf("parts[0].text = %q, want visible text. Output: %s", got, result)
	}
	if parts[0].Get("thought").Exists() {
		t.Fatalf("parts[0] should not be thought part. Output: %s", result)
	}
}

func TestConvertOpenAIRequestToAntigravity_ClaudeModelDropsEmptyAssistantTurnAfterSanitizingReasoningContent(t *testing.T) {
	inputJSON := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "reasoning_content": "unsigned reasoning"},
			{"role": "user", "content": "say ok"}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("claude-sonnet-4-6", []byte(inputJSON), false)
	contents := gjson.GetBytes(result, "request.contents").Array()
	if len(contents) != 2 {
		t.Fatalf("contents length = %d, want 2 (empty model turn dropped). Output: %s", len(contents), result)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("contents[0].role = %q, want user. Output: %s", got, result)
	}
	if got := contents[1].Get("role").String(); got != "user" {
		t.Fatalf("contents[1].role = %q, want user. Output: %s", got, result)
	}
}

func TestConvertOpenAIRequestToAntigravityMapsResponseFormatJSONObject(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"user","content":"hi"}],
		"generationConfig":{
			"responseSchema":{"type":"string","description":"stale"},
			"responseJsonSchema":{"type":"string"},
			"response_schema":{"type":"string"},
			"response_json_schema":{"type":"string"}
		},
		"response_format":{"type":"json_object"}
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3.6-flash-high", inputJSON, false)
	if got := gjson.GetBytes(out, "request.generationConfig.responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json. Output: %s", got, out)
	}
	if gjson.GetBytes(out, "request.generationConfig.responseSchema").Exists() {
		t.Fatalf("responseSchema should not be set for json_object. Output: %s", out)
	}
	assertNoAntigravityResponseSchemaAliases(t, out)
}

func TestConvertOpenAIRequestToAntigravityMapsResponseFormatJSONSchema(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"user","content":"hi"}],
		"generationConfig":{
			"responseSchema":{"type":"string","description":"stale"},
			"responseJsonSchema":{"type":"string"},
			"response_schema":{"type":"string"},
			"response_json_schema":{"type":"string"}
		},
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"verdict",
				"schema":{
					"type":"object",
					"properties":{"score":{"type":"integer"}},
					"required":["score"]
				}
			}
		}
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3.6-flash-high", inputJSON, false)
	if got := gjson.GetBytes(out, "request.generationConfig.responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json. Output: %s", got, out)
	}
	schema := gjson.GetBytes(out, "request.generationConfig.responseSchema")
	if got := schema.Get("properties.score.type").String(); got != "integer" {
		t.Fatalf("responseSchema.properties.score.type = %q, want integer. Output: %s", got, out)
	}
	if schema.Get("description").Exists() {
		t.Fatalf("stale responseSchema survived. Output: %s", out)
	}
	assertNoAntigravityResponseSchemaAliases(t, out)
}

func TestConvertOpenAIRequestToAntigravityRejectsInvalidJSONSchema(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"generationConfig":{"responseSchema":{"type":"string","description":"stale"}},
		"response_format":{"type":"json_schema","json_schema":{"schema":"invalid"}}
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3.6-flash-high", inputJSON, false)
	if !gjson.GetBytes(out, "request.generationConfig.responseMimeType").Exists() {
		t.Fatalf("JSON response MIME type missing. Output: %s", out)
	}
	if gjson.GetBytes(out, "request.generationConfig.responseSchema").Exists() {
		t.Fatalf("invalid or stale response schema survived. Output: %s", out)
	}
}

func assertNoAntigravityResponseSchemaAliases(t *testing.T, out []byte) {
	t.Helper()
	for _, schemaKey := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
		if gjson.GetBytes(out, "request.generationConfig."+schemaKey).Exists() {
			t.Errorf("stale %s survived response_format mapping. Output: %s", schemaKey, out)
		}
	}
}
