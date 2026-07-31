package interactions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestInteractionsFunctionArgumentsAreAssembledAtStepStop(t *testing.T) {
	var state any
	start := ConvertInteractionsResponseToGemini(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"weather","arguments":{}}}`),
		&state,
	)
	if len(start) != 0 {
		t.Fatalf("step.start chunks = %d, want 0", len(start))
	}
	for _, fragment := range []string{`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"ci"}}`, `{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"ty\":\"Paris\"}"}}`} {
		chunks := ConvertInteractionsResponseToGemini(
			context.Background(),
			"test-model",
			nil,
			nil,
			[]byte(fragment),
			&state,
		)
		if len(chunks) != 0 {
			t.Fatalf("arguments delta chunks = %d, want 0", len(chunks))
		}
	}
	stop := ConvertInteractionsResponseToGemini(
		context.Background(),
		"test-model",
		nil,
		nil,
		[]byte(`{"event_type":"step.stop","index":1}`),
		&state,
	)
	if len(stop) != 1 {
		t.Fatalf("step.stop chunks = %d, want 1", len(stop))
	}
	if got := gjson.GetBytes(stop[0], "candidates.0.content.parts.0.functionCall.name").String(); got != "weather" {
		t.Fatalf("function name = %q, want weather", got)
	}
	if got := gjson.GetBytes(stop[0], "candidates.0.content.parts.0.functionCall.id").String(); got != "call-1" {
		t.Fatalf("function id = %q, want call-1", got)
	}
	if got := gjson.GetBytes(stop[0], "candidates.0.content.parts.0.functionCall.args.city").String(); got != "Paris" {
		t.Fatalf("function city = %q, want Paris", got)
	}
}
