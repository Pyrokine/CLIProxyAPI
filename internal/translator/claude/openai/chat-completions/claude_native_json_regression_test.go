package chat_completions

import (
	"context"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeNativeJSONUsesRegisteredNonStreamConverter(t *testing.T) {
	body := []byte(`{"id":"message_fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"thought"},{"type":"text","text":"second"},{"type":"tool_use","id":"call_a","name":"lookup","input":{"query":"fixture"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":4,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}`)
	var param any
	out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-model", nil, nil, body, &param)
	for path, want := range map[string]string{"id": "message_fixture", "model": "claude-fixture", "choices.0.message.content": "firstsecond", "choices.0.message.reasoning_content": "thought", "choices.0.message.tool_calls.0.id": "call_a", "choices.0.message.tool_calls.0.function.name": "lookup", "choices.0.message.tool_calls.0.function.arguments": `{"query":"fixture"}`, "choices.0.finish_reason": "tool_calls"} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q; %s", path, got, want, out)
		}
	}
	for path, want := range map[string]int64{"usage.prompt_tokens": 10, "usage.completion_tokens": 4, "usage.total_tokens": 14, "usage.prompt_tokens_details.cached_tokens": 2, "usage.prompt_tokens_details.cached_creation_tokens": 3} {
		if got := gjson.GetBytes(out, path).Int(); got != want {
			t.Errorf("%s = %d, want %d; %s", path, got, want, out)
		}
	}
}
