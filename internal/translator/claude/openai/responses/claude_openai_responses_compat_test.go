package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaudeWithCompatPreservesReasoning(t *testing.T) {
	tests := []struct {
		name      string
		encrypted string
	}{
		{name: "empty"},
		{name: "opaque", encrypted: "opaque-deepseek-id"},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				payload := []byte(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"reason"}],"encrypted_content":"` + testCase.encrypted + `"}]}`)
				withCompat := ConvertOpenAIResponsesRequestToClaudeWithCompat(
					"deepseek-v4",
					payload,
					false,
				)
				part := gjson.GetBytes(withCompat, "messages.0.content.0")
				if part.Get("type").String() != "thinking" ||
					part.Get("thinking").String() != "reason" ||
					part.Get("signature").String() != testCase.encrypted {
					t.Fatalf("compat translation missing thinking block: %s", withCompat)
				}
			},
		)
	}
}
