package chat_completions

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOpenAIChatToolIDsAndNamesInInteractions(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}},
			{"id":"call_2","type":"function","function":{"name":"weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_2","content":"forecast"},
		{"role":"tool","tool_call_id":"call_1","content":"found"}
	]}`)
	out := convertOpenAIRequestToInteractions("test-model", input, false)
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 4 {
		t.Fatalf("tool step count = %d, output = %s", len(items), out)
	}
	for index, want := range []struct{ id, name string }{{"call_1", "lookup"}, {"call_2", "weather"}} {
		if items[index].Get("id").String() != want.id || items[index].Get("call_id").Exists() ||
			items[index].Get("name").String() != want.name {
			t.Fatalf("function call %d = %s, output = %s", index, items[index].Raw, out)
		}
	}
	for index, want := range []struct{ id, name string }{{"call_2", "weather"}, {"call_1", "lookup"}} {
		item := items[index+2]
		if item.Get("call_id").String() != want.id || item.Get("id").Exists() || item.Get("name").String() != want.name {
			t.Fatalf("function result %d = %s, output = %s", index, item.Raw, out)
		}
	}
}

func TestOpenAIChatResponseToolCallsOmitInteractionsCallID(t *testing.T) {
	step, ok := openAIToolCallToInteractionsStep(gjson.Parse(`{"id":"call_1","function":{"name":"lookup","arguments":"{}"}}`), false)
	if !ok || gjson.GetBytes(step, "id").String() != "call_1" || gjson.GetBytes(step, "call_id").Exists() {
		t.Fatalf("function call step = %s, valid = %t", step, ok)
	}
	state := &openAIToInteractionsStreamState{}
	events := appendInteractionsStepStart(nil, state, "function_call", gjson.ParseBytes(step))
	if len(events) != 1 || bytes.Contains(events[0], []byte(`"call_id"`)) {
		t.Fatalf("function call SSE = %q", events)
	}
}
