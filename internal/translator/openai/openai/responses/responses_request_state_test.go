package responses

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesRequestSelectionAndStreamIsolation(t *testing.T) {
	request := func(namespace string) []byte {
		return []byte(fmt.Sprintf(`{"tools":[{"type":"namespace","name":%q,"tools":[{"type":"function","name":"run"}]}]}`, namespace))
	}
	cases := []struct {
		name                 string
		original, translated []byte
		wantNamespace        string
		state                any
	}{
		{name: "original", original: request("original"), translated: request("translated"), wantNamespace: "original"},
		{name: "fallback", original: []byte(`{"broken":`), translated: request("fallback"), wantNamespace: "fallback"},
		{name: "translated", translated: request("separate"), wantNamespace: "separate"},
		{name: "invalid", original: []byte(`invalid`), translated: []byte(`invalid`)},
	}
	start := []byte(`data: {"id":"r","created":1,"choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
	tool := []byte(`data: {"id":"r","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"run","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	for i := range cases {
		test := &cases[i]
		convertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", test.original, test.translated, start, &test.state)
	}
	for i := range cases {
		test := &cases[i]
		t.Run(test.name, func(t *testing.T) {
			events := convertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", test.original, test.translated, tool, &test.state)
			events = append(events, convertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", test.original, test.translated, []byte("data: [DONE]"), &test.state)...)
			all := bytes.Join(events, nil)
			if !bytes.Contains(all, []byte(`"name":"run"`)) {
				t.Fatalf("tool identity missing: %s", all)
			}
			if test.wantNamespace == "" {
				if bytes.Contains(all, []byte(`"namespace":`)) {
					t.Fatalf("invalid request acquired another stream's namespace: %s", all)
				}
			} else if !bytes.Contains(all, []byte(fmt.Sprintf(`"namespace":%q`, test.wantNamespace))) {
				t.Fatalf("namespace %q missing: %s", test.wantNamespace, all)
			}
		})
	}
}

func TestResponsesLocalAliasPreservesCustomInput(t *testing.T) {
	request := []byte(`{"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"run"}]}]}`)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var state any
			var output gjson.Result
			if stream {
				events := convertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", request, nil,
					[]byte(`data: {"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"run","arguments":"{\"input\":\"echo <&>\"}"}}]},"finish_reason":"tool_calls"}]}`), &state)
				events = append(events, convertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", request, nil,
					[]byte("data: [DONE]"), &state)...)
				for _, event := range events {
					_, payload, found := bytes.Cut(event, []byte("\ndata: "))
					if !found {
						continue
					}
					root := gjson.ParseBytes(bytes.TrimSpace(payload))
					if root.Get("type").String() == "response.completed" {
						output = root.Get("response.output.0")
					}
				}
			} else {
				body := convertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "test", request, nil,
					[]byte(`{"id":"r","choices":[{"index":0,"message":{"tool_calls":[{"id":"c","function":{"name":"run","arguments":"{\"input\":\"echo <&>\"}"}}]},"finish_reason":"tool_calls"}]}`), &state)
				output = gjson.GetBytes(body, "output.0")
			}
			if output.Get("type").String() != "custom_tool_call" || output.Get("name").String() != "run" ||
				output.Get("namespace").String() != "editor" || output.Get("input").String() != "echo <&>" {
				t.Fatalf("custom input or identity lost: %s", output.Raw)
			}
		})
	}
}
