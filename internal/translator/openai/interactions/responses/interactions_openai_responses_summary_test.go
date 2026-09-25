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

func TestResponsesRequestPreservesContextAndAntigravityToolIdentity(t *testing.T) {
	body := []byte(`{
		"model":"antigravity-model",
		"previous_response_id":"response-prev",
		"environment":{"id":"environment-1"},
		"agent_config":{"max_total_tokens":321,"mode":"careful"},
		"max_output_tokens":999,
		"tool_choice":{"type":"function","name":"read_file"},
		"tools":[
			{"type":"function","name":"read_file"},
			{"type":"function","name":"external_read_file"},
			{"type":"function","name":"external_lookup"},
			{"type":"namespace","children":[{"type":"function","name":"external_external_read_file"}]}
		],
		"input":[
			{"type":"function_call","id":"item-1","call_id":"call-1","name":"read_file","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`)
	out := convertOpenAIResponsesRequestToInteractions("", body, false)
	checks := map[string]string{
		"previous_interaction_id":              "response-prev",
		"environment_id":                       "environment-1",
		"agent_config.mode":                    "careful",
		"generation_config.tool_choice.name":   "external_read_file",
		"tools.0.name":                         "external_read_file",
		"tools.1.name":                         "external_external_read_file",
		"tools.2.name":                         "external_lookup",
		"tools.3.function_declarations.0.name": "external_external_external_read_file",
		"input.0.name":                         "external_read_file",
		"input.1.name":                         "external_read_file",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; output=%s", path, got, want, out)
		}
	}
	if got := gjson.GetBytes(out, "agent_config.max_total_tokens").Int(); got != 321 {
		t.Fatalf("explicit max_total_tokens = %d, want 321; output=%s", got, out)
	}

	mapped := convertOpenAIResponsesRequestToInteractions(
		"antigravity-model",
		[]byte(`{"max_output_tokens":999,"input":"hello"}`),
		false,
	)
	if got := gjson.GetBytes(mapped, "agent_config.max_total_tokens").Int(); got != 999 {
		t.Fatalf("mapped max_total_tokens = %d, want 999; output=%s", got, mapped)
	}
}

func TestInteractionsRequestRestoresResponsesContextAndAntigravityToolIdentity(t *testing.T) {
	body := []byte(`{
		"model":"antigravity-model",
		"previous_interaction_id":"interaction-prev",
		"environment":{"id":"environment-2"},
		"agent_config":{"max_total_tokens":654,"mode":"careful"},
		"generation_config":{"tool_choice":{"type":"function","function":{"name":"external_external_read_file"}}},
		"tools":[
			{"type":"function","name":"external_read_file"},
			{"type":"function","name":"external_external_read_file"},
			{"type":"function","name":"external_lookup"},
			{"function_declarations":[{"name":"external_external_external_read_file"}]}
		],
		"input":[
			{"type":"function_call","id":"item-1","call_id":"call-1","name":"external_external_read_file","arguments":{}},
			{"type":"function_result","call_id":"call-1","name":"external_external_read_file","result":"ok"}
		]
	}`)
	out := convertInteractionsRequestToOpenAIResponses("", body, false)
	checks := map[string]string{
		"previous_response_id":      "interaction-prev",
		"environment_id":            "environment-2",
		"agent_config.mode":         "careful",
		"tool_choice.function.name": "external_read_file",
		"tools.0.name":              "read_file",
		"tools.1.name":              "external_read_file",
		"tools.2.name":              "external_lookup",
		"tools.3.name":              "external_external_read_file",
		"input.0.name":              "external_read_file",
		"input.1.name":              "external_read_file",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; output=%s", path, got, want, out)
		}
	}
	if got := gjson.GetBytes(out, "agent_config.max_total_tokens").Int(); got != 654 {
		t.Fatalf("max_total_tokens = %d, want 654; output=%s", got, out)
	}
}
