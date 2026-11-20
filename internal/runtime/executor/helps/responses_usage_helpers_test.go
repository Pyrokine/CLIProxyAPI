package helps

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnsureResponsesUsageDetails_NonStreamJSON(t *testing.T) {
	raw := []byte((`{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":84,"` +
		`output_tokens":16,"total_tokens":100}}`))
	got := EnsureResponsesUsageDetails(raw)

	if !gjson.GetBytes(got, "usage.output_tokens_details").Exists() {
		t.Fatalf("expected usage.output_tokens_details to exist, got %s", string(got))
	}
	if gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int() != 0 {
		t.Fatalf(
			"expected usage.output_tokens_details.reasoning_tokens == 0, got %d",
			gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int(),
		)
	}
	if !gjson.GetBytes(got, "usage.input_tokens_details").Exists() {
		t.Fatalf("expected usage.input_tokens_details to exist, got %s", string(got))
	}
	if gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int() != 0 {
		t.Fatalf(
			"expected usage.input_tokens_details.cached_tokens == 0, got %d",
			gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int(),
		)
	}
}

func TestEnsureResponsesUsageDetails_NonStreamJSONWithDataSubstring(t *testing.T) {
	raw := []byte((`{"id":"resp_1","object":"response","status":"completed","output":[{"ty` +
		`pe":"message","content":[{"type":"text","text":"data:image/png;base64,` +
		`iVBORw0KGgoAAAANSUhEUg"}]}],"usage":{"input_tokens":84,"output_tokens"` +
		`:16,"total_tokens":100}}`))
	got := EnsureResponsesUsageDetails(raw)

	if !gjson.GetBytes(got, "usage.output_tokens_details").Exists() {
		t.Fatalf("expected usage.output_tokens_details to exist, got %s", string(got))
	}
	if gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int() != 0 {
		t.Fatalf(
			"expected usage.output_tokens_details.reasoning_tokens == 0, got %d",
			gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int(),
		)
	}
	if !gjson.GetBytes(got, "usage.input_tokens_details").Exists() {
		t.Fatalf("expected usage.input_tokens_details to exist, got %s", string(got))
	}
	if gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int() != 0 {
		t.Fatalf(
			"expected usage.input_tokens_details.cached_tokens == 0, got %d",
			gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int(),
		)
	}
}

func TestEnsureResponsesUsageDetails_SSEData(t *testing.T) {
	raw := []byte((`data: {"type":"response.completed","response":{"id":"resp_1","usage":{` +
		`"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`))
	got := EnsureResponsesUsageDetails(raw)

	if !bytes.HasPrefix(got, []byte("data: ")) {
		t.Fatalf("expected data: prefix preserved, got %s", string(got))
	}
	jsonBody := bytes.TrimPrefix(got, []byte("data: "))
	if !gjson.GetBytes(jsonBody, "response.usage.output_tokens_details").Exists() {
		t.Fatalf("expected response.usage.output_tokens_details to exist, got %s", string(got))
	}
	if gjson.GetBytes(jsonBody, "response.usage.output_tokens_details.reasoning_tokens").Int() != 0 {
		t.Fatalf(
			"expected reasoning_tokens == 0, got %d",
			gjson.GetBytes(jsonBody, "response.usage.output_tokens_details.reasoning_tokens").Int(),
		)
	}
	if !gjson.GetBytes(jsonBody, "response.usage.input_tokens_details").Exists() {
		t.Fatalf("expected response.usage.input_tokens_details to exist, got %s", string(got))
	}
	if gjson.GetBytes(jsonBody, "response.usage.input_tokens_details.cached_tokens").Int() != 0 {
		t.Fatalf(
			"expected cached_tokens == 0, got %d",
			gjson.GetBytes(jsonBody, "response.usage.input_tokens_details.cached_tokens").Int(),
		)
	}
}

func TestEnsureResponsesUsageDetails_SSEEventDataMultiLine(t *testing.T) {
	raw := []byte(("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"" +
		"resp_1\",\"usage\":{\"input_tokens\":84,\"output_tokens\":16,\"total_tokens\":100}}}\n\n"))
	got := EnsureResponsesUsageDetails(raw)

	if !bytes.HasPrefix(got, []byte("event: response.completed\n")) {
		t.Fatalf("expected event header preserved, got %s", string(got))
	}

	for line := range bytes.SplitSeq(got, []byte("\n")) {
		if after, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			jsonBody := after
			if !gjson.GetBytes(jsonBody, "response.usage.output_tokens_details").Exists() {
				t.Fatalf(
					"expected response.usage.output_tokens_details to exist in multi-line frame, got %s", string(got),
				)
			}
			if gjson.GetBytes(jsonBody, "response.usage.output_tokens_details.reasoning_tokens").Int() != 0 {
				t.Fatalf(
					"expected reasoning_tokens == 0, got %d",
					gjson.GetBytes(jsonBody, "response.usage.output_tokens_details.reasoning_tokens").Int(),
				)
			}
			if !gjson.GetBytes(jsonBody, "response.usage.input_tokens_details").Exists() {
				t.Fatalf(
					"expected response.usage.input_tokens_details to exist in multi-line frame, got %s", string(got),
				)
			}
			if gjson.GetBytes(jsonBody, "response.usage.input_tokens_details.cached_tokens").Int() != 0 {
				t.Fatalf(
					"expected cached_tokens == 0, got %d",
					gjson.GetBytes(jsonBody, "response.usage.input_tokens_details.cached_tokens").Int(),
				)
			}
		}
	}
}

func TestEnsureResponsesUsageDetails_PreservesExistingDetails(t *testing.T) {
	raw := []byte((`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"` +
		`input_tokens_details":{"cached_tokens":3},"output_tokens":4,"output_tokens_details":{"` +
		`reasoning_tokens":2},"total_tokens":14}}}`))
	got := EnsureResponsesUsageDetails(raw)

	jsonBody := bytes.TrimPrefix(got, []byte("data: "))
	if gjson.GetBytes(jsonBody, "response.usage.output_tokens_details.reasoning_tokens").Int() != 2 {
		t.Fatalf(
			"expected reasoning_tokens == 2, got %d",
			gjson.GetBytes(jsonBody, "response.usage.output_tokens_details.reasoning_tokens").Int(),
		)
	}
	if gjson.GetBytes(jsonBody, "response.usage.input_tokens_details.cached_tokens").Int() != 3 {
		t.Fatalf(
			"expected cached_tokens == 3, got %d",
			gjson.GetBytes(jsonBody, "response.usage.input_tokens_details.cached_tokens").Int(),
		)
	}
}

func TestEnsureResponsesUsageDetails_HandlesNullOrEmptyDetails(t *testing.T) {
	raw := []byte((`{"id":"resp_1","usage":{"input_tokens":10,"input_tokens_details":null,` +
		`"output_tokens":4,"output_tokens_details":{},"total_tokens":14}}`))
	got := EnsureResponsesUsageDetails(raw)

	if gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int() != 0 {
		t.Fatalf(
			"expected reasoning_tokens == 0, got %d",
			gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int(),
		)
	}
	if gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int() != 0 {
		t.Fatalf(
			"expected cached_tokens == 0, got %d",
			gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int(),
		)
	}
}

func TestEnsureResponsesUsageDetails_NonJSONAndDone(t *testing.T) {
	cases := [][]byte{
		[]byte("data: [DONE]"),
		[]byte("[DONE]"),
		[]byte(": keepalive"),
		[]byte(""),
		[]byte(`{"type":"response.output_item.added"}`),
	}
	for _, c := range cases {
		got := EnsureResponsesUsageDetails(c)
		if !bytes.Equal(got, c) {
			t.Fatalf("expected unchanged for %q, got %q", string(c), string(got))
		}
	}
}
