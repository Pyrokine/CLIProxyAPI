package interactions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertInteractionsRequestToGeminiKeepsSchemaReferencesOpaque(t *testing.T) {
	payload := []byte(`{"input":[{"type":"function_result","name":"lookup","call_id":"call-1","result":{"schema":{"$ref":"#/components/schemas/Item"}}}]}`)
	out := convertInteractionsRequestToGemini("gemini-3-flash", payload, false)
	result := gjson.GetBytes(out, "contents.0.parts.0.functionResponse.response.result")
	if result.Type != gjson.String || result.String() != `{"schema":{"$ref":"#/components/schemas/Item"}}` {
		t.Fatalf("function result with $ref = %s, output = %s", result.Raw, out)
	}
}

func TestConvertInteractionsRequestToGeminiRequiresBooleanIncludeThoughts(t *testing.T) {
	for _, body := range []string{
		`{"generation_config":{"include_thoughts":"true"},"input":"hello"}`,
		`{"generation_config":{"thinking_config":{"include_thoughts":1}},"input":"hello"}`,
	} {
		out := convertInteractionsRequestToGemini("gemini-3-flash", []byte(body), false)
		if value := gjson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts"); value.Exists() {
			t.Fatalf("invalid includeThoughts selector survived as %s: %s", value.Raw, out)
		}
	}
}

func TestConvertGeminiRequestToInteractionsIgnoresInvalidSummaryAlias(t *testing.T) {
	body := []byte(`{
		"generationConfig": {
			"thinkingSummaries": "banana",
			"thinkingConfig": {"includeThoughts": true}
		},
		"contents": []
	}`)
	out := convertGeminiRequestToInteractions("gemini-3-flash", body, false)
	if got := gjson.GetBytes(out, "generation_config.thinking_summaries").String(); got != "auto" {
		t.Fatalf("thinking_summaries = %q, want auto; body=%s", got, out)
	}
	if gjson.GetBytes(out, "generation_config.thinking_config.include_thoughts").Exists() {
		t.Fatalf("compatibility include_thoughts alias remained: %s", out)
	}
}

func TestConvertInteractionsRequestToGeminiAcceptsBooleanIncludeThoughts(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{body: `{"generation_config":{"include_thoughts":true},"input":"hello"}`, want: true},
		{body: `{"generation_config":{"thinking_config":{"include_thoughts":false}},"input":"hello"}`, want: false},
	} {
		out := convertInteractionsRequestToGemini("gemini-3-flash", []byte(test.body), false)
		value := gjson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts")
		if !value.IsBool() || value.Bool() != test.want {
			t.Fatalf("includeThoughts = %s, want %t; body=%s", value.Raw, test.want, out)
		}
	}
}
