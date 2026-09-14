package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToClaudeDropsTemperature(t *testing.T) {
	raw := []byte(`{
		"generationConfig": {
			"temperature": 0.2,
			"topP": 0.8
		},
		"contents": [
			{
				"role": "user",
				"parts": [{"text": "hi"}]
			}
		]
	}`)

	out := ConvertGeminiRequestToClaude("claude-sonnet-5", raw, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.8 {
		t.Fatalf("top_p = %v, want 0.8", got)
	}
}

func TestConvertGeminiRequestToClaudeAcceptsInlineDataAliases(t *testing.T) {
	tests := []struct {
		name string
		part string
	}{
		{
			name: "camel case",
			part: `{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}`,
		},
		{
			name: "snake case",
			part: `{"inline_data":{"mime_type":"image/png","data":"aGVsbG8="}}`,
		},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				raw := []byte(`{"contents":[{"role":"user","parts":[` + testCase.part + `]}]}`)
				out := ConvertGeminiRequestToClaude("claude-sonnet-5", raw, false)
				content := gjson.GetBytes(out, "messages.0.content.0")
				if got := content.Get("type").String(); got != "image" {
					t.Fatalf("content type = %q, want image. Output: %s", got, out)
				}
				if got := content.Get("source.media_type").String(); got != "image/png" {
					t.Fatalf("media_type = %q, want image/png. Output: %s", got, out)
				}
				if got := content.Get("source.data").String(); got != "aGVsbG8=" {
					t.Fatalf("source.data = %q, want aGVsbG8=. Output: %s", got, out)
				}
			},
		)
	}
}

func TestConvertGeminiRequestToClaudeSplitsInlineDataByMIME(t *testing.T) {
	out := ConvertGeminiRequestToClaude(
		"claude-sonnet-5",
		[]byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`),
		false,
	)

	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("audio fallback type = %q, want text. Output: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.type").String(); got != "text" {
		t.Fatalf("video fallback type = %q, want text. Output: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.type").String(); got != "document" {
		t.Fatalf("PDF content type = %q, want document. Output: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.source.media_type").String(); got != "application/pdf" {
		t.Fatalf("PDF media_type = %q, want application/pdf. Output: %s", got, out)
	}
}

func TestConvertGeminiRequestToClaudeConvertsCamelFileDataByMIME(t *testing.T) {
	out := ConvertGeminiRequestToClaude(
		"claude-sonnet-5",
		[]byte(`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"https://example.com/image.png"}},{"fileData":{"mimeType":"application/pdf","fileUri":"https://example.com/document.pdf"}}]}]}`),
		false,
	)

	image := gjson.GetBytes(out, "messages.0.content.0")
	if got := image.Get("type").String(); got != "image" {
		t.Fatalf("image content type = %q, want image. Output: %s", got, out)
	}
	if got := image.Get("source.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image URL = %q. Output: %s", got, out)
	}

	document := gjson.GetBytes(out, "messages.0.content.1")
	if got := document.Get("type").String(); got != "document" {
		t.Fatalf("document content type = %q, want document. Output: %s", got, out)
	}
	if got := document.Get("source.url").String(); got != "https://example.com/document.pdf" {
		t.Fatalf("document URL = %q. Output: %s", got, out)
	}
	if got := document.Get("source.media_type").String(); got != "application/pdf" {
		t.Fatalf("document media_type = %q, want application/pdf. Output: %s", got, out)
	}
}

func TestConvertGeminiRequestToClaudeNormalizesToolSchemas(t *testing.T) {
	raw := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"tools":[{"functionDeclarations":[
			{
				"name":"any_union",
				"parameters":{"anyOf":[
					{"type":"OBJECT","properties":{"query":{"type":"STRING"}}},
					{"type":"object","properties":{"limit":{"type":"INTEGER"}}}
				]}
			},
			{"name":"boolean_alias","parametersJsonSchema":true}
		]}]
	}`)

	out := ConvertGeminiRequestToClaude("claude-sonnet-5", raw, false)
	root := gjson.ParseBytes(out)
	for _, name := range []string{"any_union", "boolean_alias"} {
		schema := root.Get(`tools.#(name=="` + name + `").input_schema`)
		if got := schema.Get("type").String(); got != "object" {
			t.Fatalf("%s input_schema.type = %q, want object. Output: %s", name, got, string(out))
		}
		if !schema.Get("properties").IsObject() {
			t.Fatalf("%s input_schema.properties should be an object. Output: %s", name, string(out))
		}
		if got := schema.Get("additionalProperties").Bool(); got {
			t.Fatalf("%s additionalProperties = true, want false. Output: %s", name, string(out))
		}
		if got := schema.Get("$schema").String(); got != "http://json-schema.org/draft-07/schema#" {
			t.Fatalf("%s $schema = %q, want draft-07. Output: %s", name, got, string(out))
		}
	}

	anySchema := root.Get(`tools.#(name=="any_union").input_schema`)
	if anySchema.Get("anyOf").Exists() {
		t.Fatalf("any_union input_schema should not contain root anyOf. Output: %s", string(out))
	}
	if got := anySchema.Get("properties.query.type").String(); got != "string" {
		t.Fatalf("query type = %q, want string. Output: %s", got, string(out))
	}
	if got := anySchema.Get("properties.limit.type").String(); got != "integer" {
		t.Fatalf("limit type = %q, want integer. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToClaudeGroupsConsecutiveRoleTurns(t *testing.T) {
	raw := []byte(`{
		"contents":[
			{"role":"model","parts":[{"text":"answer"}]},
			{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}}]},
			{"role":"model","parts":[{"functionCall":{"name":"second","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"first","response":{"result":"one"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"second","response":{"result":"two"}}}]}
		]
	}`)

	out := ConvertGeminiRequestToClaude("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	assistantContent := messages[0].Get("content").Array()
	wantTypes := []string{"text", "tool_use", "tool_use"}
	if len(assistantContent) != len(wantTypes) {
		t.Fatalf(
			"assistant content count = %d, want %d. Output: %s", len(assistantContent), len(wantTypes), string(out),
		)
	}
	for index, wantType := range wantTypes {
		if got := assistantContent[index].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q", index, got, wantType)
		}
	}
	userContent := messages[1].Get("content").Array()
	if len(userContent) != 2 {
		t.Fatalf("user content count = %d, want 2. Output: %s", len(userContent), string(out))
	}
	for index := range userContent {
		wantID := assistantContent[index+1].Get("id").String()
		if got := userContent[index].Get("tool_use_id").String(); got != wantID {
			t.Fatalf("user content[%d].tool_use_id = %q, want %q", index, got, wantID)
		}
	}
}

func TestConvertGeminiRequestToClaudeKeepsSystemInstructionSeparate(t *testing.T) {
	raw := []byte(`{
		"system_instruction":{"parts":[{"text":"system rule"}]},
		"contents":[{"role":"user","parts":[{"text":"question"}]}]
	}`)

	out := ConvertGeminiRequestToClaude("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	if got := messages[0].Get("content.0.text").String(); got != "system rule" {
		t.Fatalf("system text = %q, want system rule", got)
	}
	if got := messages[1].Get("content.0.text").String(); got != "question" {
		t.Fatalf("user text = %q, want question", got)
	}
}
