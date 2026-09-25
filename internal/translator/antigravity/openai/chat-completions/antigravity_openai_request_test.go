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
	if got := parts[2].Get("functionCall.name").String(); got != "external_read_file" {
		t.Fatalf("functionCall.name = %q, want external_read_file. Output: %s", got, result)
	}
	if got := parts[2].Get("thoughtSignature").String(); got != geminiCLIFunctionThoughtSignature {
		t.Fatalf("functionCall thoughtSignature = %q, want bypass sentinel. Output: %s", got, result)
	}
	if got := contents[2].Get("parts.0.functionResponse.name").String(); got != "external_read_file" {
		t.Fatalf("functionResponse.name = %q, want external_read_file. Output: %s", got, result)
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

func TestConvertOpenAIRequestToAntigravityTokenFallbackAndToolChoiceNone(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want int64
	}{
		{"completion only", `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":200}`, 200},
		{"max tokens wins", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_completion_tokens":200}`, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := ConvertOpenAIRequestToAntigravity("gemini-test", []byte(test.body), false)
			if got := gjson.GetBytes(out, "request.generationConfig.maxOutputTokens").Int(); got != test.want {
				t.Fatalf("maxOutputTokens = %d, want %d; output=%s", got, test.want, out)
			}
		})
	}

	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		body := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":` + choice + `}`)
		out := ConvertOpenAIRequestToAntigravity("gemini-test", body, false)
		if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String(); got != "NONE" {
			t.Fatalf("tool mode = %q, want NONE; output=%s", got, out)
		}
		if gjson.GetBytes(out, "request.tools").Exists() {
			t.Fatalf("request.tools must be omitted; output=%s", out)
		}
	}
}

func TestConvertOpenAIRequestToAntigravityTranslatesOnlyInlineBase64VideoURLs(t *testing.T) {
	validInput := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"inspect video"},
			{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}}
		]}]
	}`)
	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", validInput, false)
	parts := gjson.GetBytes(out, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("valid video parts count = %d, want 2; output=%s", len(parts), out)
	}
	if got := parts[1].Get("inlineData.mimeType").String(); got != "video/mp4" {
		t.Fatalf("video MIME = %q, want video/mp4; output=%s", got, out)
	}
	if got := parts[1].Get("inlineData.data").String(); got != "AAAAIGZ0eXBtcDQy" {
		t.Fatalf("video data = %q; output=%s", got, out)
	}

	invalidURLs := []string{
		"https://example.com/video.mp4",
		"data:image/png;base64,AAAA",
		"data:video/mp4;base64,",
		"data:video/mp4,AAAA",
		"",
	}
	for _, videoURL := range invalidURLs {
		input := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"keep"},{"type":"video_url","video_url":{"url":"` + videoURL + `"}}]}]}`)
		output := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
		invalidParts := gjson.GetBytes(output, "request.contents.0.parts").Array()
		if len(invalidParts) != 1 || invalidParts[0].Get("text").String() != "keep" {
			t.Fatalf("invalid video URL %q was accepted; output=%s", videoURL, output)
		}
	}
}

func TestConvertOpenAIRequestToAntigravityKeepsMidSessionDeveloperInUserBoundary(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":"global"},
			{"role":"user","content":"first user"},
			{"role":"assistant","content":"first answer"},
			{"role":"developer","content":"mid-session notice"},
			{"role":"user","content":"second user"}
		]
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
	if got := gjson.GetBytes(out, "request.systemInstruction.parts.0.text").String(); got != "global" {
		t.Fatalf("system instruction = %q, want global; output=%s", got, out)
	}
	if got := len(gjson.GetBytes(out, "request.systemInstruction.parts").Array()); got != 1 {
		t.Fatalf("system instruction part count = %d, want 1; output=%s", got, out)
	}
	contents := gjson.GetBytes(out, "request.contents").Array()
	if len(contents) != 3 {
		t.Fatalf("contents count = %d, want 3; output=%s", len(contents), out)
	}
	parts := contents[2].Get("parts").Array()
	if len(parts) != 2 || parts[0].Get("text").String() != "mid-session notice" || parts[1].Get("text").String() != "second user" {
		t.Fatalf("mid-session developer/user order changed; output=%s", out)
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesTypedToolResponses(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_string","type":"function","function":{"name":"string_tool","arguments":"{}"}},
				{"id":"call_object","type":"function","function":{"name":"object_tool","arguments":"{}"}},
				{"id":"call_array","type":"function","function":{"name":"array_tool","arguments":"{}"}},
				{"id":"call_null","type":"function","function":{"name":"null_tool","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_string","content":"{\"shape\":\"string\"}"},
			{"role":"tool","tool_call_id":"call_object","content":{"shape":"object"}},
			{"role":"tool","tool_call_id":"call_array","content":[1,{"shape":"array"}]},
			{"role":"tool","tool_call_id":"call_null","content":null}
		]
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
	parts := gjson.GetBytes(out, "request.contents.1.parts").Array()
	if len(parts) != 4 {
		t.Fatalf("tool response count = %d, want 4; output=%s", len(parts), out)
	}
	stringResult := parts[0].Get("functionResponse.response.result")
	if stringResult.Type != gjson.String || stringResult.String() != `{"shape":"string"}` {
		t.Fatalf("JSON-shaped string changed type or value: %s; output=%s", stringResult.Raw, out)
	}
	if got := parts[1].Get("functionResponse.response.result.shape").String(); got != "object" {
		t.Fatalf("object result = %q; output=%s", got, out)
	}
	arrayResult := parts[2].Get("functionResponse.response.result")
	if !arrayResult.IsArray() || len(arrayResult.Array()) != 2 || arrayResult.Get("1.shape").String() != "array" {
		t.Fatalf("array result changed: %s; output=%s", arrayResult.Raw, out)
	}
	if got := parts[3].Get("functionResponse.response.result"); got.Raw != "null" {
		t.Fatalf("null result = %s, want null; output=%s", got.Raw, out)
	}
}

func TestConvertOpenAIRequestToAntigravityCleansOnlyResponseFormatSchema(t *testing.T) {
	input := []byte(`{
		"messages":[{"role":"assistant","tool_calls":[{
			"id":"call_1","type":"function","function":{
				"name":"inspect","arguments":"{\"default\":\"keep\",\"title\":\"keep\",\"format\":\"keep\",\"const\":\"keep\"}"
			}
		}]}],
		"response_format":{"type":"json_schema","json_schema":{"schema":{
			"type":"object","properties":{
				"empty":{"type":"object","properties":{}},
				"values":{"type":"array"},
				"choice":{"oneOf":[{"type":"string"},{"type":"number"}]},
				"code":{"enum":[1,2]}
			}
		}}}
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
	schema := gjson.GetBytes(out, "request.generationConfig.responseSchema")
	if schema.Get("properties.empty.properties.reason").Exists() || schema.Get("properties.values.items").Exists() {
		t.Fatalf("response schema received tool-only defaults: %s", schema.Raw)
	}
	if !schema.Get("properties.choice.oneOf").IsArray() || schema.Get("properties.code.type").Exists() {
		t.Fatalf("response schema received tool-only union/enum rewrites: %s", schema.Raw)
	}
	arguments := gjson.GetBytes(out, "request.contents.0.parts.0.functionCall.args")
	for _, key := range []string{"default", "title", "format", "const"} {
		if got := arguments.Get(key).String(); got != "keep" {
			t.Fatalf("function argument %s = %q, want keep; output=%s", key, got, out)
		}
	}
}

func TestConvertOpenAIRequestToAntigravityMapsExternalToolNameChain(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{
				"id":"call_1","type":"function","function":{
					"name":"external_external_read_file","arguments":"{}"
				}
			}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{
			"name":"external_external_read_file","parameters":{"type":"object"}
		}}],
		"tool_choice":{"type":"function","function":{"name":"external_external_read_file"}}
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
	const expected = "external_external_external_read_file"
	for _, path := range []string{
		"request.tools.0.functionDeclarations.0.name",
		"request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
		"request.contents.0.parts.0.functionCall.name",
		"request.contents.1.parts.0.functionResponse.name",
	} {
		if got := gjson.GetBytes(out, path).String(); got != expected {
			t.Fatalf("%s = %q, want %q; output=%s", path, got, expected, out)
		}
	}
}
