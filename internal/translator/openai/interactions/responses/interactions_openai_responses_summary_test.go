package responses

import (
	"context"
	"errors"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsFailedEventToOpenAIResponses(t *testing.T) {
	var state any
	created := convertInteractionsResponseToOpenAIResponses(
		context.Background(), "test-model", nil, nil,
		[]byte(`{"event_type":"interaction.created","interaction":{"id":"interaction_1","model":"test-model"}}`), &state,
	)
	if len(created) != 1 {
		t.Fatalf("created events = %d, want 1", len(created))
	}
	failed := convertInteractionsResponseToOpenAIResponses(
		context.Background(), "test-model", nil, nil,
		[]byte(`{"event_type":"response.failed","error":{"type":"api_error","code":"resource_exhausted","message":"model capacity exhausted"}}`), &state,
	)
	if len(failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(failed))
	}
	response := responseEventPayload(failed[0])
	if response.Get("type").String() != "response.failed" ||
		response.Get("response.id").String() != "interaction_1" ||
		response.Get("response.status").String() != "failed" ||
		response.Get("response.error.code").String() != "resource_exhausted" ||
		response.Get("response.error.message").String() != "model capacity exhausted" {
		t.Fatalf("failed response = %s", response.Raw)
	}
	completed := convertInteractionsResponseToOpenAIResponses(context.Background(), "test-model", nil, nil,
		[]byte(`{"event_type":"interaction.completed"}`), &state)
	if len(completed) != 0 {
		t.Fatalf("completed after failure = %q", completed)
	}
	done := convertInteractionsResponseToOpenAIResponses(context.Background(), "test-model", nil, nil, []byte("[DONE]"), &state)
	if len(done) != 1 || string(done[0]) != "data: [DONE]\n\n" {
		t.Fatalf("terminal marker after failure = %q", done)
	}
}

func TestInteractionFailedEventToOpenAIResponsesUsesNestedError(t *testing.T) {
	var state any
	failed := convertInteractionsResponseToOpenAIResponses(context.Background(), "test-model", nil, nil,
		[]byte(`{"event_type":"interaction.failed","response":{"id":"response-1","error":{"code":"unavailable","message":"upstream offline"}}}`), &state)
	if len(failed) != 1 {
		t.Fatalf("failed events = %q", failed)
	}
	payload := responseEventPayload(failed[0])
	if payload.Get("response.id").String() != "response-1" ||
		payload.Get("response.error.code").String() != "unavailable" ||
		payload.Get("response.error.message").String() != "upstream offline" {
		t.Fatalf("nested failure = %s", payload.Raw)
	}
}

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
		"previous_interaction_id":            "response-prev",
		"environment_id":                     "environment-1",
		"agent_config.mode":                  "careful",
		"generation_config.tool_choice.name": "external_read_file",
		"tools.0.name":                       "external_read_file",
		"tools.1.name":                       "external_external_read_file",
		"tools.2.name":                       "external_lookup",
		"tools.3.name":                       "external_external_external_read_file",
		"input.0.name":                       "external_read_file",
		"input.1.name":                       "external_read_file",
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

func TestResponsesRequestFlattensDevinNamespaceChildrenAndFiltersAutomation(t *testing.T) {
	body := []byte(`{
		"model":"devin/swe-2",
		"tool_choice":{"type":"function","name":"automation_update","namespace":"mcp__codex_app"},
		"tools":[
			{"type":"namespace","name":"mcp__codex_app","children":[{"type":"function","name":"automation_update","description":"modify automations"}]},
			{"type":"namespace","name":"mcp__terminal","children":[{"type":"function","name":"exec_command","description":"returning output or a session ID for ongoing interaction","parameters":{"type":"object"}}]},
			{"type":"function","name":"write_stdin","description":"Writes characters to an existing unified exec session and returns recent output."}
		],
		"input":[
			{"type":"function_call","call_id":"call-1","namespace":"mcp__terminal","name":"exec_command","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":"done"}
		]
	}`)
	out := convertOpenAIResponsesRequestToInteractions("", body, false)
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 || tools[0].Get("name").String() != "mcp__terminal__exec_command" || tools[1].Get("name").String() != "write_stdin" {
		t.Fatalf("Devin namespace tools = %s, output = %s", tools, out)
	}
	if tools[0].Get("description").String() != "returning output or an session ID for ongoing interaction" ||
		tools[1].Get("description").String() != "Writes characters to a existing unified exec session and returns recent output." ||
		tools[0].Get("parameters.type").String() != "object" {
		t.Fatalf("Devin tool descriptions/parameters = %s", out)
	}
	if gjson.GetBytes(out, "generation_config.tool_choice").Exists() {
		t.Fatalf("filtered automation tool was forced by tool_choice: %s", out)
	}
	if gjson.GetBytes(out, "input.0.name").String() != "mcp__terminal__exec_command" ||
		gjson.GetBytes(out, "input.1.name").String() != "mcp__terminal__exec_command" {
		t.Fatalf("namespace tool history identity changed: %s", out)
	}
}

func TestResponsesToInteractionsRejectsOnlyUnsendableUserTurns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		wantError bool
	}{
		{"file-only user message", `[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]`, true},
		{"file-only earlier turn", `[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-1"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"later"}]}]`, true},
		{"file-only standalone part", `[{"type":"input_file","file_id":"file-1"}]`, true},
		{"file with sendable text", `[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-1"},{"type":"input_text","text":"keep"}]}]`, false},
		{"image with data", `[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatInteractions, "model", []byte(`{"input":`+tc.input+`}`), false)
			var unsupported *translatorcommon.UnsupportedPartError
			if tc.wantError {
				if !errors.As(err, &unsupported) || unsupported.StatusCode() != 400 || !unsupported.IsRequestScoped() || body != nil {
					t.Fatalf("body=%s err=%v, want request-scoped 400 without body", body, err)
				}
				return
			}
			if err != nil || len(body) == 0 || !gjson.GetBytes(body, "input.0.content.0").Exists() {
				t.Fatalf("body=%s err=%v, want sendable content", body, err)
			}
		})
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
