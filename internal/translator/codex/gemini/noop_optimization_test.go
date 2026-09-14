package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCleanGeminiCodexToolParametersPreservesCanonicalSchema(t *testing.T) {
	input := []byte(`{"type":"object","properties":{"value":{"type":"string"}},"additionalProperties":false}`)

	output := cleanGeminiCodexToolParameters(gjson.ParseBytes(input))

	if string(output) != string(input) {
		t.Fatalf("canonical schema changed:\n got: %s\nwant: %s", output, input)
	}
}

func TestCleanGeminiCodexToolParametersNormalizesSchema(t *testing.T) {
	input := []byte(`{"type":"object","$schema":"draft","additionalProperties":true}`)

	output := cleanGeminiCodexToolParameters(gjson.ParseBytes(input))

	if gjson.GetBytes(output, "$schema").Exists() {
		t.Fatal("$schema should be removed")
	}
	if additionalProperties := gjson.GetBytes(
		output, "additionalProperties",
	); additionalProperties.Type != gjson.False {
		t.Fatalf("additionalProperties = %s, want false", additionalProperties.Raw)
	}
}
