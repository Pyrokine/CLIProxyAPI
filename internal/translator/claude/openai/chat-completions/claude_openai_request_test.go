package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaudeDropsTemperature(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"temperature": 0.2,
		"top_p": 0.8,
		"messages": [
			{"role": "user", "content": "hi"}
		]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if resultJSON.Get("temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if got := resultJSON.Get("top_p").Float(); got != 0.8 {
		t.Fatalf("top_p = %v, want 0.8", got)
	}
}

func TestConvertOpenAIRequestToClaude_ToolResultTextAndBase64Image(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": [
					{"type": "text", "text": "tool ok"},
					{
						"type": "image_url",
						"image_url": {
							"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
						}
					}
				]
			}
		]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolResult := messages[1].Get("content.0")
	if got := toolResult.Get("type").String(); got != "tool_result" {
		t.Fatalf("Expected content[0].type %q, got %q", "tool_result", got)
	}
	if got := toolResult.Get("tool_use_id").String(); got != "call_1" {
		t.Fatalf("Expected tool_use_id %q, got %q", "call_1", got)
	}

	toolContent := toolResult.Get("content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool_result content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "text" {
		t.Fatalf("Expected first tool_result part type %q, got %q", "text", got)
	}
	if got := toolContent.Get("0.text").String(); got != "tool ok" {
		t.Fatalf("Expected first tool_result part text %q, got %q", "tool ok", got)
	}
	if got := toolContent.Get("1.type").String(); got != "image" {
		t.Fatalf("Expected second tool_result part type %q, got %q", "image", got)
	}
	if got := toolContent.Get("1.source.type").String(); got != "base64" {
		t.Fatalf("Expected image source type %q, got %q", "base64", got)
	}
	if got := toolContent.Get("1.source.media_type").String(); got != "image/png" {
		t.Fatalf("Expected image media type %q, got %q", "image/png", got)
	}
	if got := toolContent.Get("1.source.data").String(); got != "iVBORw0KGgoAAAANSUhEUg==" {
		t.Fatalf("Unexpected base64 image data: %q", got)
	}
}

func TestConvertOpenAIRequestToClaudeMapsSystemToTopLevel(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "be concise"},
			{"role": "system", "content": [{"type":"text","text":"use json"}]},
			{"role": "user", "content": "hello"}
		]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("system.0.text").String(); got != "be concise" {
		t.Fatalf("system.0.text = %q, want be concise. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("system.1.text").String(); got != "use json" {
		t.Fatalf("system.1.text = %q, want use json. Output: %s", got, string(result))
	}
	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1. Output: %s", len(messages), string(result))
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("message role = %q, want user. Output: %s", got, string(result))
	}
}

func TestConvertOpenAIRequestToClaudeSanitizesToolIDs(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call.bad/id",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call.bad/id",
				"content": "tool ok"
			}
		]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("messages.0.content.0.id").String(); got != "call_bad_id" {
		t.Fatalf("tool_use id = %q, want call_bad_id. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("messages.1.content.0.tool_use_id").String(); got != "call_bad_id" {
		t.Fatalf("tool_result tool_use_id = %q, want call_bad_id. Output: %s", got, string(result))
	}
}

func TestConvertOpenAIRequestToClaude_ToolResultURLImageOnly(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": [
					{
						"type": "image_url",
						"image_url": {
							"url": "https://example.com/tool.png"
						}
					}
				]
			}
		]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolContent := messages[1].Get("content.0.content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool_result content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "image" {
		t.Fatalf("Expected tool_result part type %q, got %q", "image", got)
	}
	if got := toolContent.Get("0.source.type").String(); got != "url" {
		t.Fatalf("Expected image source type %q, got %q", "url", got)
	}
	if got := toolContent.Get("0.source.url").String(); got != "https://example.com/tool.png" {
		t.Fatalf("Unexpected image URL: %q", got)
	}
}

func TestConvertOpenAIRequestToClaudeGroupsConsecutiveAssistantMessages(t *testing.T) {
	inputJSON := `{
		"messages": [
			{
				"role": "assistant",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "first", "arguments": "{}"}
				}]
			},
			{"role": "assistant", "content": "answer"},
			{
				"role": "assistant",
				"tool_calls": [{
					"id": "call_2",
					"type": "function",
					"function": {"name": "second", "arguments": "{}"}
				}]
			}
		]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-5", []byte(inputJSON), false)
	messages := gjson.GetBytes(result, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1. Output: %s", len(messages), string(result))
	}
	content := messages[0].Get("content").Array()
	wantTypes := []string{"text", "tool_use", "tool_use"}
	if len(content) != len(wantTypes) {
		t.Fatalf("content count = %d, want %d. Output: %s", len(content), len(wantTypes), string(result))
	}
	for index, wantType := range wantTypes {
		if got := content[index].Get("type").String(); got != wantType {
			t.Fatalf("content[%d].type = %q, want %q", index, got, wantType)
		}
	}
	if got := content[1].Get("id").String(); got != "call_1" {
		t.Fatalf("first tool_use id = %q, want call_1", got)
	}
	if got := content[2].Get("id").String(); got != "call_2" {
		t.Fatalf("second tool_use id = %q, want call_2", got)
	}
}

func TestConvertOpenAIRequestToClaudeNormalizesRootToolSchemaUnion(t *testing.T) {
	inputJSON := `{
		"messages": [{"role": "user", "content": "hello"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "search",
				"parameters": {
					"anyOf": [
						{"type": "object", "properties": {"query": {"type": "string"}}},
						{"type": "object", "properties": {"limit": {"type": "integer"}}}
					]
				}
			}
		}]
	}`

	result := convertOpenAIRequestToClaudeDefault("claude-sonnet-5", []byte(inputJSON), false)
	schema := gjson.GetBytes(result, "tools.0.input_schema")
	if got := schema.Get("type").String(); got != "object" {
		t.Fatalf("input_schema.type = %q, want object. Output: %s", got, string(result))
	}
	if schema.Get("anyOf").Exists() {
		t.Fatalf("root anyOf should be removed. Output: %s", string(result))
	}
	if !schema.Get("properties.query").Exists() {
		t.Fatalf("query property missing. Output: %s", string(result))
	}
	if !schema.Get("properties.limit").Exists() {
		t.Fatalf("limit property missing. Output: %s", string(result))
	}
}

func TestConvertOpenAIRequestToClaudeWithCompatGroupsThinkingTextAndTools(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"assistant","reasoning_content":"reason","content":"answer"},
			{"role":"assistant","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"first","arguments":"{}"}},
				{"id":"call_2","type":"function","function":{"name":"second","arguments":"{}"}}
			]}
		]
	}`)

	out := ConvertOpenAIRequestToClaudeWithCompat("claude-test", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1. Output: %s", len(messages), string(out))
	}
	content := messages[0].Get("content").Array()
	wantTypes := []string{"thinking", "text", "tool_use", "tool_use"}
	if len(content) != len(wantTypes) {
		t.Fatalf("content count = %d, want %d. Output: %s", len(content), len(wantTypes), string(out))
	}
	for index, wantType := range wantTypes {
		if got := content[index].Get("type").String(); got != wantType {
			t.Fatalf("content[%d].type = %q, want %q", index, got, wantType)
		}
	}
}

func TestConvertOpenAIRequestToClaudeMergesToolResultWithAdjacentUserContent(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"work","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"},
			{"role":"user","content":"continue"}
		]
	}`)

	out := convertOpenAIRequestToClaudeDefault("claude-test", inputJSON, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	userContent := messages[1].Get("content").Array()
	if len(userContent) != 2 {
		t.Fatalf("user content count = %d, want 2. Output: %s", len(userContent), string(out))
	}
	if got := userContent[0].Get("type").String(); got != "tool_result" {
		t.Fatalf("user content[0].type = %q, want tool_result", got)
	}
	if got := userContent[1].Get("text").String(); got != "continue" {
		t.Fatalf("user content[1].text = %q, want continue", got)
	}
}

func TestConvertOpenAIRequestToClaudeMapsDeveloperToTopLevelSystem(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"system","content":"S1"},
			{"role":"developer","content":[{"type":"text","text":"D1"},{"type":"text","text":"D2"}]},
			{"role":"user","content":"Hello"}
		]
	}`)

	out := convertOpenAIRequestToClaudeDefault("claude-test", inputJSON, false)
	root := gjson.ParseBytes(out)
	system := root.Get("system").Array()
	if len(system) != 3 {
		t.Fatalf("system block count = %d, want 3. Output: %s", len(system), string(out))
	}
	for index, want := range []string{"S1", "D1", "D2"} {
		if got := system[index].Get("text").String(); got != want {
			t.Fatalf("system[%d].text = %q, want %q", index, got, want)
		}
	}
	messages := root.Get("messages").Array()
	if len(messages) != 1 || messages[0].Get("role").String() != "user" {
		t.Fatalf("messages = %s, want one user message", root.Get("messages").Raw)
	}
}

func TestConvertOpenAIRequestToClaudeSystemMessageCacheControlAppliesToLastBlock(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"developer","cache_control":{"type":"ephemeral","ttl":"1h"},"content":[
				{"type":"text","text":"D1"},
				{"type":"text","text":"D2","cache_control":{"type":"ephemeral"}}
			]},
			{"role":"user","content":"Hello"}
		]
	}`)

	system := gjson.GetBytes(
		convertOpenAIRequestToClaudeDefault("claude-test", inputJSON, false),
		"system",
	).Array()
	if len(system) != 2 {
		t.Fatalf("system block count = %d, want 2", len(system))
	}
	if system[0].Get("cache_control").Exists() {
		t.Fatalf("system[0] must not carry cache_control: %s", system[0].Raw)
	}
	if got := system[1].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system[1].cache_control.type = %q, want ephemeral", got)
	}
	if system[1].Get("cache_control.ttl").Exists() {
		t.Fatalf("part-level cache_control must win: %s", system[1].Raw)
	}
}

func TestConvertOpenAIRequestToClaudeSystemOnlyInputKeepsFallbackUserMessage(t *testing.T) {
	out := convertOpenAIRequestToClaudeDefault(
		"claude-test",
		[]byte(`{"messages":[{"role":"system","content":"S1"}]}`),
		false,
	)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 || messages[0].Get("role").String() != "user" {
		t.Fatalf("messages = %s, want one fallback user message", gjson.GetBytes(out, "messages").Raw)
	}
}

func TestConvertOpenAIRequestToClaudeMaxCompletionTokensFallback(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{"completion only", `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":128000}`, 128000},
		{"max tokens only", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`, 4096},
		{"max tokens wins", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096,"max_completion_tokens":128000}`, 4096},
		{"default", `{"messages":[{"role":"user","content":"hi"}]}`, 32000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := convertOpenAIRequestToClaudeDefault("claude-test", []byte(test.body), false)
			if got := gjson.GetBytes(out, "max_tokens").Int(); got != test.want {
				t.Fatalf("max_tokens = %d, want %d; output=%s", got, test.want, out)
			}
		})
	}
}
