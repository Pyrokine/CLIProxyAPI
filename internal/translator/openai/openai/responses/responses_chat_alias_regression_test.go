package responses

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResponsesChatNamespaceAliasesRoundTrip(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			namespaces := []string{"ns" + strings.Repeat("x", 70) + "a", "ns" + strings.Repeat("x", 70) + "b"}
			callType, callInput, arguments := "function_call", `"arguments":"{}"`, "{}"
			if kind == "custom" {
				callType, callInput, arguments = "custom_tool_call", `"input":"payload"`, `{"input":"payload"}`
			}
			request := []byte(fmt.Sprintf(`{"tools":[{"type":"namespace","name":%q,"tools":[{"type":%q,"name":"lookup","parameters":{"type":"object"}}]},{"type":"namespace","name":%q,"tools":[{"type":%q,"name":"lookup","parameters":{"type":"object"}}]}],"input":[{"type":%q,"namespace":%q,"name":"lookup","call_id":"call_a",%s},{"type":"function_call_output","call_id":"call_a","output":"result"}],"tool_choice":{"type":%q,"namespace":%q,"name":"lookup"}}`, namespaces[0], kind, namespaces[1], kind, callType, namespaces[0], callInput, kind, namespaces[0]))
			translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("fixture-model", request, true)
			tools := gjson.GetBytes(translated, "tools").Array()
			if len(tools) != 2 {
				t.Fatalf("tools = %d, want 2; %s", len(tools), translated)
			}
			aliases := []string{tools[0].Get("function.name").String(), tools[1].Get("function.name").String()}
			if aliases[0] == aliases[1] || len(aliases[0]) > 64 || len(aliases[1]) > 64 {
				t.Fatalf("aliases collide or exceed 64: %v", aliases)
			}
			for _, path := range []string{"messages.0.tool_calls.0.function.name", "tool_choice.function.name"} {
				if got := gjson.GetBytes(translated, path).String(); got != aliases[0] {
					t.Fatalf("%s alias = %q, want %q; %s", path, got, aliases[0], translated)
				}
			}
			response := []byte(fmt.Sprintf(`{"choices":[{"message":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, aliases[1], arguments))
			var param any
			out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", request, translated, response, &param)
			if gjson.GetBytes(out, "output.0.type").String() != callType || gjson.GetBytes(out, "output.0.name").String() != "lookup" || gjson.GetBytes(out, "output.0.namespace").String() != namespaces[1] {
				t.Fatalf("nonstream identity not restored: %s", out)
			}
			param = nil
			chunks := []string{
				fmt.Sprintf(`data: {"id":"fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":%q,"arguments":%q}}]}}]}`, aliases[1], arguments),
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			seen := make(map[string]bool)
			for _, chunk := range chunks {
				frames := sdktranslator.Default().TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "fixture-model", request, translated, []byte(chunk), &param)
				for _, frame := range frames {
					for _, line := range bytes.Split(frame, []byte("\n")) {
						if !bytes.HasPrefix(line, []byte("data:")) {
							continue
						}
						payload := gjson.ParseBytes(bytes.TrimSpace(line[5:]))
						event := payload.Get("type").String()
						var item gjson.Result
						switch event {
						case "response.output_item.added", "response.output_item.done":
							item = payload.Get("item")
						case "response.completed":
							item = payload.Get("response.output.0")
						default:
							continue
						}
						seen[event] = true
						if item.Get("type").String() != callType || item.Get("name").String() != "lookup" || item.Get("namespace").String() != namespaces[1] {
							t.Fatalf("%s identity not restored: %s", event, payload.Raw)
						}
					}
				}
			}
			for _, event := range []string{"response.output_item.added", "response.output_item.done", "response.completed"} {
				if !seen[event] {
					t.Fatalf("stream event %s missing", event)
				}
			}
		})
	}
}

func TestResponsesChatHistoryAliasesPreserveDeclarations(t *testing.T) {
	name := "shared_" + strings.Repeat("x", 60)
	declarations := fmt.Sprintf(`"tools":[{"type":"namespace","name":"first","tools":[{"type":"function","name":%q}]},{"type":"namespace","name":"second","tools":[{"type":"function","name":%q}]}]`, name, name)
	base := []byte(`{` + declarations + `}`)
	request := []byte(fmt.Sprintf(`{%s,"input":[{"type":"function_call","name":%q,"call_id":"history","arguments":"{}"},{"type":"function_call_output","call_id":"history","output":"result"}],"tool_choice":{"type":"function","name":%q}}`, declarations, name, name))
	plain := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("offline", base, false)
	body := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("offline", request, false)
	if gjson.GetBytes(plain, "tools").Raw != gjson.GetBytes(body, "tools").Raw {
		t.Fatalf("history changed declared aliases: before=%s after=%s", plain, body)
	}
	alias := gjson.GetBytes(body, "messages.0.tool_calls.0.function.name").String()
	if len(alias) > 64 || alias == "" || alias != gjson.GetBytes(body, "tool_choice.function.name").String() {
		t.Fatalf("history and choice alias mismatch: %s", body)
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if alias == tool.Get("function.name").String() {
			t.Fatalf("ambiguous history acquired a declared identity: %s", body)
		}
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var param any
			var item gjson.Result
			if stream {
				chunks := []string{
					fmt.Sprintf(`data: {"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":%q,"arguments":"{}"}}]}}]}`, alias),
					`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
					"data: [DONE]",
				}
				for _, chunk := range chunks {
					for _, frame := range sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "offline", request, body, []byte(chunk), &param) {
						kind, event := openAIResponsesSSEData(t, frame)
						if kind == "response.completed" {
							item = event.Get("response.output.0")
						}
					}
				}
			} else {
				raw := []byte(fmt.Sprintf(`{"id":"r","choices":[{"index":0,"message":{"tool_calls":[{"id":"c","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, alias))
				out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, "offline", request, body, raw, &param)
				item = gjson.GetBytes(out, "output.0")
			}
			if item.Get("name").String() != name || item.Get("namespace").Exists() || item.Get("type").String() != "function_call" {
				t.Fatalf("unknown history identity changed: %s", item.Raw)
			}
		})
	}
}
