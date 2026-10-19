package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSplitGeminiFunctionResponseTurnsPreservesToolAdjacency(t *testing.T) {
	model := []byte(`{"role":"model","parts":[{"functionCall":{"name":"fetch","args":{}}}]}`)
	reminder := []byte(`{"role":"user","parts":[{"text":"reminder"}]}`)
	mixed := []byte(`{"role":"user","parts":[{"functionResponse":{"name":"fetch","response":{"ok":true}}},{"text":"after result"}]}`)
	response := SplitGeminiFunctionResponseTurns([][]byte{model, reminder, mixed})
	if len(response) != 4 {
		t.Fatalf("turn count = %d, want model, result, reminder, text", len(response))
	}
	if got := gjson.GetBytes(response[1], "parts.0.functionResponse.name").String(); got != "fetch" {
		t.Fatalf("tool result was not adjacent to model call: %s", response[1])
	}
	if got := gjson.GetBytes(response[2], "parts.0.text").String(); got != "reminder" {
		t.Fatalf("reminder changed: %s", response[2])
	}
	if got := gjson.GetBytes(response[3], "parts.0.text").String(); got != "after result" {
		t.Fatalf("tool response text changed: %s", response[3])
	}
	merged := MergeAdjacentGeminiUserContents(response)
	if len(merged) != 3 || !ContentHasGeminiFunctionResponse(merged[1]) {
		t.Fatalf("tool result was merged into plain user content: %q", merged)
	}
}

func TestGeminiFunctionResultDoesNotInterpretSchemaReferenceAsMedia(t *testing.T) {
	part := []byte(`{"functionResponse":{"name":"fetch","response":{}}}`)
	result := gjson.Parse(`{"nested":{"$ref":"#/components/schemas/ToolResult"}}`)
	converted := SetGeminiFunctionResponseResult(part, "functionResponse.response", result)
	if got := gjson.GetBytes(converted, "functionResponse.response.result").String(); got != result.Raw {
		t.Fatalf("schema reference is not opaque JSON text: %s", converted)
	}
	plain := SetGeminiFunctionResponseResult(part, "functionResponse.response", gjson.Parse(`{"ok":true}`))
	if !gjson.GetBytes(plain, "functionResponse.response.ok").Bool() {
		t.Fatalf("ordinary response was stringified: %s", plain)
	}
}
