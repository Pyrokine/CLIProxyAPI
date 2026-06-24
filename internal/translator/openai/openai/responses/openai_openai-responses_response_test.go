package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func openAIResponsesSSEData(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	lines := strings.Split(string(chunk), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected SSE chunk: %q", chunk)
	}
	event := strings.TrimSpace(strings.TrimPrefix(lines[0], "event:"))
	dataLine := strings.TrimSpace(strings.TrimPrefix(lines[1], "data:"))
	if !gjson.Valid(dataLine) {
		t.Fatalf("invalid SSE data JSON: %q", dataLine)
	}
	return event, gjson.Parse(dataLine)
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesReasoningSummaryPath(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"id":"chatcmpl_1","created":1700000000,"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`),
		[]byte(`{"id":"chatcmpl_1","created":1700000000,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}

	var param any
	var output [][]byte
	for _, chunk := range chunks {
		output = append(
			output,
			ConvertOpenAIChatCompletionsResponseToOpenAIResponses(
				context.Background(), "gpt-test", nil, nil, chunk, &param,
			)...,
		)
	}

	for _, chunk := range output {
		event, data := openAIResponsesSSEData(t, chunk)
		if event != "response.output_item.done" || data.Get("item.type").String() != "reasoning" {
			continue
		}
		if got := data.Get("item.summary.0.text").String(); got != "think" {
			t.Fatalf("item.summary.0.text = %q, want think; chunk=%s", got, chunk)
		}
		if data.Get("item.summary.text").Exists() {
			t.Fatalf("unexpected legacy item.summary.text path: %s", chunk)
		}
		return
	}

	t.Fatalf("missing reasoning response.output_item.done event; output=%q", output)
}
