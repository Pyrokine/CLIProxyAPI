package responses

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexNormalizedRequestOriginalMinimalInputReusesBytes(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6","stream":true,"store":false,"parallel_tool_calls":true,` +
		`"include":["reasoning.encrypted_content"],"service_tier":"priority",` +
		`"input":[{"type":"message","role":"user","content":"hello"}]}`)
	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", input, true)
	if len(output) == 0 || !bytes.Equal(output, input) || &output[0] != &input[0] {
		t.Fatalf("original normalized request was copied or changed: %s", output)
	}
}

func TestCodexRequiredFieldsOriginalMinimalInput(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5.6",
		"stream":"true",
		"store":true,
		"parallel_tool_calls":false,
		"include":["file_search_call.results","reasoning.encrypted_content"],
		"max_output_tokens":4096,
		"max_completion_tokens":4096,
		"temperature":0.2,
		"top_p":0.9,
		"service_tier":"standard",
		"truncation":"auto",
		"user":"request-owner",
		"input":[{"type":"message","role":"system","content":"hello"}]
	}`)
	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", input, true)
	if gjson.GetBytes(output, "stream").Type != gjson.True ||
		gjson.GetBytes(output, "store").Type != gjson.False ||
		gjson.GetBytes(output, "parallel_tool_calls").Type != gjson.True {
		t.Fatalf("required boolean fields changed: %s", output)
	}
	include := gjson.GetBytes(output, "include").Array()
	if len(include) != 1 || include[0].Type != gjson.String || include[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("include changed: %s", output)
	}
	if gjson.GetBytes(output, "input.0.role").String() != "developer" {
		t.Fatalf("original system role was not normalized: %s", output)
	}
	for _, field := range []string{
		"max_output_tokens", "max_completion_tokens", "temperature", "top_p", "service_tier", "truncation", "user",
	} {
		if gjson.GetBytes(output, field).Exists() {
			t.Fatalf("%s should be removed: %s", field, output)
		}
	}
}
