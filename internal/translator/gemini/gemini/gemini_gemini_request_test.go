package gemini

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

const largeInlineDataSize = 20 << 20

// noinspection GoUnusedGlobalVariable
var largeInlineDataBenchmarkOutput []byte

func TestConvertGeminiRequestToGeminiReusesLargeNormalizedPayload(t *testing.T) {
	input := largeInlineDataGeminiRequest(true)

	output := convertGeminiRequestToGemini("gemini-test", input, false)
	if &output[0] != &input[0] {
		t.Fatal("normalized request should reuse the input payload")
	}
	largeInlineDataBenchmarkOutput = output

	result := testing.Benchmark(
		func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				largeInlineDataBenchmarkOutput = convertGeminiRequestToGemini("gemini-test", input, false)
			}
		},
	)

	if result.N == 0 {
		t.Fatal("allocation benchmark did not complete an iteration")
	}
	if allocated := result.AllocedBytesPerOp(); allocated >= 1<<20 {
		t.Fatalf("normalized 20 MiB inlineData request allocated %d bytes/op, want less than 1 MiB", allocated)
	}
}

func BenchmarkConvertGeminiRequestToGeminiLargeInlineData(b *testing.B) {
	for _, test := range []struct {
		name                  string
		includeSafetySettings bool
	}{
		{name: "normalized_passthrough", includeSafetySettings: true},
		{name: "attach_default_safety", includeSafetySettings: false},
	} {
		b.Run(
			test.name, func(b *testing.B) {
				input := largeInlineDataGeminiRequest(test.includeSafetySettings)
				b.ReportAllocs()
				b.SetBytes(int64(len(input)))
				b.ResetTimer()
				for b.Loop() {
					largeInlineDataBenchmarkOutput = convertGeminiRequestToGemini("gemini-test", input, false)
				}
			},
		)
	}
}

func largeInlineDataGeminiRequest(includeSafetySettings bool) []byte {
	prefix := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"video/mp4","data":"`
	suffix := `"}}]}]`
	if includeSafetySettings {
		suffix += `,"safetySettings":[]`
	}
	return []byte(prefix + strings.Repeat("A", largeInlineDataSize) + suffix + `}`)
}

func validGeminiThoughtSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
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

	out := convertGeminiRequestToGemini("", input, false)

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

	out := convertGeminiRequestToGemini("gemini-3-pro", input, false)

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

	out := convertGeminiRequestToGemini("", input, false)

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

func TestConvertGeminiRequestToGeminiReusesNormalizedPayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"function_declarations":[{"name":"lookup","parametersJsonSchema":{"type":"object","properties":{"query":{"type":"string"}}}}]}],"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"OFF"}]}`)

	out := convertGeminiRequestToGemini("gemini-3-pro", input, false)

	if &out[0] != &input[0] {
		t.Fatal("normalized Gemini request caused a payload copy")
	}
}

func TestBackfillEmptyFunctionResponseNamesReusesNormalizedPayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}]}`)

	out := backfillEmptyFunctionResponseNames(input)

	if &out[0] != &input[0] {
		t.Fatal("normalized function response names caused a payload copy")
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
