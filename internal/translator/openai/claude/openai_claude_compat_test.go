package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToOpenAIWithCompatPreservesOpaqueThinkingWithToolCalls(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"claude#opaque"},{"type":"text","text":"Reading files."},{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"main.go"}}]}]}`)

	withoutCompat := convertClaudeRequestToOpenAIDefault("deepseek-v4", payload, false)
	if gjson.GetBytes(withoutCompat, "messages.0.reasoning_content").Exists() {
		t.Fatalf("default translation preserved incompatible reasoning: %s", withoutCompat)
	}

	withCompat := ConvertClaudeRequestToOpenAIWithCompat("deepseek-v4", payload, false)
	assistant := gjson.GetBytes(withCompat, "messages.0")
	if assistant.Get("reasoning_content").String() != "reason" {
		t.Fatalf("compat translation missing reasoning_content: %s", withCompat)
	}
	if !assistant.Get("tool_calls").Exists() {
		t.Fatalf("compat translation missing tool_calls: %s", withCompat)
	}
}
