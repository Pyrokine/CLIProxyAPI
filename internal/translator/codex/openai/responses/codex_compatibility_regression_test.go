package responses

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexResponsesCompatibilityRegressions(t *testing.T) {
	t.Run("blank function arguments", func(t *testing.T) {
		input := []byte(`{"input":[{"type":"function_call","arguments":" \n\t"},{"type":"function_call","arguments":"{invalid"},{"type":"function_call","arguments":null},{"type":"custom_tool_call","input":""}]}`)
		out := ConvertOpenAIResponsesRequestToCodex("fixture-model", input, true)
		for path, want := range map[string]string{"input.0.arguments": "{}", "input.1.arguments": "{invalid", "input.3.input": ""} {
			if got := gjson.GetBytes(out, path).String(); got != want {
				t.Fatalf("%s = %q, want %q; %s", path, got, want, out)
			}
		}
		if gjson.GetBytes(out, "input.2.arguments").Type != gjson.Null {
			t.Fatalf("non-string arguments changed: %s", out)
		}
	})
	t.Run("preserve requested search sources", func(t *testing.T) {
		input := []byte(`{"include":["web_search_call.action.sources","file_search_call.results","web_search_call.action.sources"],"input":[]}`)
		out := ConvertOpenAIResponsesRequestToCodex("fixture-model", input, true)
		if got := gjson.GetBytes(out, "include").Raw; got != `["reasoning.encrypted_content","web_search_call.action.sources"]` {
			t.Fatalf("include = %s", got)
		}
	})
	t.Run("cache hints do not survive role conversion", func(t *testing.T) {
		input := []byte(`{"input":[{"role":"system","prompt_cache_breakpoint":true,"content":[{"type":"input_text","text":"system","prompt_cache_breakpoint":true}]},{"type":"function_call_output","call_id":"call_a","prompt_cache_breakpoint":true,"output":[{"type":"input_text","text":"result","prompt_cache_breakpoint":true}]}]}`)
		out := ConvertOpenAIResponsesRequestToCodex("fixture-model", input, true)
		for _, path := range []string{"input.0.prompt_cache_breakpoint", "input.0.content.0.prompt_cache_breakpoint", "input.1.prompt_cache_breakpoint", "input.1.output.0.prompt_cache_breakpoint"} {
			if gjson.GetBytes(out, path).Exists() {
				t.Fatalf("unsupported %s survived: %s", path, out)
			}
		}
		for path, want := range map[string]string{"input.0.role": "developer", "input.0.content.0.text": "system", "input.1.output.0.text": "result", "input.1.call_id": "call_a"} {
			if got := gjson.GetBytes(out, path).String(); got != want {
				t.Fatalf("%s = %q, want %q; %s", path, got, want, out)
			}
		}
	})
	for tier, want := range map[string]string{"fast": "priority", " PRIORITY ": "priority", " ULTRAFAST ": "ultrafast", "standard": "", "flex": ""} {
		t.Run("tier "+tier, func(t *testing.T) {
			out := ConvertOpenAIResponsesRequestToCodex("fixture-model", []byte(fmt.Sprintf(`{"service_tier":%q,"input":[]}`, tier)), true)
			if got := gjson.GetBytes(out, "service_tier").String(); got != want {
				t.Fatalf("tier = %q, want %q; %s", got, want, out)
			}
		})
	}
}

func TestCodexResponsesNativeJSONNonStream(t *testing.T) {
	for _, status := range []string{"completed", "incomplete"} {
		input := []byte(fmt.Sprintf(`{"id":"resp_fixture","object":"response","status":%q,"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`, status))
		out := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "fixture-model", nil, nil, input, nil)
		if !bytes.Equal(out, input) {
			t.Fatalf("native %s response changed or disappeared: %s", status, out)
		}
	}
	out := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "fixture-model", nil, nil, []byte(`{"type":"response.created","response":{"output":[]}}`), nil)
	if len(out) != 0 {
		t.Fatalf("non-terminal event accepted: %s", out)
	}
}
