package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesSummaryToInteractionsUsesSupportedEnum(t *testing.T) {
	for _, test := range []struct {
		summary string
		want    string
	}{
		{summary: "auto", want: "auto"},
		{summary: "concise", want: "auto"},
		{summary: "detailed", want: "auto"},
		{summary: "none", want: "none"},
		{summary: "banana"},
	} {
		body := []byte(`{"reasoning":{"summary":"` + test.summary + `"},"input":"hello"}`)
		out := convertOpenAIResponsesRequestToInteractions("model", body, false)
		got := gjson.GetBytes(out, "generation_config.thinking_summaries")
		if test.want == "" {
			if got.Exists() {
				t.Fatalf("summary %q produced %s: %s", test.summary, got.Raw, out)
			}
			continue
		}
		if got.String() != test.want {
			t.Fatalf("summary %q produced %q, want %q; body=%s", test.summary, got.String(), test.want, out)
		}
	}
}

func TestInteractionsSummaryToResponsesUsesSupportedEnum(t *testing.T) {
	for _, test := range []struct {
		summary string
		want    string
	}{
		{summary: "auto", want: "auto"},
		{summary: "none"},
		{summary: "banana"},
	} {
		body := []byte(`{"generation_config":{"thinking_summaries":"` + test.summary + `"},"input":"hello"}`)
		out := convertInteractionsRequestToOpenAIResponses("model", body, false)
		got := gjson.GetBytes(out, "reasoning.summary")
		if test.want == "" {
			if got.Exists() {
				t.Fatalf("summary %q produced %s: %s", test.summary, got.Raw, out)
			}
			continue
		}
		if got.String() != test.want {
			t.Fatalf("summary %q produced %q, want %q; body=%s", test.summary, got.String(), test.want, out)
		}
	}
}
