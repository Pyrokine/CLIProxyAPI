package conformance_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	codexclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/claude"
	codexchat "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/chat-completions"
	codexresponses "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/responses"
	"github.com/tidwall/gjson"
)

type toolFixture struct {
	ID        string
	Name      string
	Arguments string
	Result    string
}

type protocolFixture struct {
	name            string
	request         func([]byte) []byte
	stream          func([]byte, []byte, *any) [][]byte
	nonStream       func([]byte, []byte) []byte
	requestJSON     func([]toolFixture) []byte
	normalizeStream func(*testing.T, [][]byte) normalizedResponse
	normalizeFinal  func(*testing.T, []byte) normalizedResponse
	stopReason      string
}

type normalizedCall struct {
	ID        string
	Name      string
	Arguments string
}

type normalizedResponse struct {
	Calls             []normalizedCall
	Text              string
	StopReason        string
	TerminalCount     int
	StopMetadataCount int
}

func TestToolOnlyRequestConformance(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		for _, test := range toolFixtureCases() {
			t.Run(
				protocol.name+"/"+test.name, func(t *testing.T) {
					request := protocol.requestJSON(test.calls)
					assertCanonicalToolRequest(t, protocol.request(request), test.calls)
				},
			)
		}
	}
}

func TestToolOnlyResponseStreamMatchesNonStream(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		for _, test := range toolFixtureCases() {
			t.Run(
				protocol.name+"/"+test.name, func(t *testing.T) {
					originalRequest := protocol.requestJSON(test.calls)
					var state any
					var translated [][]byte
					for _, event := range codexToolStream(test.calls, "response.completed") {
						translated = append(translated, protocol.stream(originalRequest, event, &state)...)
					}

					stream := protocol.normalizeStream(t, translated)
					final := protocol.normalizeFinal(
						t, protocol.nonStream(originalRequest, codexToolTerminal(test.calls, "response.completed")),
					)
					assertToolOnlyResponse(t, stream, test.calls, protocol.stopReason)
					assertToolOnlyResponse(t, final, test.calls, protocol.stopReason)
					if fmt.Sprint(stream.Calls) != fmt.Sprint(final.Calls) {
						t.Fatalf("stream calls = %#v, non-stream calls = %#v", stream.Calls, final.Calls)
					}
				},
			)
		}
	}
}

func TestToolOnlyStreamTerminalAliases(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		for _, terminalType := range []string{"response.completed", "response.done", "response.incomplete"} {
			t.Run(
				protocol.name+"/"+terminalType, func(t *testing.T) {
					calls := toolFixtureCases()[0].calls
					originalRequest := protocol.requestJSON(calls)
					var state any
					var translated [][]byte
					for _, event := range codexToolStream(calls, terminalType) {
						translated = append(translated, protocol.stream(originalRequest, event, &state)...)
					}
					assertSingleStreamTerminal(t, protocol.normalizeStream(t, translated), translated)
				},
			)
		}
	}
}

func TestToolOnlyStreamSuppressesDuplicateTerminal(t *testing.T) {
	for _, protocol := range protocolFixtures() {
		t.Run(
			protocol.name, func(t *testing.T) {
				calls := toolFixtureCases()[0].calls
				originalRequest := protocol.requestJSON(calls)
				var state any
				var translated [][]byte
				for _, event := range codexToolStream(calls, "response.completed") {
					translated = append(translated, protocol.stream(originalRequest, event, &state)...)
				}
				translated = append(
					translated,
					protocol.stream(
						originalRequest, codexSSE(string(codexToolTerminal(calls, "response.done"))), &state,
					)...,
				)
				assertSingleStreamTerminal(t, protocol.normalizeStream(t, translated), translated)
			},
		)
	}
}

func assertSingleStreamTerminal(t *testing.T, response normalizedResponse, output [][]byte) {
	t.Helper()
	if response.TerminalCount != 1 {
		t.Fatalf("terminal count = %d, want 1; output=%q", response.TerminalCount, output)
	}
	if response.StopMetadataCount != 1 {
		t.Fatalf("stop metadata count = %d, want 1; output=%q", response.StopMetadataCount, output)
	}
}

func protocolFixtures() []protocolFixture {
	return []protocolFixture{
		{
			name: "openai-chat",
			request: func(raw []byte) []byte {
				return codexchat.ConvertOpenAIRequestToCodex("gpt-5.6-sol", raw, true)
			},
			stream: func(original, event []byte, state *any) [][]byte {
				return codexchat.ConvertCodexResponseToOpenAI(
					context.Background(), "gpt-5.6-sol", original, nil, event, state,
				)
			},
			nonStream: func(original, terminal []byte) []byte {
				return codexchat.ConvertCodexResponseToOpenAINonStream(
					context.Background(), "gpt-5.6-sol", original, nil, terminal, nil,
				)
			},
			requestJSON:     openAIChatRequest,
			normalizeStream: normalizeOpenAIChatStream,
			normalizeFinal:  normalizeOpenAIChatFinal,
			stopReason:      "tool_calls",
		},
		{
			name: "openai-responses",
			request: func(raw []byte) []byte {
				return codexresponses.ConvertOpenAIResponsesRequestToCodex("gpt-5.6-sol", raw, true)
			},
			stream: func(original, event []byte, state *any) [][]byte {
				return codexresponses.ConvertCodexResponseToOpenAIResponses(
					context.Background(), "gpt-5.6-sol", original, nil, event, state,
				)
			},
			nonStream: func(original, terminal []byte) []byte {
				return codexresponses.ConvertCodexResponseToOpenAIResponsesNonStream(
					context.Background(), "gpt-5.6-sol", original, nil, terminal, nil,
				)
			},
			requestJSON:     openAIResponsesRequest,
			normalizeStream: normalizeOpenAIResponsesStream,
			normalizeFinal:  normalizeOpenAIResponsesFinal,
			stopReason:      "completed",
		},
		{
			name: "anthropic-messages",
			request: func(raw []byte) []byte {
				return codexclaude.ConvertClaudeRequestToCodex("gpt-5.6-sol", raw, true)
			},
			stream: func(original, event []byte, state *any) [][]byte {
				return codexclaude.ConvertCodexResponseToClaude(
					context.Background(), "gpt-5.6-sol", original, nil, event, state,
				)
			},
			nonStream: func(original, terminal []byte) []byte {
				return codexclaude.ConvertCodexResponseToClaudeNonStream(
					context.Background(), "gpt-5.6-sol", original, nil, terminal, nil,
				)
			},
			requestJSON:     anthropicMessagesRequest,
			normalizeStream: normalizeAnthropicStream,
			normalizeFinal:  normalizeAnthropicFinal,
			stopReason:      "tool_use",
		},
	}
}

func toolFixtureCases() []struct {
	name  string
	calls []toolFixture
} {
	calls := []toolFixture{
		{ID: "call_weather", Name: "weather", Arguments: `{"city":"北京"}`, Result: "sunny"},
		{ID: "call_time", Name: "time", Arguments: `{"zone":"UTC"}`, Result: "12:00"},
	}
	return []struct {
		name  string
		calls []toolFixture
	}{
		{name: "single", calls: calls[:1]},
		{name: "parallel", calls: calls},
	}
}

func openAIChatRequest(calls []toolFixture) []byte {
	var toolCalls strings.Builder
	var results strings.Builder
	var tools strings.Builder
	for index, call := range calls {
		if index > 0 {
			toolCalls.WriteByte(',')
			results.WriteByte(',')
			tools.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&toolCalls,
			`{"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Name),
			strconv.Quote(call.Arguments),
		)
		_, _ = fmt.Fprintf(
			&results,
			`{"role":"tool","tool_call_id":%s,"content":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Result),
		)
		_, _ = fmt.Fprintf(
			&tools,
			`{"type":"function","function":{"name":%s,"parameters":{"type":"object"}}}`,
			strconv.Quote(call.Name),
		)
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"Use tools"},{"role":"assistant","content":null,"tool_calls":[%s]},%s],"tools":[%s]}`,
		toolCalls.String(), results.String(), tools.String(),
	))
}

func openAIResponsesRequest(calls []toolFixture) []byte {
	var input strings.Builder
	var tools strings.Builder
	input.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Use tools"}]}`)
	for index, call := range calls {
		_, _ = fmt.Fprintf(
			&input,
			`,{"type":"function_call","call_id":%s,"name":%s,"arguments":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Name),
			strconv.Quote(call.Arguments),
		)
		if index > 0 {
			tools.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&tools,
			`{"type":"function","name":%s,"parameters":{"type":"object"}}`,
			strconv.Quote(call.Name),
		)
	}
	for _, call := range calls {
		_, _ = fmt.Fprintf(
			&input,
			`,{"type":"function_call_output","call_id":%s,"output":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Result),
		)
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5.6-sol","stream":true,"input":[%s],"tools":[%s]}`,
		input.String(), tools.String(),
	))
}

func anthropicMessagesRequest(calls []toolFixture) []byte {
	var toolUse strings.Builder
	var toolResults strings.Builder
	var tools strings.Builder
	for index, call := range calls {
		if index > 0 {
			toolUse.WriteByte(',')
			toolResults.WriteByte(',')
			tools.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&toolUse,
			`{"type":"tool_use","id":%s,"name":%s,"input":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Name),
			call.Arguments,
		)
		_, _ = fmt.Fprintf(
			&toolResults,
			`{"type":"tool_result","tool_use_id":%s,"content":%s}`,
			strconv.Quote(call.ID),
			strconv.Quote(call.Result),
		)
		_, _ = fmt.Fprintf(
			&tools,
			`{"name":%s,"input_schema":{"type":"object"}}`,
			strconv.Quote(call.Name),
		)
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"Use tools"}]},{"role":"assistant","content":[%s]},{"role":"user","content":[%s]}],"tools":[%s]}`,
		toolUse.String(), toolResults.String(), tools.String(),
	))
}

func assertCanonicalToolRequest(t *testing.T, raw []byte, expected []toolFixture) {
	t.Helper()
	items := gjson.GetBytes(raw, "input").Array()
	calls := make([]normalizedCall, 0, len(expected))
	outputs := make(
		[]struct {
			ID     string
			Result string
		}, 0, len(expected),
	)
	seenOutput := false
	for _, item := range items {
		switch item.Get("type").String() {
		case "message":
			if item.Get("role").String() == "assistant" && len(item.Get("content").Array()) == 0 {
				t.Fatalf("empty assistant message was emitted: %s", item.Raw)
			}
		case "function_call":
			if seenOutput {
				t.Fatalf("function call was emitted after a tool result: %s", raw)
			}
			calls = append(
				calls, normalizedCall{
					ID:        item.Get("call_id").String(),
					Name:      item.Get("name").String(),
					Arguments: item.Get("arguments").String(),
				},
			)
		case "function_call_output":
			seenOutput = true
			outputs = append(
				outputs, struct {
					ID     string
					Result string
				}{ID: item.Get("call_id").String(), Result: item.Get("output").String()},
			)
		}
	}
	if len(calls) != len(expected) || len(outputs) != len(expected) {
		t.Fatalf("calls=%#v outputs=%#v, want %d pairs; request=%s", calls, outputs, len(expected), raw)
	}
	for index, want := range expected {
		if calls[index] != (normalizedCall{ID: want.ID, Name: want.Name, Arguments: want.Arguments}) {
			t.Fatalf("call %d = %#v, want %#v", index, calls[index], want)
		}
		if outputs[index].ID != want.ID || outputs[index].Result != want.Result {
			t.Fatalf("output %d = %#v, want id=%q result=%q", index, outputs[index], want.ID, want.Result)
		}
	}
}

func codexToolStream(calls []toolFixture, terminalType string) [][]byte {
	events := [][]byte{
		codexSSE(`{"type":"response.created","response":{"id":"resp_tools","model":"gpt-5.6-sol","created_at":1700000000}}`),
	}
	for index, call := range calls {
		events = append(
			events, codexSSE(
				fmt.Sprintf(
					`{"type":"response.output_item.added","output_index":%d,"item":{"id":"item_%d","type":"function_call","call_id":%s,"name":%s,"arguments":""}}`,
					index, index, strconv.Quote(call.ID), strconv.Quote(call.Name),
				),
			),
		)
	}
	for fragmentIndex := range 2 {
		for index, call := range calls {
			fragments := splitArguments(call.Arguments)
			events = append(
				events, codexSSE(
					fmt.Sprintf(
						`{"type":"response.function_call_arguments.delta","item_id":"item_%d","output_index":%d,"delta":%s}`,
						index, index, strconv.Quote(fragments[fragmentIndex]),
					),
				),
			)
		}
	}
	for index, call := range calls {
		events = append(
			events,
			codexSSE(
				fmt.Sprintf(
					`{"type":"response.function_call_arguments.done","item_id":"item_%d","output_index":%d,"arguments":%s}`,
					index, index, strconv.Quote(call.Arguments),
				),
			),
			codexSSE(
				fmt.Sprintf(
					`{"type":"response.output_item.done","output_index":%d,"item":{"id":"item_%d","type":"function_call","call_id":%s,"name":%s,"arguments":%s}}`,
					index, index, strconv.Quote(call.ID), strconv.Quote(call.Name), strconv.Quote(call.Arguments),
				),
			),
		)
	}
	return append(events, codexSSE(string(codexToolTerminal(calls, terminalType))))
}

func codexToolTerminal(calls []toolFixture, terminalType string) []byte {
	status := "completed"
	if terminalType == "response.incomplete" {
		status = "incomplete"
	}
	var output strings.Builder
	for index, call := range calls {
		if index > 0 {
			output.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&output,
			`{"id":"item_%d","type":"function_call","call_id":%s,"name":%s,"arguments":%s}`,
			index, strconv.Quote(call.ID), strconv.Quote(call.Name), strconv.Quote(call.Arguments),
		)
	}
	incomplete := ""
	if terminalType == "response.incomplete" {
		incomplete = `,"incomplete_details":{"reason":"max_output_tokens"}`
	}
	return []byte(fmt.Sprintf(
		`{"type":%s,"response":{"id":"resp_tools","model":"gpt-5.6-sol","status":%s,"output":[%s],"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}%s}}`,
		strconv.Quote(terminalType), strconv.Quote(status), output.String(), incomplete,
	))
}

func codexSSE(payload string) []byte {
	return []byte("data: " + payload)
}

func splitArguments(arguments string) [2]string {
	middle := len(arguments) / 2
	for middle < len(arguments) && arguments[middle]&0xc0 == 0x80 {
		middle++
	}
	return [2]string{arguments[:middle], arguments[middle:]}
}

func normalizeOpenAIChatStream(t *testing.T, chunks [][]byte) normalizedResponse {
	t.Helper()
	response := normalizedResponse{}
	calls := make(map[int]*normalizedCall)
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if !gjson.ValidBytes(chunk) {
			t.Fatalf("invalid Chat chunk: %q", chunk)
		}
		root := gjson.ParseBytes(chunk)
		if content := root.Get("choices.0.delta.content"); content.Exists() && content.String() != "" {
			response.Text += content.String()
		}
		for _, item := range root.Get("choices.0.delta.tool_calls").Array() {
			index := int(item.Get("index").Int())
			call := calls[index]
			if call == nil {
				call = &normalizedCall{}
				calls[index] = call
			}
			if value := item.Get("id").String(); value != "" {
				call.ID = value
			}
			if value := item.Get("function.name").String(); value != "" {
				call.Name = value
			}
			call.Arguments += item.Get("function.arguments").String()
		}
		if finishReason := root.Get("choices.0.finish_reason").String(); finishReason != "" {
			response.TerminalCount++
			response.StopMetadataCount++
			response.StopReason = finishReason
		}
	}
	for index := 0; index < len(calls); index++ {
		if calls[index] == nil {
			t.Fatalf("missing Chat tool call index %d", index)
		}
		response.Calls = append(response.Calls, *calls[index])
	}
	return response
}

func normalizeOpenAIChatFinal(t *testing.T, raw []byte) normalizedResponse {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid Chat response: %q", raw)
	}
	response := normalizedResponse{
		Text:              gjson.GetBytes(raw, "choices.0.message.content").String(),
		StopReason:        gjson.GetBytes(raw, "choices.0.finish_reason").String(),
		TerminalCount:     1,
		StopMetadataCount: 1,
	}
	for _, item := range gjson.GetBytes(raw, "choices.0.message.tool_calls").Array() {
		response.Calls = append(
			response.Calls, normalizedCall{
				ID:        item.Get("id").String(),
				Name:      item.Get("function.name").String(),
				Arguments: item.Get("function.arguments").String(),
			},
		)
	}
	return response
}

func normalizeOpenAIResponsesStream(t *testing.T, chunks [][]byte) normalizedResponse {
	t.Helper()
	response := normalizedResponse{}
	calls := make(map[int]*normalizedCall)
	for _, chunk := range chunks {
		for _, payload := range sseDataPayloads(t, chunk) {
			root := gjson.ParseBytes(payload)
			switch root.Get("type").String() {
			case "response.output_item.added":
				item := root.Get("item")
				if item.Get("type").String() != "function_call" {
					continue
				}
				index := int(root.Get("output_index").Int())
				calls[index] = &normalizedCall{
					ID:   item.Get("call_id").String(),
					Name: item.Get("name").String(),
				}
			case "response.function_call_arguments.delta":
				index := int(root.Get("output_index").Int())
				if calls[index] == nil {
					t.Fatalf("Responses arguments delta targets missing index %d", index)
				}
				calls[index].Arguments += root.Get("delta").String()
			case "response.output_text.delta":
				response.Text += root.Get("delta").String()
			case "response.completed", "response.done":
				response.TerminalCount++
				response.StopMetadataCount++
				response.StopReason = "completed"
			case "response.incomplete":
				response.TerminalCount++
				response.StopMetadataCount++
				response.StopReason = "incomplete"
			}
		}
	}
	for index := 0; index < len(calls); index++ {
		if calls[index] == nil {
			t.Fatalf("missing Responses tool call index %d", index)
		}
		response.Calls = append(response.Calls, *calls[index])
	}
	return response
}

func normalizeOpenAIResponsesFinal(t *testing.T, raw []byte) normalizedResponse {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid Responses response: %q", raw)
	}
	response := normalizedResponse{
		StopReason:        "completed",
		TerminalCount:     1,
		StopMetadataCount: 1,
	}
	for _, item := range gjson.GetBytes(raw, "output").Array() {
		switch item.Get("type").String() {
		case "function_call":
			response.Calls = append(
				response.Calls, normalizedCall{
					ID:        item.Get("call_id").String(),
					Name:      item.Get("name").String(),
					Arguments: item.Get("arguments").String(),
				},
			)
		case "message":
			response.Text += item.Get("content.0.text").String()
		}
	}
	return response
}

func normalizeAnthropicStream(t *testing.T, chunks [][]byte) normalizedResponse {
	t.Helper()
	response := normalizedResponse{}
	calls := make(map[int]*normalizedCall)
	for _, chunk := range chunks {
		for _, payload := range sseDataPayloads(t, chunk) {
			root := gjson.ParseBytes(payload)
			switch root.Get("type").String() {
			case "content_block_start":
				index := int(root.Get("index").Int())
				switch root.Get("content_block.type").String() {
				case "tool_use":
					calls[index] = &normalizedCall{
						ID:   root.Get("content_block.id").String(),
						Name: root.Get("content_block.name").String(),
					}
				case "text":
					response.Text += root.Get("content_block.text").String()
				}
			case "content_block_delta":
				index := int(root.Get("index").Int())
				switch root.Get("delta.type").String() {
				case "input_json_delta":
					if calls[index] == nil {
						t.Fatalf("Anthropic arguments delta targets missing index %d", index)
					}
					calls[index].Arguments += root.Get("delta.partial_json").String()
				case "text_delta":
					response.Text += root.Get("delta.text").String()
				}
			case "message_delta":
				response.StopMetadataCount++
				response.StopReason = root.Get("delta.stop_reason").String()
			case "message_stop":
				response.TerminalCount++
			}
		}
	}
	for index := 0; index < len(calls); index++ {
		if calls[index] == nil {
			t.Fatalf("missing Anthropic tool call index %d", index)
		}
		response.Calls = append(response.Calls, *calls[index])
	}
	return response
}

func normalizeAnthropicFinal(t *testing.T, raw []byte) normalizedResponse {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid Anthropic response: %q", raw)
	}
	response := normalizedResponse{
		StopReason:        gjson.GetBytes(raw, "stop_reason").String(),
		TerminalCount:     1,
		StopMetadataCount: 1,
	}
	for _, block := range gjson.GetBytes(raw, "content").Array() {
		switch block.Get("type").String() {
		case "tool_use":
			response.Calls = append(
				response.Calls, normalizedCall{
					ID:        block.Get("id").String(),
					Name:      block.Get("name").String(),
					Arguments: block.Get("input").Raw,
				},
			)
		case "text":
			response.Text += block.Get("text").String()
		}
	}
	return response
}

func sseDataPayloads(t *testing.T, chunk []byte) [][]byte {
	t.Helper()
	var payloads [][]byte
	for block := range strings.SplitSeq(strings.ReplaceAll(string(chunk), "\r\n", "\n"), "\n\n") {
		for line := range strings.SplitSeq(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if len(payload) == 0 || string(payload) == "[DONE]" {
				continue
			}
			if !gjson.ValidBytes(payload) {
				t.Fatalf("invalid SSE data payload: %q", payload)
			}
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

func assertToolOnlyResponse(
	t *testing.T,
	response normalizedResponse,
	expected []toolFixture,
	stopReason string,
) {
	t.Helper()
	if response.Text != "" {
		t.Fatalf("tool-only response contains text %q", response.Text)
	}
	if response.StopReason != stopReason {
		t.Fatalf("stop reason = %q, want %q", response.StopReason, stopReason)
	}
	if response.TerminalCount != 1 || response.StopMetadataCount != 1 {
		t.Fatalf("terminal count=%d stop metadata=%d, want 1/1", response.TerminalCount, response.StopMetadataCount)
	}
	if len(response.Calls) != len(expected) {
		t.Fatalf("calls = %#v, want %d", response.Calls, len(expected))
	}
	for index, want := range expected {
		expectedCall := normalizedCall{ID: want.ID, Name: want.Name, Arguments: want.Arguments}
		if response.Calls[index] != expectedCall {
			t.Fatalf("call %d = %#v, want %#v", index, response.Calls[index], expectedCall)
		}
	}
}
