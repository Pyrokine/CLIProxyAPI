package responses

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResponsesLocalShellRequestAndHistory(t *testing.T) {
	request := []byte(`{"tools":[{"type":"shell","environment":{"type":"local"}},{"type":"function","name":"__cpa_local_shell"},{"type":"custom","name":"__cpa_local_shell_1"}],"tool_choice":{"type":"shell"},"input":[{"type":"shell_call","call_id":"sh1","action":{"commands":["printf phase2"]}},{"type":"shell_call_output","call_id":"sh1","max_output_length":4096,"output":[{"stdout":"phase2","stderr":"","outcome":{"type":"exit","exit_code":0}}]}]}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", request, false)
	if gjson.GetBytes(out, "tools.#").Int() != 3 {
		t.Fatalf("local shell declaration lost: %s", out)
	}
	name := gjson.GetBytes(out, "tool_choice.function.name").String()
	if name == "" || name == "__cpa_local_shell" || name == "__cpa_local_shell_1" || len(name) > 64 {
		t.Fatalf("shell alias collides: %s", out)
	}
	found := false
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		if tool.Get("function.name").String() == name {
			found = tool.Get("function.parameters.properties.commands.items.type").String() == "string"
		}
	}
	if !found || gjson.GetBytes(out, "messages.0.tool_calls.0.function.name").String() != name || gjson.GetBytes(out, "messages.1.tool_call_id").String() != "sh1" {
		t.Fatalf("shell declaration and history disagree: %s", out)
	}
	if got := gjson.Parse(gjson.GetBytes(out, "messages.0.tool_calls.0.function.arguments").String()).Get("commands.0").String(); got != "printf phase2" {
		t.Fatalf("shell action lost: %s", out)
	}
	originalOutput := gjson.GetBytes(request, "input.1")
	if !reflect.DeepEqual(gjson.Parse(gjson.GetBytes(out, "messages.1.content").String()).Value(), originalOutput.Value()) {
		t.Fatalf("shell output envelope lost: %s", out)
	}
	withdrawn := []byte(`{"input":[{"type":"shell_call","call_id":"sh1","action":{"commands":["printf phase2"]}},{"type":"shell_call_output","call_id":"sh1","output":[{"stdout":"phase2"}]},{"type":"function_call","name":"__cpa_local_shell","call_id":"user1","arguments":"{}"},{"type":"function_call_output","call_id":"user1","output":"result"}]}`)
	replayed := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", withdrawn, false)
	if gjson.GetBytes(replayed, "tools.#").Int() != 0 || gjson.GetBytes(replayed, "messages.0.tool_calls.0.id").String() != "sh1" || gjson.GetBytes(replayed, "messages.0.tool_calls.0.function.name").String() == "__cpa_local_shell" || gjson.GetBytes(replayed, "messages.2.tool_calls.0.id").String() != "user1" {
		t.Fatalf("withdrawn tool re-enabled or history lost: %s", replayed)
	}
	for _, environment := range []string{`{}`, `{"type":"container_auto"}`, `{"type":"container_reference","container_id":"fixture"}`} {
		out = ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", []byte(`{"tools":[{"type":"shell","environment":`+environment+`}]}`), false)
		if gjson.GetBytes(out, "tools.#").Int() != 0 {
			t.Fatalf("non-local shell was exposed as local: %s", out)
		}
	}
}

func TestResponsesLocalShellDeclarationBoundaries(t *testing.T) {
	for _, test := range []struct {
		request string
		count   int64
	}{
		{`{"input":[{"type":"additional_tools","tools":[{"type":"shell","environment":{"type":"local"}}]}]}`, 1},
		{`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"shell","environment":{"type":"local"}}]}]}`, 0},
		{`{"tools":[{"type":"shell","environment":{"type":"local"}},{"type":"shell","environment":{"type":"local"}}]}`, 1},
	} {
		out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", []byte(test.request), false)
		if gjson.GetBytes(out, "tools.#").Int() != test.count {
			t.Fatalf("shell declaration boundary violated: %s", out)
		}
	}
	for _, choice := range []string{"auto", "none", "required"} {
		request := []byte(fmt.Sprintf(`{"tools":[{"type":"shell","environment":{"type":"local"}}],"tool_choice":%q}`, choice))
		out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", request, false)
		if gjson.GetBytes(out, "tool_choice").String() != choice {
			t.Fatalf("string tool choice changed: %s", out)
		}
	}
	request := []byte(`{"tools":[{"type":"shell","environment":{"type":"local"}},{"type":"function","name":"__cpa_local_shell"},{"type":"custom","name":"__cpa_local_shell_1"}]}`)
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", request, false)
	for _, test := range []struct{ name, kind string }{{"__cpa_local_shell", "function_call"}, {"__cpa_local_shell_1", "custom_tool_call"}} {
		upstream := []byte(fmt.Sprintf(`{"choices":[{"message":{"tool_calls":[{"id":"user1","function":{"name":%q,"arguments":"{}"}}]}}]}`, test.name))
		var param any
		out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", request, translated, upstream, &param)
		if gjson.GetBytes(out, "output.0.type").String() != test.kind || gjson.GetBytes(out, "output.0.name").String() != test.name {
			t.Fatalf("user tool misclassified as shell: %s", out)
		}
	}
}

func TestResponsesLocalShellRegisteredRoundTrip(t *testing.T) {
	request := []byte(`{"tools":[{"type":"shell","environment":{"type":"local"}}]}`)
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", request, true)
	name := gjson.GetBytes(translated, "tools.0.function.name").String()
	arguments := `{"commands":["printf phase2"],"timeout_ms":1000,"max_output_length":4096}`
	upstream := []byte(fmt.Sprintf(`{"id":"fixture","choices":[{"index":0,"message":{"tool_calls":[{"id":"sh1","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, name, arguments))
	var param any
	out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", request, translated, upstream, &param)
	item := gjson.GetBytes(out, "output.0")
	if item.Get("type").String() != "shell_call" || item.Get("status").String() != "completed" || item.Get("action.commands.0").String() != "printf phase2" || item.Get("name").Exists() || item.Get("arguments").Exists() {
		t.Fatalf("native shell response missing: %s", out)
	}
	var done, completed gjson.Result
	for _, event := range responsesShellTestEvents(request, translated, name, arguments) {
		switch event.Get("type").String() {
		case "response.output_item.added":
			if event.Get("item.type").String() != "shell_call" {
				t.Fatalf("shell item represented as function: %s", event.Raw)
			}
		case "response.output_item.done":
			done = event.Get("item")
		case "response.completed":
			completed = event.Get("response.output.0")
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			t.Fatalf("shell leaked function events: %s", event.Raw)
		}
	}
	if !reflect.DeepEqual(done.Value(), item.Value()) || !reflect.DeepEqual(completed.Value(), item.Value()) {
		t.Fatalf("shell final items disagree: nonstream=%s done=%s completed=%s", item.Raw, done.Raw, completed.Raw)
	}
	param = nil
	unknown := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", []byte(`{}`), []byte(`{}`), upstream, &param)
	if gjson.GetBytes(unknown, "output.0.type").String() != "function_call" {
		t.Fatalf("shell name inferred without declaration: %s", unknown)
	}
}

func TestResponsesLocalShellRejectsInvalidActions(t *testing.T) {
	request := []byte(`{"tools":[{"type":"shell","environment":{"type":"local"}}]}`)
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", request, true)
	name := gjson.GetBytes(translated, "tools.0.function.name").String()
	for _, arguments := range []string{`{`, `{}`, `{"commands":[]}`, `{"commands":[42]}`, `{"commands":[" "]}`, `{"commands":["printf phase2"],"timeout_ms":-1}`, `{"commands":["printf phase2"],"timeout_ms":1.5}`, `{"commands":["printf phase2"],"timeout_ms":1e999}`, `{"commands":["printf phase2"],"env":{}}`} {
		t.Run(arguments, func(t *testing.T) {
			upstream := []byte(fmt.Sprintf(`{"id":"fixture","choices":[{"message":{"tool_calls":[{"id":"sh1","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, name, arguments))
			var param any
			out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", request, translated, upstream, &param)
			if gjson.GetBytes(out, "status").String() != "failed" || gjson.GetBytes(out, "error.code").String() != "invalid_tool_arguments" || !strings.Contains(gjson.GetBytes(out, "error.message").String(), "shell") {
				t.Fatalf("invalid shell action accepted: %s", out)
			}
			failed := false
			for _, event := range responsesShellTestEvents(request, translated, name, arguments) {
				if failed {
					t.Fatalf("event emitted after shell failure: %s", event.Raw)
				}
				switch event.Get("type").String() {
				case "response.failed":
					failed = true
				case "response.completed", "response.output_item.done":
					t.Fatalf("invalid shell action completed: %s", event.Raw)
				}
			}
			if !failed {
				t.Fatal("shell stream failure missing")
			}
		})
	}
}

func responsesShellTestEvents(request, translated []byte, name, arguments string) []gjson.Result {
	chunks := []string{
		fmt.Sprintf(`data: {"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"sh1","function":{"name":%q,"arguments":%q}}]}}]}`, name, arguments[:len(arguments)/2]),
		fmt.Sprintf(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, arguments[len(arguments)/2:]),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		`data: {"choices":[{"index":0,"delta":{"content":"late content"}}]}`,
	}
	var param any
	var events []gjson.Result
	for _, chunk := range chunks {
		for _, frame := range sdktranslator.Default().TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", request, translated, []byte(chunk), &param) {
			for _, line := range bytes.Split(frame, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data:")) {
					events = append(events, gjson.ParseBytes(bytes.TrimSpace(line[5:])))
				}
			}
		}
	}
	return events
}
