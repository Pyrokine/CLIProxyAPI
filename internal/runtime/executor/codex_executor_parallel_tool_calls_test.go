package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeCodexParallelToolCallsForTools_DropsWhenToolsEmpty(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[],"parallel_tool_calls":false,"input":"hi"}`)
	out := normalizeCodexParallelToolCallsForTools(body)
	if gjson.GetBytes(out, "parallel_tool_calls").Exists() {
		t.Fatalf("parallel_tool_calls should be removed when tools are empty: %s", out)
	}
	if !gjson.GetBytes(out, "tools").IsArray() || len(gjson.GetBytes(out, "tools").Array()) != 0 {
		t.Fatalf("empty tools should be preserved: %s", out)
	}
}

func TestNormalizeCodexParallelToolCalls_ResponsesLiteMetadataForcesFalse(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-luna","tools":[{"type":"function","name":"lookup"}],` +
		`"parallel_tool_calls":true,"client_metadata":` +
		`{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"input":"hi"}`)
	out := normalizeCodexParallelToolCalls(body, nil)
	parallelToolCalls := gjson.GetBytes(out, "parallel_tool_calls")
	if !parallelToolCalls.Exists() || parallelToolCalls.Type != gjson.False {
		t.Fatalf("responses-lite parallel_tool_calls should be false: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "lookup" {
		t.Fatalf("responses-lite lost its declared tool: %s", out)
	}
}
