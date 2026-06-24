package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeXAIToolChoiceForToolsDropsEmptyToolsAndOrphanedChoice(t *testing.T) {
	body := []byte(`{"model":"grok-4.3","tools":[],"tool_choice":"auto","parallel_tool_calls":true}`)
	body = normalizeXAIToolChoiceForTools(body)

	if gjson.GetBytes(body, "tools").Exists() {
		t.Fatalf("tools still exists: %s", body)
	}
	if gjson.GetBytes(body, "tool_choice").Exists() {
		t.Fatalf("tool_choice still exists: %s", body)
	}
	if gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		t.Fatalf("parallel_tool_calls still exists: %s", body)
	}
}
