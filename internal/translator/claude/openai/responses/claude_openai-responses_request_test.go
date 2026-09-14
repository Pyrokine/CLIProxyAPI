package responses

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validClaudeThinkingSignature() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertOpenAIResponsesRequestToClaudeReasoningItemToThinkingBlock(t *testing.T) {
	signature := validClaudeThinkingSignature()
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[{"type":"summary_text","text":"internal reasoning"}]
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"visible answer"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	assistant := root.Get("messages.0")
	if got := assistant.Get("role").String(); got != "assistant" {
		t.Fatalf("first message role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.signature").String(); got != signature {
		t.Fatalf("thinking signature = %q, want %q", got, signature)
	}
	if got := assistant.Get("content.0.thinking").String(); got != "internal reasoning" {
		t.Fatalf("thinking text = %q, want internal reasoning", got)
	}
	if got := assistant.Get("content.1.type").String(); got != "text" {
		t.Fatalf("second content type = %q, want text. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.1.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeSignatureOnlyReasoningFlushesBeforeUser(t *testing.T) {
	signature := validClaudeThinkingSignature()
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	thinking := root.Get("messages.0.content.0")
	if got := thinking.Get("type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := thinking.Get("signature").String(); got != signature {
		t.Fatalf("thinking signature = %q, want %q", got, signature)
	}
	if got := thinking.Get("thinking").String(); got != "" {
		t.Fatalf("thinking text = %q, want empty", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeSanitizesToolIDs(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"function_call",
				"call_id":"call.bad/id",
				"name":"do_work",
				"arguments":"{\"a\":1}"
			},
			{
				"type":"function_call_output",
				"call_id":"call.bad/id",
				"output":"tool ok"
			}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.0.content.0.id").String(); got != "call_bad_id" {
		t.Fatalf("tool_use id = %q, want call_bad_id. Output: %s", got, string(out))
	}
	if got := root.Get("messages.1.content.0.tool_use_id").String(); got != "call_bad_id" {
		t.Fatalf("tool_result tool_use_id = %q, want call_bad_id. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeTrailingReasoningFlushes(t *testing.T) {
	signature := validClaudeThinkingSignature()
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			},
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[{"type":"summary_text","text":"tail reasoning"}]
			}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	thinking := root.Get("messages.1.content.0")
	if got := thinking.Get("type").String(); got != "thinking" {
		t.Fatalf("trailing content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := thinking.Get("signature").String(); got != signature {
		t.Fatalf("trailing thinking signature = %q, want %q", got, signature)
	}
	if got := thinking.Get("thinking").String(); got != "tail reasoning" {
		t.Fatalf("trailing thinking text = %q, want tail reasoning", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaudeNamespaceToolMapping(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":"hello",
		"tools":[{
			"type":"namespace",
			"name":"mcp__browser",
			"tools":[{
				"type":"function",
				"name":"open",
				"description":"Open URL",
				"parameters":{"type":"object","properties":{"url":{"type":"string"}}}
			}]
		}],
		"tool_choice":{"type":"function","function":{"name":"open"}}
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("tools.0.name").String(); got != "mcp__browser__open" {
		t.Fatalf("tools.0.name = %q, want mcp__browser__open. Output: %s", got, string(out))
	}
	if got := root.Get("tools.0.input_schema.properties.url.type").String(); got != "string" {
		t.Fatalf("url schema type = %q, want string", got)
	}
	if got := root.Get("tool_choice.name").String(); got != "mcp__browser__open" {
		t.Fatalf("tool_choice.name = %q, want mcp__browser__open. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeWebSearchTool(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":"hello",
		"tools":[{
			"type":"web_search",
			"name":"web_search",
			"max_uses":3,
			"filters":{"allowed_domains":["example.com"]},
			"user_location":{"type":"approximate","country":"US"}
		}]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("tools.0.type").String(); got != "web_search_20250305" {
		t.Fatalf("tools.0.type = %q, want web_search_20250305. Output: %s", got, string(out))
	}
	if got := root.Get("tools.0.max_uses").Int(); got != 3 {
		t.Fatalf("max_uses = %d, want 3", got)
	}
	if got := root.Get("tools.0.allowed_domains.0").String(); got != "example.com" {
		t.Fatalf("allowed domain = %q, want example.com", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaudeDropsEmptyWebSearchDomains(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":"hello",
		"tools":[{
			"type":"web_search",
			"filters":{"allowed_domains":[],"blocked_domains":["blocked.example"]}
		}]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if root.Get("tools.0.allowed_domains").Exists() {
		t.Fatalf("empty allowed_domains should be omitted. Output: %s", string(out))
	}
	if got := root.Get("tools.0.blocked_domains.0").String(); got != "blocked.example" {
		t.Fatalf("blocked domain = %q, want blocked.example. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeDropsInvalidToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":"hello",
		"tools":[{"type":"function","name":"allowed"}],
		"tool_choice":{"type":"function","function":{"name":"missing"}}
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if root.Get("tool_choice").Exists() {
		t.Fatalf("tool_choice should be omitted for missing tool. Output: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeNormalizesToolSchemas(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":"hello",
		"tools":[
			{
				"type":"function",
				"name":"any_union",
				"parameters":{"anyOf":[
					{"type":"object","properties":{"query":{"type":"string"}}},
					{"type":"object","properties":{"limit":{"type":"integer"}}}
				]}
			},
			{
				"type":"function",
				"name":"one_union_alias",
				"parametersJsonSchema":{
					"type":"object",
					"properties":{"query":{"type":"string"},"id":{"type":"string"}},
					"oneOf":[{"required":["query"]},{"required":["id"]}]
				}
			},
			{
				"type":"function",
				"name":"all_union",
				"parameters":{"allOf":[
					{"type":"object","properties":{"first":{"type":"string"}},"required":["first"]},
					{"type":"object","properties":{"second":{"type":"integer"}},"required":["second"]}
				]}
			},
			{"type":"function","name":"missing_schema"},
			{"type":"function","name":"null_schema","parameters":null},
			{"type":"function","name":"boolean_schema","parameters":true},
			{"type":"function","name":"scalar_schema","parameters":"invalid"}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)
	for _, name := range []string{
		"any_union",
		"one_union_alias",
		"all_union",
		"missing_schema",
		"null_schema",
		"boolean_schema",
		"scalar_schema",
	} {
		schema := root.Get(`tools.#(name=="` + name + `").input_schema`)
		if got := schema.Get("type").String(); got != "object" {
			t.Fatalf("%s input_schema.type = %q, want object. Output: %s", name, got, string(out))
		}
		if !schema.Get("properties").IsObject() {
			t.Fatalf("%s input_schema.properties should be an object. Output: %s", name, string(out))
		}
		for _, unionName := range []string{"anyOf", "oneOf", "allOf"} {
			if schema.Get(unionName).Exists() {
				t.Fatalf("%s input_schema should not contain root %s. Output: %s", name, unionName, string(out))
			}
		}
	}

	anySchema := root.Get(`tools.#(name=="any_union").input_schema`)
	if !anySchema.Get("properties.query").Exists() || !anySchema.Get("properties.limit").Exists() {
		t.Fatalf("any_union properties were not merged. Output: %s", string(out))
	}
	oneSchema := root.Get(`tools.#(name=="one_union_alias").input_schema`)
	if oneSchema.Get("required").Exists() {
		t.Fatalf("one_union_alias should not merge alternative required fields. Output: %s", string(out))
	}
	allSchema := root.Get(`tools.#(name=="all_union").input_schema`)
	if got := allSchema.Get("required.#").Int(); got != 2 {
		t.Fatalf("all_union required count = %d, want 2. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaudeGroupsAssistantAndToolResultTurns(t *testing.T) {
	signature := validClaudeThinkingSignature()
	raw := []byte(`{
		"input":[
			{"type":"reasoning","encrypted_content":"` + signature + `","summary":[{"type":"summary_text","text":"reason"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
			{"type":"function_call","call_id":"call_1","name":"first","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"second","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"one"},
			{"type":"function_call_output","call_id":"call_2","output":"two"}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	assistantContent := messages[0].Get("content").Array()
	wantAssistantTypes := []string{"thinking", "text", "tool_use", "tool_use"}
	if len(assistantContent) != len(wantAssistantTypes) {
		t.Fatalf(
			"assistant content count = %d, want %d. Output: %s", len(assistantContent), len(wantAssistantTypes),
			string(out),
		)
	}
	for index, wantType := range wantAssistantTypes {
		if got := assistantContent[index].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q", index, got, wantType)
		}
	}
	for index, wantID := range []string{"call_1", "call_2"} {
		if got := assistantContent[index+2].Get("id").String(); got != wantID {
			t.Fatalf("assistant tool_use[%d].id = %q, want %q", index, got, wantID)
		}
	}
	userContent := messages[1].Get("content").Array()
	if len(userContent) != 2 {
		t.Fatalf("user content count = %d, want 2. Output: %s", len(userContent), string(out))
	}
	for index, wantID := range []string{"call_1", "call_2"} {
		if got := userContent[index].Get("tool_use_id").String(); got != wantID {
			t.Fatalf("user content[%d].tool_use_id = %q, want %q", index, got, wantID)
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaudeMergesUserMessagesAndPreservesCacheControl(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"message","role":"user","cache_control":{"type":"ephemeral"},"content":[{"type":"input_text","text":"first"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1. Output: %s", len(messages), string(out))
	}
	content := messages[0].Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("content count = %d, want 2. Output: %s", len(content), string(out))
	}
	if got := content[0].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("content[0].cache_control.type = %q, want ephemeral", got)
	}
	if got := content[0].Get("text").String(); got != "first" {
		t.Fatalf("content[0].text = %q, want first", got)
	}
	if got := content[1].Get("text").String(); got != "second" {
		t.Fatalf("content[1].text = %q, want second", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaudeDoesNotMergeAcrossRoleChanges(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"message","role":"assistant","content":"first"},
			{"type":"message","role":"user","content":"question"},
			{"type":"message","role":"assistant","content":"second"}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("message count = %d, want 3. Output: %s", len(messages), string(out))
	}
	for index, wantRole := range []string{"assistant", "user", "assistant"} {
		if got := messages[index].Get("role").String(); got != wantRole {
			t.Fatalf("messages[%d].role = %q, want %q", index, got, wantRole)
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaudeEmptyMessageDoesNotBreakTurn(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"message","role":"assistant","content":"first"},
			{"type":"message","role":"user","content":""},
			{"type":"message","role":"assistant","content":"second"}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1. Output: %s", len(messages), string(out))
	}
	content := messages[0].Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("content count = %d, want 2. Output: %s", len(content), string(out))
	}
	for index, wantText := range []string{"first", "second"} {
		if got := content[index].Get("text").String(); got != wantText {
			t.Fatalf("content[%d].text = %q, want %q", index, got, wantText)
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaudeMapsSystemLevelInputsToSeparateBlocks(t *testing.T) {
	raw := []byte(`{
		"instructions":"I1",
		"input":[
			{"type":"message","role":"system","content":[{"type":"input_text","text":"S1"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"U1"}]},
			{"type":"message","role":"developer","content":"D1"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A1"}]},
			{"type":"message","role":"system","content":[{"type":"text","text":"S2"}]}
		]
	}`)

	out := convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false)
	root := gjson.ParseBytes(out)
	system := root.Get("system").Array()
	if len(system) != 4 {
		t.Fatalf("system block count = %d, want 4. Output: %s", len(system), string(out))
	}
	for index, want := range []string{"I1", "S1", "D1", "S2"} {
		if got := system[index].Get("text").String(); got != want {
			t.Fatalf("system[%d].text = %q, want %q", index, got, want)
		}
	}
	messages := root.Get("messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	for index, wantRole := range []string{"user", "assistant"} {
		if got := messages[index].Get("role").String(); got != wantRole {
			t.Fatalf("messages[%d].role = %q, want %q", index, got, wantRole)
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaudeSystemOnlyInputKeepsFallbackUserMessage(t *testing.T) {
	out := convertOpenAIResponsesRequestToClaudeDefault(
		"claude-test",
		[]byte(`{"instructions":"I1"}`),
		false,
	)
	root := gjson.ParseBytes(out)
	if got := len(root.Get("system").Array()); got != 1 {
		t.Fatalf("system block count = %d, want 1", got)
	}
	messages := root.Get("messages").Array()
	if len(messages) != 1 || messages[0].Get("role").String() != "user" {
		t.Fatalf("messages = %s, want one fallback user message", root.Get("messages").Raw)
	}
}

func TestConvertOpenAIResponsesRequestToClaudeSystemNonTextPartKeepsTypedMarker(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"message","role":"developer","content":[
				{"type":"input_text","text":"D1"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"U1"}]}
		]
	}`)

	system := gjson.GetBytes(
		convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false),
		"system",
	).Array()
	if len(system) != 2 {
		t.Fatalf("system block count = %d, want 2", len(system))
	}
	if got := system[0].Get("text").String(); got != "D1" {
		t.Fatalf("system[0].text = %q, want D1", got)
	}
	if got := system[1].Get("type").String(); got != "input_image" {
		t.Fatalf("system[1].type = %q, want input_image", got)
	}
	if system[1].Get("source").Exists() || system[1].Get("image_url").Exists() {
		t.Fatalf("typed marker copied unsupported payload: %s", system[1].Raw)
	}
}

func TestConvertOpenAIResponsesRequestToClaudeSystemCacheControlAppliesToLastBlock(t *testing.T) {
	raw := []byte(`{
		"input":[
			{"type":"message","role":"system","cache_control":{"type":"ephemeral","ttl":"1h"},"content":[
				{"type":"input_text","text":"S1"},
				{"type":"input_text","text":"S2","cache_control":{"type":"ephemeral"}}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"U1"}]}
		]
	}`)

	system := gjson.GetBytes(
		convertOpenAIResponsesRequestToClaudeDefault("claude-test", raw, false),
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
