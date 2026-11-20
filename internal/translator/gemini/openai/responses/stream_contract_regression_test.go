package responses_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func responseEvents(frames [][]byte) []gjson.Result {
	var events []gjson.Result
	for _, frame := range frames {
		for line := range strings.SplitSeq(string(frame), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				events = append(events, gjson.Parse(data))
			}
		}
	}
	return events
}

func TestGeminiResponsesSDKEmptyTerminalAndCandidates(t *testing.T) {
	for _, tc := range []struct{ reason, status string }{
		{"STOP", "failed"}, {"MAX_TOKENS", "incomplete"}, {"SAFETY", "incomplete"},
	} {
		var state any
		frames := sdk.Default().TranslateStream(
			context.Background(), sdk.FormatGemini, sdk.FormatOpenAIResponse, "gemini-test", nil, nil,
			[]byte(fmt.Sprintf(
				`{"candidates":[{"content":{"parts":[]},"finishReason":%q}],"usageMetadata":{"`+
					`promptTokenCount":1,"candidatesTokenCount":0,"totalTokenCount":1}}`,
				tc.reason,
			)), &state,
		)
		frames = append(
			frames, sdk.Default().TranslateStream(
				context.Background(), sdk.FormatGemini, sdk.FormatOpenAIResponse, "gemini-test", nil, nil,
				[]byte(`[DONE]`), &state,
			)...,
		)
		terminals := 0
		for _, event := range responseEvents(frames) {
			switch event.Get("type").String() {
			case "response.completed", "response.incomplete", "response.failed":
				terminals++
				if event.Get("response.status").String() != tc.status {
					t.Fatalf("%s terminal changed: %s", tc.reason, event.Raw)
				}
			}
		}
		if terminals != 1 {
			t.Fatalf("%s terminal count=%d frames=%s", tc.reason, terminals, frames)
		}
	}
	var state any
	response := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"candidate_0"}]}` +
		`,"finishReason":"STOP"},{"index":1,"content":{"parts":[{"text":"candid` +
		`ate_1"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":` +
		`1,"candidatesTokenCount":2,"totalTokenCount":3}}`)
	frames := sdk.Default().TranslateStream(
		context.Background(),
		sdk.FormatGemini, sdk.FormatOpenAIResponse, "gemini-test", nil, nil, response, &state,
	)
	var terminal gjson.Result
	for _, event := range responseEvents(frames) {
		if event.Get("type").String() == "response.completed" {
			terminal = event.Get("response")
		}
	}
	if terminal.Get("output.0.content.0.text").String() != "candidate_0" ||
		terminal.Get("output.1.content.0.text").String() != "candidate_1" {
		t.Fatalf("stream candidate content lost: %s", terminal.Raw)
	}
}

func TestChatResponsesSDKIncompleteToolBuffersRemainIncomplete(t *testing.T) {
	original := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	for _, identity := range []string{``, `"id":"call-a",`} {
		var state any
		frame := []byte(
			`{"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":` +
				`0,` +
				identity +
				`"type":"function","function":{"arguments":"{\"x\":"}}]}}]}`,
		)
		sdk.Default().TranslateStream(
			context.Background(),
			sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", original, nil, frame, &state,
		)
		frames := sdk.Default().TranslateStream(
			context.Background(), sdk.FormatOpenAI, sdk.FormatOpenAIResponse, "fixture", original, nil,
			[]byte(`[DONE]`), &state,
		)
		for _, event := range responseEvents(frames) {
			if event.Get("type").String() == "response.completed" {
				t.Fatalf("pending tool became success: %s", event.Raw)
			}
		}
	}
}

func TestGeminiResponsesSDKWaitsForEveryKnownCandidate(t *testing.T) {
	var state any
	frames := []string{
		`{"candidates":[{"index":0,"content":{"parts":[{"text":"candidate_0"}]}` +
			`},{"index":1,"content":{"parts":[{"text":"candidate_1"}]}}]}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"` +
			`candidatesTokenCount":2,"totalTokenCount":3}}`,
		`{"candidates":[{"index":1,"content":{"parts":[{"text":"candidate_1_tail"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":3,"totalTokenCount":4}}`,
	}
	terminals := 0
	for index, frame := range frames {
		out := sdk.Default().TranslateStream(
			context.Background(),
			sdk.FormatGemini, sdk.FormatOpenAIResponse, "gemini-test", nil, nil, []byte(frame),
			&state,
		)
		for _, event := range responseEvents(out) {
			if event.Get("type").String() == "response.completed" {
				terminals++
				if index != 2 || !strings.Contains(event.Get("response.output").Raw, "candidate_1_tail") {
					t.Fatalf("candidate completed early or lost tail: %s", event.Raw)
				}
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal count = %d", terminals)
	}
}
