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

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesCreatedIncludesOriginalRequestModel(t *testing.T) {
	originalRequest := []byte(`{"model":"original-openai-model"}`)
	translatedRequest := []byte(`{"model":"translated-openai-model"}`)
	chunk := []byte(`data: {"id":"chatcmpl_1","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`)

	var state any
	outputs := convertOpenAIChatCompletionsResponseToOpenAIResponses(
		context.Background(),
		"fallback-model",
		originalRequest,
		translatedRequest,
		chunk,
		&state,
	)

	models := make(map[string]string)
	for _, output := range outputs {
		event, data := openAIResponsesSSEData(t, output)
		models[event] = data.Get("response.model").String()
	}
	for _, event := range []string{"response.created", "response.in_progress"} {
		if got := models[event]; got != "original-openai-model" {
			t.Fatalf("%s model = %q, want original-openai-model", event, got)
		}
	}
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
			convertOpenAIChatCompletionsResponseToOpenAIResponses(
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

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesResponseCompletedWaitsForDone(t *testing.T) {
	request := []byte(`{"model":"gpt-test"}`)
	tests := []struct {
		name           string
		chunks         []string
		doneInputIndex int
		wantUsage      bool
	}{
		{
			name: "no finish reason",
			chunks: []string{
				`data: {"id":"response-no-finish","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
				`data: [DONE]`,
			},
			doneInputIndex: 1,
		},
		{
			name: "late usage after finish reason",
			chunks: []string{
				`data: {"id":"response-late-usage","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
				`data: {"id":"response-late-usage","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: {"id":"response-late-usage","object":"chat.completion.chunk","created":1,"choices":[],"usage":{"prompt_tokens":11,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens":7,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 3,
			wantUsage:      true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				var state any
				completedCount := 0
				completedInputIndex := -1
				itemDoneCount := 0
				var completed gjson.Result
				for inputIndex, chunk := range test.chunks {
					for _, eventChunk := range convertOpenAIChatCompletionsResponseToOpenAIResponses(
						context.Background(), "gpt-test", request, request, []byte(chunk), &state,
					) {
						event, data := openAIResponsesSSEData(t, eventChunk)
						switch event {
						case "response.output_item.done":
							if data.Get("item.type").String() == "message" {
								itemDoneCount++
							}
						case "response.completed":
							completedCount++
							completedInputIndex = inputIndex
							completed = data
						}
					}
				}

				if completedCount != 1 {
					t.Fatalf("response.completed count = %d, want 1", completedCount)
				}
				if completedInputIndex != test.doneInputIndex {
					t.Fatalf("response.completed input index = %d, want %d", completedInputIndex, test.doneInputIndex)
				}
				if itemDoneCount != 1 {
					t.Fatalf("message item done count = %d, want 1", itemDoneCount)
				}
				if got := completed.Get("response.output.0.content.0.text").String(); got != "hello" {
					t.Fatalf("completed output text = %q, want hello; completed=%s", got, completed.Raw)
				}
				if test.wantUsage {
					if got := completed.Get("response.usage.input_tokens").Int(); got != 11 {
						t.Fatalf("input tokens = %d, want 11", got)
					}
					if got := completed.Get("response.usage.input_tokens_details.cached_tokens").Int(); got != 3 {
						t.Fatalf("cached tokens = %d, want 3", got)
					}
					if got := completed.Get("response.usage.output_tokens").Int(); got != 7 {
						t.Fatalf("output tokens = %d, want 7", got)
					}
					if got := completed.Get("response.usage.total_tokens").Int(); got != 18 {
						t.Fatalf("total tokens = %d, want 18", got)
					}
				} else if completed.Get("response.usage").Exists() {
					t.Fatalf("unexpected usage without upstream usage: %s", completed.Raw)
				}

				repeated := convertOpenAIChatCompletionsResponseToOpenAIResponses(
					context.Background(), "gpt-test", request, request, []byte(`data: [DONE]`), &state,
				)
				if len(repeated) != 0 {
					t.Fatalf("repeated [DONE] emitted %d events, want none", len(repeated))
				}
			},
		)
	}
}
