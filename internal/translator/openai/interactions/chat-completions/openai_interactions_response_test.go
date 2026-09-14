package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestInteractionsToolCallUsesDenseOpenAIStreamIndex(t *testing.T) {
	var state any
	convertInteractionsResponseToOpenAI(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.start","index":0,"step":{"type":"thought"}}`),
		&state,
	)
	start := convertInteractionsResponseToOpenAI(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"tool"}}`),
		&state,
	)
	if len(start) != 1 {
		t.Fatalf("function start chunks = %d, want 1", len(start))
	}
	if got := gjson.GetBytes(start[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("function start index = %d, want 0", got)
	}

	delta := convertInteractionsResponseToOpenAI(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{}"}}`),
		&state,
	)
	if len(delta) != 1 {
		t.Fatalf("function delta chunks = %d, want 1", len(delta))
	}
	if got := gjson.GetBytes(delta[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("function delta index = %d, want 0", got)
	}
}
