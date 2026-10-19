package responses

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
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

func TestOpenAIChatToolArgumentsKeepHTMLCharactersInResponses(t *testing.T) {
	arguments := `{"command":"2>&1 <work>"}`
	chatResponse := []byte(`{"id":"chatcmpl_1","created":1,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"command\":\"2>&1 <work>\"}"}}]},"finish_reason":"tool_calls"}]}`)
	out := convertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-test", nil, nil, chatResponse, nil)
	if got := gjson.GetBytes(out, "output.0.arguments").String(); got != arguments ||
		!strings.Contains(string(out), `2>&1 <work>`) {
		t.Fatalf("non-stream tool arguments = %q in %s", got, out)
	}

	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_1","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"command\":\"2>&1 <work>\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		[]byte(`data: [DONE]`),
	}
	var state any
	counts := make(map[string]int)
	for _, chunk := range chunks {
		for _, output := range convertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-test", nil, nil, chunk, &state) {
			event, _ := openAIResponsesSSEData(t, output)
			switch event {
			case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed":
				counts[event]++
				if !strings.Contains(string(output), `2>&1 <work>`) {
					t.Fatalf("%s escaped tool argument HTML: %s", event, output)
				}
			}
		}
	}
	for _, event := range []string{"response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed"} {
		if counts[event] != 1 {
			t.Fatalf("%s count=%d, want one", event, counts[event])
		}
	}
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

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesIncompleteToolStreamDoesNotFinalize(t *testing.T) {
	request := []byte(`{"model":"gpt-test"}`)
	tests := []struct {
		name  string
		chunk string
	}{
		{
			name:  "empty arguments",
			chunk: `data: {"id":"interrupted-empty","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		},
		{
			name:  "partial arguments",
			chunk: `data: {"id":"interrupted-partial","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"hel"}}]},"finish_reason":null}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var state any
			chunks := []string{test.chunk, `data: [DONE]`}
			for _, chunk := range chunks {
				for _, output := range convertOpenAIChatCompletionsResponseToOpenAIResponses(
					context.Background(), "gpt-test", request, request, []byte(chunk), &state,
				) {
					event, _ := openAIResponsesSSEData(t, output)
					switch event {
					case "response.completed", "response.incomplete", "response.output_item.done",
						"response.function_call_arguments.done", "response.custom_tool_call_input.done":
						t.Fatalf("incomplete tool stream emitted terminal event %q: %s", event, output)
					}
				}
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesIncompleteFinishReasons(t *testing.T) {
	request := []byte(`{"model":"gpt-test"}`)
	tests := []struct {
		finishReason string
		wantReason   string
	}{
		{finishReason: "length", wantReason: "max_output_tokens"},
		{finishReason: "max_tokens", wantReason: "max_output_tokens"},
		{finishReason: "content_filter", wantReason: "content_filter"},
	}

	for _, test := range tests {
		t.Run(test.finishReason, func(t *testing.T) {
			var state any
			chunks := []string{
				`data: {"id":"incomplete-tool","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"incomplete-tool","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"` + test.finishReason + `"}]}`,
				`data: [DONE]`,
			}
			var itemDone, incomplete gjson.Result
			completedCount := 0
			for _, chunk := range chunks {
				for _, output := range convertOpenAIChatCompletionsResponseToOpenAIResponses(
					context.Background(), "gpt-test", request, request, []byte(chunk), &state,
				) {
					event, data := openAIResponsesSSEData(t, output)
					switch event {
					case "response.output_item.done":
						if data.Get("item.type").String() == "function_call" {
							itemDone = data
						}
					case "response.incomplete":
						incomplete = data
					case "response.completed":
						completedCount++
					}
				}
			}
			if completedCount != 0 {
				t.Fatalf("response.completed count = %d, want 0", completedCount)
			}
			if !itemDone.Exists() {
				t.Fatal("missing function_call response.output_item.done")
			}
			if got := itemDone.Get("item.status").String(); got != "incomplete" {
				t.Fatalf("item.status = %q, want incomplete", got)
			}
			if got := itemDone.Get("item.arguments").String(); got == "{}" {
				t.Fatalf("item.arguments = %q, must not synthesize empty object", got)
			}
			if !incomplete.Exists() {
				t.Fatal("missing response.incomplete")
			}
			if got := incomplete.Get("response.incomplete_details.reason").String(); got != test.wantReason {
				t.Fatalf("incomplete reason = %q, want %q", got, test.wantReason)
			}
			if got := incomplete.Get("response.output.0.status").String(); got != "incomplete" {
				t.Fatalf("response.output.0.status = %q, want incomplete", got)
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesEmptyToolCallsDoNotCloseContent(t *testing.T) {
	request := []byte(`{"model":"gpt-test"}`)
	chunks := []string{
		`data: {"id":"empty-tools","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"think ","tool_calls":[]},"finish_reason":null}]}`,
		`data: {"id":"empty-tools","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"more","tool_calls":[]},"finish_reason":null}]}`,
		`data: {"id":"empty-tools","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"hello ","tool_calls":[]},"finish_reason":null}]}`,
		`data: {"id":"empty-tools","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"world","tool_calls":[]},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}

	var state any
	var reasoningDone, messageDone, completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range convertOpenAIChatCompletionsResponseToOpenAIResponses(
			context.Background(), "gpt-test", request, request, []byte(chunk), &state,
		) {
			event, data := openAIResponsesSSEData(t, output)
			switch event {
			case "response.output_item.done":
				switch data.Get("item.type").String() {
				case "reasoning":
					reasoningDone = data
				case "message":
					messageDone = data
				}
			case "response.completed":
				completed = data
			}
		}
	}
	if got := reasoningDone.Get("item.summary.0.text").String(); got != "think more" {
		t.Fatalf("reasoning text = %q, want think more", got)
	}
	if got := messageDone.Get("item.content.0.text").String(); got != "hello world" {
		t.Fatalf("message text = %q, want hello world", got)
	}
	if got := completed.Get("response.output.1.content.0.text").String(); got != "hello world" {
		t.Fatalf("completed message text = %q, want hello world", got)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStreamIncompleteFinishReasons(t *testing.T) {
	for _, test := range []struct {
		finishReason string
		wantReason   string
	}{
		{finishReason: "length", wantReason: "max_output_tokens"},
		{finishReason: "max_tokens", wantReason: "max_output_tokens"},
		{finishReason: "content_filter", wantReason: "content_filter"},
	} {
		t.Run(test.finishReason, func(t *testing.T) {
			raw := []byte(`{"id":"chatcmpl_1","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"partial","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"` + test.finishReason + `"}]}`)
			out := convertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(
				context.Background(), "gpt-test", nil, nil, raw, nil,
			)
			data := gjson.ParseBytes(out)
			if got := data.Get("status").String(); got != "incomplete" {
				t.Fatalf("status = %q, want incomplete; output=%s", got, out)
			}
			if got := data.Get("incomplete_details.reason").String(); got != test.wantReason {
				t.Fatalf("incomplete reason = %q, want %q; output=%s", got, test.wantReason, out)
			}
			for _, output := range data.Get("output").Array() {
				if got := output.Get("status").String(); got != "incomplete" {
					t.Fatalf("output status = %q, want incomplete; output=%s", got, out)
				}
			}
		})
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

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStreamReasoningFallback(t *testing.T) {
	tests := []struct {
		name          string
		message       string
		request       string
		wantReasoning bool
		wantText      string
	}{
		{
			name:          "reasoning content",
			message:       `{"content":"answer","reasoning_content":"primary"}`,
			wantReasoning: true,
			wantText:      "primary",
		},
		{
			name:          "reasoning fallback",
			message:       `{"content":"answer","reasoning":"fallback"}`,
			wantReasoning: true,
			wantText:      "fallback",
		},
		{
			name:          "reasoning content priority",
			message:       `{"content":"answer","reasoning_content":"primary","reasoning":"fallback"}`,
			wantReasoning: true,
			wantText:      "primary",
		},
		{
			name:          "empty reasoning content fallback",
			message:       `{"content":"answer","reasoning_content":"","reasoning":"fallback"}`,
			wantReasoning: true,
			wantText:      "fallback",
		},
		{
			name:    "no reasoning",
			message: `{"content":"answer"}`,
		},
		{
			name:          "request reasoning preserves empty item",
			message:       `{"content":"answer"}`,
			request:       `{"model":"gpt-test","reasoning":{"effort":"medium"}}`,
			wantReasoning: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`{"id":"chatcmpl_reasoning","created":1,"choices":[{"index":0,"message":` + test.message + `,"finish_reason":"stop"}]}`)
			request := []byte(test.request)
			out := convertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(
				context.Background(), "gpt-test", request, request, raw, nil,
			)

			var reasoning gjson.Result
			for _, item := range gjson.GetBytes(out, "output").Array() {
				if item.Get("type").String() == "reasoning" {
					reasoning = item
					break
				}
			}
			if reasoning.Exists() != test.wantReasoning {
				t.Fatalf("reasoning exists = %v, want %v; output=%s", reasoning.Exists(), test.wantReasoning, out)
			}
			if got := reasoning.Get("summary.0.text").String(); got != test.wantText {
				t.Fatalf("reasoning text = %q, want %q; output=%s", got, test.wantText, out)
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesReasoningPrecedesSameChunkContent(t *testing.T) {
	request := []byte(`{"model":"gpt-test"}`)
	chunks := []string{
		`data: {"id":"same-chunk","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"thought","content":"answer"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}

	type eventRecord struct {
		typeName string
		itemType string
		index    int64
	}
	var state any
	var events []eventRecord
	for _, chunk := range chunks {
		for _, output := range convertOpenAIChatCompletionsResponseToOpenAIResponses(
			context.Background(), "gpt-test", request, request, []byte(chunk), &state,
		) {
			event, data := openAIResponsesSSEData(t, output)
			if event != "response.output_item.added" && event != "response.output_item.done" {
				continue
			}
			events = append(events, eventRecord{event, data.Get("item.type").String(), data.Get("output_index").Int()})
		}
	}

	want := []eventRecord{
		{"response.output_item.added", "reasoning", 0},
		{"response.output_item.done", "reasoning", 0},
		{"response.output_item.added", "message", 1},
		{"response.output_item.done", "message", 1},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events[%d] = %#v, want %#v", i, events[i], want[i])
		}
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponsesMultipleReasoningItems(t *testing.T) {
	request := []byte(`{"model":"gpt-test"}`)
	chunks := []string{
		`data: {"id":"multi-reasoning","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"first"},"finish_reason":null}]}`,
		`data: {"id":"multi-reasoning","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"answer "},"finish_reason":null}]}`,
		`data: {"id":"multi-reasoning","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"reasoning":"second"},"finish_reason":null}]}`,
		`data: {"id":"multi-reasoning","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}

	var state any
	var reasoningIndices []int64
	var reasoningIDs []string
	for _, chunk := range chunks {
		for _, output := range convertOpenAIChatCompletionsResponseToOpenAIResponses(
			context.Background(), "gpt-test", request, request, []byte(chunk), &state,
		) {
			event, data := openAIResponsesSSEData(t, output)
			if event == "response.output_item.added" && data.Get("item.type").String() == "reasoning" {
				reasoningIndices = append(reasoningIndices, data.Get("output_index").Int())
				reasoningIDs = append(reasoningIDs, data.Get("item.id").String())
			}
		}
	}

	if len(reasoningIndices) != 2 || reasoningIndices[0] != 0 || reasoningIndices[1] != 2 {
		t.Fatalf("reasoning output indexes = %v, want [0 2]", reasoningIndices)
	}
	if reasoningIDs[0] == reasoningIDs[1] {
		t.Fatalf("reasoning item IDs must be distinct: %v", reasoningIDs)
	}
}

func TestResponsesChatDeferredPatchIdentityValidation(t *testing.T) {
	patch := `{"tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, test := range []struct {
		name, request string
		ids, names    []string
		wantFailure   bool
	}{
		{name: "return to first ID", request: patch, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "apply_patch"}, wantFailure: true},
		{name: "keep second ID", request: patch, ids: []string{"c1", "c2", "c2"}, names: []string{"", "", "apply_patch"}, wantFailure: true},
		{name: "omit final ID", request: patch, ids: []string{"c1", "c2", ""}, names: []string{"", "", "apply_patch"}, wantFailure: true},
		{name: "infer at terminal", request: patch, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", ""}, wantFailure: true},
		{name: "name conflict before ID", request: patch, ids: []string{"", "", "c1"}, names: []string{"apply_patch", "other", "other"}, wantFailure: true},
		{name: "ID conflict after preview", request: patch, ids: []string{"c1", "c2", "c1"}, names: []string{"apply_patch", "apply_patch", "apply_patch"}, wantFailure: true},
		{name: "repeated identity", request: patch, ids: []string{"c1", "c1", "c1"}, names: []string{"apply_patch", "apply_patch", "apply_patch"}},
		{name: "ordinary function", request: `{"tools":[{"type":"function","name":"apply_patch"}]}`, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "apply_patch"}},
		{name: "function winner", request: `{"tools":[{"type":"function","name":"apply_patch"},{"type":"custom","name":"apply_patch"}]}`, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "apply_patch"}},
		{name: "other custom", request: `{"tools":[{"type":"custom","name":"edit"}]}`, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "edit"}},
		{name: "unrelated function beside patch", request: `{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"lookup"}]}`, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "lookup"}},
		{name: "namespace patch", request: `{"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}]}`, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "editor__apply_patch"}, wantFailure: true},
		{name: "additional patch", request: `{"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch"}]}]}`, ids: []string{"c1", "c2", "c1"}, names: []string{"", "", "apply_patch"}, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var param any
			var output [][]byte
			feed := func(chunk string) {
				output = append(output, sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "offline", []byte(test.request), nil, []byte(chunk), &param)...)
			}
			for index := range test.ids {
				arguments := ""
				if index == 1 {
					arguments = `{"input":"complete input"}`
				}
				feed(fmt.Sprintf(`data: {"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"function":{"name":%q,"arguments":%q}}]}}]}`, test.ids[index], test.names[index], arguments))
			}
			feed(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
			feed("data: [DONE]")
			failures, completions := 0, 0
			var item gjson.Result
			for _, frame := range output {
				kind, event := openAIResponsesSSEData(t, frame)
				switch kind {
				case "response.failed":
					failures++
					if event.Get("response.error.code").String() != "invalid_tool_arguments" {
						t.Fatalf("unexpected failure: %s", event.Raw)
					}
				case "response.completed":
					completions++
					item = event.Get("response.output.0")
				}
			}
			err := sdktranslator.ResponseTranslationError(&param)
			if test.wantFailure {
				if failures != 1 || completions != 0 || err == nil || err.(interface{ StatusCode() int }).StatusCode() != 502 {
					t.Fatalf("conflicting patch identity accepted: failures=%d completions=%d error=%v", failures, completions, err)
				}
			} else if failures != 0 || completions != 1 || err != nil || item.Get("call_id").String() != "c1" {
				t.Fatalf("unrelated tool changed: failures=%d completions=%d item=%s error=%v", failures, completions, item.Raw, err)
			}
			if late := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "offline", []byte(test.request), nil, []byte("data: [DONE]"), &param); len(late) != 0 {
				t.Fatalf("terminal state emitted additional events: %q", late)
			}
		})
	}
}

func TestResponsesChatReasoningOnlyRequiresFinishReason(t *testing.T) {
	for _, reason := range []string{"", "stop", "length"} {
		t.Run("finish="+reason, func(t *testing.T) {
			var param any
			chunks := []string{`data: {"id":"r","choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`}
			if reason != "" {
				chunks = append(chunks, fmt.Sprintf(`data: {"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason))
			}
			chunks = append(chunks, "data: [DONE]")
			terminal := ""
			for _, chunk := range chunks {
				for _, frame := range sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "offline", []byte(`{}`), nil, []byte(chunk), &param) {
					kind, _ := openAIResponsesSSEData(t, frame)
					if kind == "response.completed" || kind == "response.incomplete" {
						if terminal != "" {
							t.Fatalf("duplicate terminal: %s", frame)
						}
						terminal = kind
					}
				}
			}
			want := ""
			if reason == "stop" {
				want = "response.completed"
			} else if reason == "length" {
				want = "response.incomplete"
			}
			if terminal != want {
				t.Fatalf("reasoning terminal=%q, want %q", terminal, want)
			}
		})
	}
}
