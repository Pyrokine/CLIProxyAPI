package chat_completions

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToOpenAIReusesMatchingModelPayload(t *testing.T) {
	input := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)

	output := convertOpenAIRequestToOpenAI("gpt-test", input, false)

	if len(input) == 0 || len(output) == 0 {
		t.Fatal("input and output must be non-empty")
	}
	if !bytes.Equal(output, input) {
		t.Fatalf("matching model changed payload:\n got: %s\nwant: %s", output, input)
	}
	if &output[0] != &input[0] {
		t.Fatal("matching model did not reuse the input backing array")
	}
}

func TestConvertOpenAIRequestToOpenAIUpdatesDifferentModel(t *testing.T) {
	input := []byte(`{"model":"old-model","messages":[]}`)

	output := convertOpenAIRequestToOpenAI("new-model", input, false)

	if model := gjson.GetBytes(output, "model").String(); model != "new-model" {
		t.Fatalf("model = %q, want new-model", model)
	}
}
