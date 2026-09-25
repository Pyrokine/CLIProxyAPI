package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestRequestModelNamePrefersOriginalRequest(t *testing.T) {
	original := []byte(`{"model":"original-model"}`)
	translated := []byte(`{"model":"translated-model"}`)

	if got := RequestModelName(original, translated); got != "original-model" {
		t.Fatalf("model = %q, want original-model", got)
	}
}

func TestRequestModelNameSupportsWrappedRequest(t *testing.T) {
	request := []byte(`{"request":{"model":"wrapped-model"}}`)

	if got := RequestModelName(nil, request); got != "wrapped-model" {
		t.Fatalf("model = %q, want wrapped-model", got)
	}
}

func TestRequestModelNameSkipsInvalidAndEmptyModels(t *testing.T) {
	original := []byte(`{"model":"   "}`)
	translated := []byte(`{"request":{"model":"translated-model"}}`)

	if got := RequestModelName(original, translated); got != "translated-model" {
		t.Fatalf("model = %q, want translated-model", got)
	}
}

func TestGeminiToolCallIDPrecedence(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: `{"id":"id-a","call_id":"id-b","callId":"id-c"}`, want: "id-a"},
		{raw: `{"call_id":"id-b","callId":"id-c"}`, want: "id-b"},
		{raw: `{"callId":"id-c"}`, want: "id-c"},
	}
	for _, testCase := range tests {
		if got := GeminiToolCallID(gjson.Parse(testCase.raw)); got != testCase.want {
			t.Fatalf("GeminiToolCallID(%s) = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}

func TestGeminiToolCallQueueMatchesExplicitNameAndFIFO(t *testing.T) {
	var queue GeminiToolCallQueue
	queue.Add("alpha", "call-a1")
	queue.Add("beta", "call-b")
	queue.Add("alpha", "call-a2")

	if got := queue.Match("alpha", "call-a2"); got != "call-a2" {
		t.Fatalf("explicit match = %q, want call-a2", got)
	}
	if got := queue.Match("alpha", ""); got != "call-a1" {
		t.Fatalf("name match = %q, want call-a1", got)
	}
	if got := queue.Match("", ""); got != "call-b" {
		t.Fatalf("FIFO match = %q, want call-b", got)
	}
	if got := queue.Match("alpha", "external"); got != "external" {
		t.Fatalf("unmatched explicit ID = %q, want external", got)
	}
}

func TestContentHasGeminiFunctionResponseAliases(t *testing.T) {
	for _, raw := range []string{
		`{"parts":[{"functionResponse":{"name":"lookup"}}]}`,
		`{"parts":[{"function_response":{"name":"lookup"}}]}`,
	} {
		if !ContentHasGeminiFunctionResponse([]byte(raw)) {
			t.Fatalf("function response was not detected: %s", raw)
		}
	}
	if ContentHasGeminiFunctionResponse([]byte(`{"parts":[{"text":"hello"}]}`)) {
		t.Fatal("text content was detected as a function response")
	}
}

func TestAntigravityToolNameRoundTrip(t *testing.T) {
	tests := []struct {
		client   string
		upstream string
	}{
		{client: "read_file", upstream: "external_read_file"},
		{client: "external_read_file", upstream: "external_external_read_file"},
		{client: "external_external_read_file", upstream: "external_external_external_read_file"},
		{client: "external_lookup", upstream: "external_lookup"},
	}
	for _, test := range tests {
		if got := AntigravityToolNameToUpstream(test.client); got != test.upstream {
			t.Fatalf("AntigravityToolNameToUpstream(%q) = %q, want %q", test.client, got, test.upstream)
		}
		if got := AntigravityUpstreamToolNameToClient(test.upstream); got != test.client {
			t.Fatalf("AntigravityUpstreamToolNameToClient(%q) = %q, want %q", test.upstream, got, test.client)
		}
	}
}

func TestGeminiToolCallQueuePreservesPendingCallsAcrossUnknownExplicitResult(t *testing.T) {
	var queue GeminiToolCallQueue
	queue.Add("alpha", "call-a")
	queue.Add("beta", "call-b")

	if got := queue.Match("unknown", "external"); got != "external" {
		t.Fatalf("explicit match = %q, want external", got)
	}
	pending := queue.PendingIDs()
	if len(pending) != 2 || pending[0] != "call-a" || pending[1] != "call-b" {
		t.Fatalf("pending IDs = %v, want [call-a call-b]", pending)
	}
	pending[0] = "mutated"
	if got := queue.PendingIDs()[0]; got != "call-a" {
		t.Fatalf("queue was modified through returned slice: %q", got)
	}
	if got := queue.Match("beta", "call-b"); got != "call-b" {
		t.Fatalf("partial explicit match = %q, want call-b", got)
	}
	remaining := queue.PendingIDs()
	if len(remaining) != 1 || remaining[0] != "call-a" {
		t.Fatalf("remaining IDs = %v, want [call-a]", remaining)
	}
}

func TestDeduplicateToolResultsByRawIDUsesLastValueAtFirstPosition(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"tool_result","raw_id":"call.a","value":"first"},
		{"type":"text","text":"between"},
		{"type":"tool_result","raw_id":"call_a","value":"separate"},
		{"type":"image","source":"image"},
		{"type":"tool_result","raw_id":"call.a","value":"last"},
		{"type":"tool_result","raw_id":"","value":"empty-one"},
		{"type":"tool_result","raw_id":"","value":"empty-two"}
	]`).Array()

	deduplicated := DeduplicateToolResultsByRawID(items, func(item gjson.Result) (string, bool) {
		if item.Get("type").String() != "tool_result" {
			return "", false
		}
		return item.Get("raw_id").String(), true
	})
	if len(deduplicated) != 6 {
		t.Fatalf("item count = %d, want 6", len(deduplicated))
	}
	if got := deduplicated[0].Get("value").String(); got != "last" {
		t.Fatalf("first result value = %q, want last", got)
	}
	if got := deduplicated[1].Get("text").String(); got != "between" {
		t.Fatalf("text moved or changed: %q", got)
	}
	if got := deduplicated[2].Get("raw_id").String(); got != "call_a" {
		t.Fatalf("sanitize-colliding raw ID was merged: %q", got)
	}
	if got := deduplicated[3].Get("source").String(); got != "image" {
		t.Fatalf("image moved or changed: %q", got)
	}
	if got := deduplicated[4].Get("value").String(); got != "empty-one" {
		t.Fatalf("first empty-ID result = %q, want empty-one", got)
	}
	if got := deduplicated[5].Get("value").String(); got != "empty-two" {
		t.Fatalf("second empty-ID result = %q, want empty-two", got)
	}
}
