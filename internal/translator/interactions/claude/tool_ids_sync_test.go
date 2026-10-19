package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeToolIDsAndNamesInInteractions(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}},
			{"type":"tool_use","id":"toolu_2","name":"weather","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_2","content":"offline","is_error":true},
			{"type":"tool_result","tool_use_id":"toolu_1","content":"found"}]}
	]}`)
	out := convertClaudeRequestToInteractions("test-model", input, false, false)
	for index, want := range []struct{ id, name string }{{"toolu_1", "lookup"}, {"toolu_2", "weather"}} {
		item := gjson.GetBytes(out, "input."+string('0'+rune(index)))
		if item.Get("id").String() != want.id || item.Get("call_id").Exists() || item.Get("name").String() != want.name {
			t.Fatalf("function call %d = %s, output = %s", index, item.Raw, out)
		}
	}
	for index, want := range []struct {
		callID, name, result string
		isError              bool
	}{{"toolu_1", "lookup", "found", false}, {"toolu_2", "weather", "offline", true}} {
		item := gjson.GetBytes(out, "input."+string('2'+rune(index)))
		if item.Get("call_id").String() != want.callID || item.Get("id").Exists() || item.Get("name").String() != want.name || item.Get("result").String() != want.result || item.Get("is_error").Bool() != want.isError {
			t.Fatalf("function result %d = %s, output = %s", index, item.Raw, out)
		}
	}
}
