package helps

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestDevinToolCallIDsCarryNativeValues(t *testing.T) {
	for _, id := range []string{"call_1", "0:foo", "foo/bar", "é:工具", "cpa_tid_v1_bare", "cpa_tid_v1_MDpmb28"} {
		encoded := EncodeDevinToolCallID(id)
		if !portableToolCallID(encoded) {
			t.Errorf("encoded ID %q is not portable", encoded)
		}
		if decoded := DecodeDevinToolCallID(encoded); decoded != id {
			t.Errorf("ID %q: decode(encode) = %q", id, decoded)
		}
	}
	for _, id := range []string{"", "call_1", "cpa_tid_v1_!!!!", "cpa_tid_v1_Zm9v", "cpa_tid_v1__w"} {
		if decoded := DecodeDevinToolCallID(id); decoded != id {
			t.Errorf("ordinary or malformed ID %q decoded to %q", id, decoded)
		}
	}
	prompts := []DevinPrompt{
		{ToolCalls: []DevinToolCall{{ID: EncodeDevinToolCallID("0:foo")}}},
		{ToolCallID: EncodeDevinToolCallID("0:foo")},
	}
	RestoreDevinToolCallIDs(prompts)
	if prompts[0].ToolCalls[0].ID != "0:foo" || prompts[1].ToolCallID != "0:foo" {
		t.Fatalf("native ID not restored in call and result: %#v", prompts)
	}
}

func TestNormalizeClaudeToolCallIDsPreservesToolPairingAndOriginalText(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"assistant","content":[{"type":"text","text":"toolu/1"},{"type":"tool_use","id":"toolu/1","name":"a","input":{}},{"type":"tool_use","id":"toolu_1","name":"b","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu/1","content":"ok"}]}]}`)
	normalized := NormalizeClaudeToolCallIDs(body)
	if !gjson.ValidBytes(normalized) || bytes.Equal(normalized, body) {
		t.Fatalf("invalid IDs not repaired: %s", normalized)
	}
	use := gjson.GetBytes(normalized, "messages.0.content.1.id").String()
	result := gjson.GetBytes(normalized, "messages.1.content.0.tool_use_id").String()
	if !portableToolCallID(use) || use != result || use == "toolu_1" || use == "toolu/1" {
		t.Fatalf("tool call/result IDs differ or collide: %q and %q", use, result)
	}
	if gjson.GetBytes(normalized, "messages.0.content.2.id").String() != "toolu_1" || gjson.GetBytes(normalized, "messages.0.content.0.text").String() != "toolu/1" {
		t.Fatalf("legal ID or ordinary text was changed: %s", normalized)
	}
	if second := NormalizeClaudeToolCallIDs(normalized); !bytes.Equal(second, normalized) {
		t.Fatalf("normalization was not idempotent: %s", second)
	}
}
