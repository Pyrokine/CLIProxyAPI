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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
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

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if root.Get("tool_choice").Exists() {
		t.Fatalf("tool_choice should be omitted for missing tool. Output: %s", string(out))
	}
}
