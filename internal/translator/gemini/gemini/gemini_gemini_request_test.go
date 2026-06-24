package gemini

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validGeminiThoughtSignatureForTest() string {
	payload := protowire.AppendTag(nil, 1, protowire.BytesType)
	payload = protowire.AppendBytes(payload, []byte{0x01, 0x02, 0x03, 0x04})
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertGeminiRequestToGeminiCleansToolSchema(t *testing.T) {
	input := []byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
		"tools": [
			{
				"functionDeclarations": [
					{
						"name": "search",
						"eager_input_streaming": true,
						"parameters": {
							"type": "object",
							"title": "SearchInput",
							"properties": {
								"query": {"type": "string", "nullable": true, "title": "Query"}
							}
						}
					}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToGemini("", input, false)

	if gjson.GetBytes(out, "tools.0.function_declarations.0.eager_input_streaming").Exists() {
		t.Fatalf("eager_input_streaming should be removed: %s", out)
	}
	if gjson.GetBytes(out, "tools.0.function_declarations.0.parametersJsonSchema.title").Exists() {
		t.Fatalf("schema title should be removed: %s", out)
	}
	if gjson.GetBytes(out, "tools.0.function_declarations.0.parametersJsonSchema.properties.query.nullable").Exists() {
		t.Fatalf("schema nullable should be removed: %s", out)
	}
}

func TestConvertGeminiRequestToGeminiPreservesValidThoughtSignature(t *testing.T) {
	signature := validGeminiThoughtSignatureForTest()
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {}}, "thoughtSignature": "` + signature + `"}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToGemini("gemini-3-pro", input, false)

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != signature {
		t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, signature, string(out))
	}
}

func TestBackfillEmptyFunctionResponseNames_Single(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected backfilled name 'Bash', got '%s'", name)
	}
}

func TestBackfillEmptyFunctionResponseNames_Parallel(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Read", "args": {"path": "/a"}}},
					{"functionCall": {"name": "Grep", "args": {"pattern": "x"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "content a"}}},
					{"functionResponse": {"name": "", "response": {"result": "match x"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name0 := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	name1 := gjson.GetBytes(out, "contents.1.parts.1.functionResponse.name").String()
	if name0 != "Read" {
		t.Errorf("Expected first name 'Read', got '%s'", name0)
	}
	if name1 != "Grep" {
		t.Errorf("Expected second name 'Grep', got '%s'", name1)
	}
}

func TestBackfillEmptyFunctionResponseNames_PreservesExisting(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "Bash", "response": {"result": "ok"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected preserved name 'Bash', got '%s'", name)
	}
}

func TestConvertGeminiRequestToGemini_BackfillsEmptyName(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToGemini("", input, false)

	name := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected backfilled name 'Bash', got '%s'", name)
	}
}

func TestBackfillEmptyFunctionResponseNames_MoreResponsesThanCalls(t *testing.T) {
	// Extra responses beyond the call count should not panic and should be left unchanged.
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "ok"}}},
					{"functionResponse": {"name": "", "response": {"result": "extra"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name0 := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name0 != "Bash" {
		t.Errorf("Expected first name 'Bash', got '%s'", name0)
	}
	// Second response has no matching call, should remain empty
	name1 := gjson.GetBytes(out, "contents.1.parts.1.functionResponse.name").String()
	if name1 != "" {
		t.Errorf("Expected second name to remain empty, got '%s'", name1)
	}
}

func TestBackfillEmptyFunctionResponseNames_MultipleGroups(t *testing.T) {
	// Two sequential call/response groups should each get correct names.
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Read", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "content"}}}
				]
			},
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Grep", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "match"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name0 := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	name1 := gjson.GetBytes(out, "contents.3.parts.0.functionResponse.name").String()
	if name0 != "Read" {
		t.Errorf("Expected first group name 'Read', got '%s'", name0)
	}
	if name1 != "Grep" {
		t.Errorf("Expected second group name 'Grep', got '%s'", name1)
	}
}
