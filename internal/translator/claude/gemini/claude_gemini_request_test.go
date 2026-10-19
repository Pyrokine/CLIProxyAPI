package gemini

import (
	"fmt"
	"sync"
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
	for _, role := range []string{"user", "model"} {
		t.Run(role, func(t *testing.T) {
			out := ConvertGeminiRequestToClaude("claude-sonnet-5", []byte(`{"contents":[{"role":"`+role+`","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)
			parts := gjson.GetBytes(out, "messages.0.content").Array()
			wantCount := 1
			if role == "model" {
				wantCount = 3
			}
			if len(parts) != wantCount {
				t.Fatalf("content count = %d, want %d; %s", len(parts), wantCount, out)
			}
			for _, part := range parts[:len(parts)-1] {
				if part.Get("type").String() != "text" {
					t.Fatalf("assistant media placeholder missing: %s", out)
				}
			}
			pdf := parts[len(parts)-1]
			if pdf.Get("type").String() != "document" || pdf.Get("source.media_type").String() != "application/pdf" {
				t.Fatalf("PDF content changed: %s", out)
			}
		})
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

func TestConvertGeminiRequestToClaudePreservesAndMatchesToolCallIDs(t *testing.T) {
	raw := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"alpha","args":{}}},
				{"functionCall":{"call_id":"beta-id","name":"beta","args":{}}},
				{"functionCall":{"id":"alpha-two","name":"alpha","args":{}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"callId":"alpha-two","name":"alpha","response":{"result":"explicit"}}},
				{"functionResponse":{"name":"beta","response":{"result":"named"}}},
				{"functionResponse":{"response":{"result":"fifo"}}}
			]}
		]
	}`)

	out := ConvertGeminiRequestToClaude("claude-test", raw, false)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), out)
	}
	toolUses := messages[0].Get("content").Array()
	wantUses := []string{"toolu_gemini_0000000000000001", "beta-id", "alpha-two"}
	for index, want := range wantUses {
		if got := toolUses[index].Get("id").String(); got != want {
			t.Fatalf("tool use %d ID = %q, want %q; output: %s", index, got, want, out)
		}
	}
	toolResults := messages[1].Get("content").Array()
	wantResults := []string{"alpha-two", "beta-id", "toolu_gemini_0000000000000001"}
	for index, want := range wantResults {
		if got := toolResults[index].Get("tool_use_id").String(); got != want {
			t.Fatalf("tool result %d ID = %q, want %q; output: %s", index, got, want, out)
		}
	}
}

func TestConvertGeminiRequestToClaudeScopesMetadataUserID(t *testing.T) {
	userID := func(raw string) string {
		out := ConvertGeminiRequestToClaude("claude-test", []byte(raw), false)
		return gjson.GetBytes(out, "metadata.user_id").String()
	}

	firstRetry := userID(`{"metadata":{"canonical_session_id":"session-a"},"contents":[{"role":"user","parts":[{"text":"first"}]}]}`)
	secondRetry := userID(`{"metadata":{"canonical_session_id":"session-a"},"contents":[{"role":"user","parts":[{"text":"first"},{"text":"second turn"}]}]}`)
	if firstRetry == "" || firstRetry != secondRetry {
		t.Fatalf("same canonical session user IDs differ: %q != %q", firstRetry, secondRetry)
	}

	otherSession := userID(`{"metadata":{"canonical_session_id":"session-b"},"contents":[{"role":"user","parts":[{"text":"first"}]}]}`)
	if otherSession == firstRetry {
		t.Fatalf("different canonical sessions share user ID %q", firstRetry)
	}

	credentialA := `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-1111-4111-8111-111111111111"}`
	credentialB := `{"device_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":"11111111-1111-4111-8111-111111111111"}`
	explicitIDs := make([]string, 0, 2)
	for _, explicit := range []string{credentialA, credentialB} {
		raw := fmt.Sprintf(`{"metadata":{"user_id":%q},"contents":[{"role":"user","parts":[{"text":"first"}]}]}`, explicit)
		got := userID(raw)
		if got != explicit {
			t.Fatalf("explicit metadata.user_id = %q, want %q", got, explicit)
		}
		if retry := userID(raw); retry != got {
			t.Fatalf("explicit credential identity changed across retries: %q != %q", retry, got)
		}
		explicitIDs = append(explicitIDs, got)
	}
	if explicitIDs[0] == explicitIDs[1] {
		t.Fatalf("different credential identities share metadata.user_id %q", explicitIDs[0])
	}
}

func TestConvertGeminiRequestToClaudeConcurrentIdentityIsolation(t *testing.T) {
	const workers = 64
	expected := make([]string, workers)
	for index := range expected {
		raw := fmt.Sprintf(`{"session_id":"session-%d","contents":[{"role":"user","parts":[{"text":"request"}]}]}`, index)
		expected[index] = gjson.GetBytes(
			ConvertGeminiRequestToClaude("claude-test", []byte(raw), false),
			"metadata.user_id",
		).String()
	}

	errors := make(chan string, workers)
	var waitGroup sync.WaitGroup
	for index := range expected {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			raw := fmt.Sprintf(`{"session_id":"session-%d","contents":[{"role":"user","parts":[{"text":"request"}]}]}`, index)
			got := gjson.GetBytes(
				ConvertGeminiRequestToClaude("claude-test", []byte(raw), false),
				"metadata.user_id",
			).String()
			if got != expected[index] {
				errors <- fmt.Sprintf("session-%d user ID = %q, want %q", index, got, expected[index])
			}
		}(index)
	}
	waitGroup.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
}
