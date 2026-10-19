package chat_completions

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeRegisteredToolStrict(t *testing.T) {
	for _, test := range []struct {
		name, functionStrict, rootStrict, want string
	}{
		{"function true", "true", "", "true"},
		{"function false", "false", "", "false"},
		{"root true", "", "true", "true"},
		{"root false", "", "false", "false"},
		{"function true wins", "true", "false", "true"},
		{"function false wins", "false", "true", "false"},
		{"omitted", "", "", ""},
		{"function null", "null", "true", ""},
		{"function string", `"true"`, "true", ""},
		{"function number", "1", "false", ""},
		{"root null", "", "null", ""},
		{"root string", "", `"false"`, ""},
		{"root number", "", "0", ""},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				function := `"name":"fixture"`
				if test.functionStrict != "" {
					function += `,"strict":` + test.functionStrict
				}
				tool := `"type":"function","function":{` + function + `}`
				if test.rootStrict != "" {
					tool += `,"strict":` + test.rootStrict
				}
				input := []byte(`{"messages":[{"role":"user","content":"fixture"}],"tools":[{` + tool + `}]}`)
				envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.RequestEnvelope{
					Format: sdktranslator.FormatOpenAI, Model: "fixture-claude", Body: input, Stream: stream,
				})
				if envelope.Err != nil {
					t.Fatal(envelope.Err)
				}
				if got := gjson.GetBytes(envelope.Body, "tools.0.strict").Raw; got != test.want {
					t.Fatalf("strict=%q, want %q; body=%s", got, test.want, envelope.Body)
				}
			})
		}
	}
}

func TestClaudeRegisteredThinkingSummaryVisibility(t *testing.T) {
	for _, test := range []struct{ name, flags, want string }{
		{"effort only", "", ""},
		{"include", `,"include_reasoning":true`, "summarized"},
		{"exclude", `,"reasoning":{"exclude":true}`, "omitted"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				input := []byte(`{"reasoning_effort":"high","messages":[{"role":"user","content":"fixture"}]` + test.flags + `}`)
				envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.RequestEnvelope{
					Format: sdktranslator.FormatOpenAI, Model: "claude-opus-5-5", Body: input, Stream: stream,
				})
				if envelope.Err != nil {
					t.Fatal(envelope.Err)
				}
				if got := gjson.GetBytes(envelope.Body, "thinking.display").String(); got != test.want {
					t.Fatalf("display=%q, want %q; body=%s", got, test.want, envelope.Body)
				}
			})
		}
	}
}

func claudeToolContractRequest(t *testing.T, names []string, choice any) []byte {
	t.Helper()
	tools := make([]any, 0, len(names))
	calls := make([]any, 0, len(names))
	messages := []any{map[string]any{"role": "assistant", "content": "fixture"}}
	for index, name := range names {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name}})
		id := fmt.Sprintf("call_%d", index)
		calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": `{"n":9007199254740993}`}})
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": "fixture"})
	}
	messages[0].(map[string]any)["tool_calls"] = calls
	messages = append(messages, map[string]any{"role": "user", "content": "fixture"})
	body, err := json.Marshal(map[string]any{"messages": messages, "tools": tools, "tool_choice": choice})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestClaudeRegisteredToolNames(t *testing.T) {
	names := []string{"mcp.server.special:get_time", "mcp:server.special.get_time", strings.Repeat("t", 64) + "a", strings.Repeat("t", 64) + "b", "clean_tool", "工具"}
	validName := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	for _, stream := range []bool{false, true} {
		for _, choice := range []string{"named", "allowed"} {
			t.Run(fmt.Sprintf("%s/stream=%t", choice, stream), func(t *testing.T) {
				selection := any(map[string]any{"type": "function", "function": map[string]any{"name": names[0]}})
				if choice == "allowed" {
					selection = map[string]any{"type": "allowed_tools", "allowed_tools": map[string]any{"mode": "required", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": names[0]}}}}}
				}
				input := claudeToolContractRequest(t, names, selection)
				envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.RequestEnvelope{
					Format: sdktranslator.FormatOpenAI, Model: "fixture-claude", Body: input, Stream: stream,
				})
				if envelope.Err != nil {
					t.Fatal(envelope.Err)
				}
				mapped := util.ClaudeFunctionNameMap(input)
				seen := make(map[string]bool)
				for index, tool := range gjson.GetBytes(envelope.Body, "tools").Array() {
					name := tool.Get("name").String()
					if name != mapped[names[index]] || !validName.MatchString(name) || seen[name] {
						t.Fatalf("invalid or conflicting declaration name=%q; body=%s", name, envelope.Body)
					}
					seen[name] = true
					if tool.Get("input_schema.type").String() != "object" || !tool.Get("input_schema.properties").IsObject() {
						t.Fatalf("parameterless schema changed: %s", tool.Raw)
					}
				}
				for index, name := range names {
					call := gjson.GetBytes(envelope.Body, fmt.Sprintf("messages.0.content.%d", index+1))
					if call.Get("name").String() != mapped[name] || call.Get("input.n").Raw != "9007199254740993" {
						t.Fatalf("historical name or argument changed: %s", call.Raw)
					}
				}
				if choice == "named" && gjson.GetBytes(envelope.Body, "tool_choice.name").String() != mapped[names[0]] {
					t.Fatalf("named choice does not address declaration: %s", envelope.Body)
				}
				if choice == "allowed" && (gjson.GetBytes(envelope.Body, "tools.#").Int() != 1 || gjson.GetBytes(envelope.Body, "tool_choice.type").String() != "any") {
					t.Fatalf("allowed tool selection changed: %s", envelope.Body)
				}
			})
		}
	}
}

func TestClaudeRegisteredToolNameResponses(t *testing.T) {
	names := []string{"mcp.server.special:get_time", "mcp:server.special.get_time", strings.Repeat("t", 64) + "a", strings.Repeat("t", 64) + "b", "clean_tool"}
	input := claudeToolContractRequest(t, names, "auto")
	mapped := util.ClaudeFunctionNameMap(input)
	var content []any
	frames := []string{`data: {"type":"message_start","message":{"id":"fixture","model":"fixture-claude"}}`}
	for index, name := range names {
		id := fmt.Sprintf("call_%d", index)
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": mapped[name], "input": map[string]json.Number{"n": "9007199254740993"}})
		frames = append(frames,
			fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, index, id, mapped[name]),
			fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":"{\"n\":9007199254740993}"}}`, index),
			fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index),
		)
	}
	frames = append(frames, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `data: {"type":"message_stop"}`)
	native, err := json.Marshal(map[string]any{"type": "message", "id": "fixture", "model": "fixture-claude", "content": content, "stop_reason": "tool_use"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"native JSON", native},
		{"aggregated SSE", []byte(strings.Join(frames, "\n\n"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state any
			out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-claude", input, nil, test.body, &state)
			calls := gjson.GetBytes(out, "choices.0.message.tool_calls").Array()
			if len(calls) != len(names) {
				t.Fatalf("tool count=%d, want %d; body=%s", len(calls), len(names), out)
			}
			for index, call := range calls {
				if call.Get("function.name").String() != names[index] || call.Get("function.arguments").String() != `{"n":9007199254740993}` {
					t.Fatalf("tool identity or arguments changed: %s", call.Raw)
				}
			}
		})
	}
	t.Run("stream", func(t *testing.T) {
		var state any
		var restored []string
		for _, frame := range frames {
			for _, out := range sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-claude", input, nil, []byte(frame), &state) {
				if call := gjson.GetBytes(out, "choices.0.delta.tool_calls.0"); call.Exists() {
					if call.Get("function.arguments").String() != `{"n":9007199254740993}` {
						t.Fatalf("arguments changed: %s", out)
					}
					restored = append(restored, call.Get("function.name").String())
				}
			}
		}
		if strings.Join(restored, "\x00") != strings.Join(names, "\x00") {
			t.Fatalf("restored names=%q, want %q", restored, names)
		}
	})
}

func TestClaudeRegisteredToolNameResponseSource(t *testing.T) {
	original := claudeToolContractRequest(t, []string{"fixture.name"}, "auto")
	working := claudeToolContractRequest(t, []string{"fixture:name"}, "auto")
	for _, test := range []struct {
		name              string
		original, working []byte
		upstream, want    string
	}{
		{"original wins", original, working, "fixture_name", "fixture.name"},
		{"missing original", nil, working, "fixture_name", "fixture:name"},
		{"invalid original", []byte(`{"tools":`), working, "fixture_name", "fixture:name"},
		{"unknown response name", original, working, "unknown_tool", "unknown_tool"},
		{"no request", nil, nil, "fixture_name", "fixture_name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state any
			native := []byte(fmt.Sprintf(`{"type":"message","content":[{"type":"tool_use","id":"call_0","name":%q,"input":{}}]}`, test.upstream))
			out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-claude", test.original, test.working, native, &state)
			if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String(); got != test.want {
				t.Fatalf("nonstream name=%q, want %q; body=%s", got, test.want, out)
			}
			state = nil
			start := []byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_0","name":%q}}`, test.upstream))
			sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-claude", test.original, test.working, start, &state)
			frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-claude", test.original, test.working, []byte(`data: {"type":"content_block_stop","index":0}`), &state)
			if len(frames) != 1 || gjson.GetBytes(frames[0], "choices.0.delta.tool_calls.0.function.name").String() != test.want {
				t.Fatalf("stream name not restored: %q", frames)
			}
		})
	}
}

func TestClaudeRegisteredToolNameStreamIsolation(t *testing.T) {
	names := []string{"fixture.name", "fixture:name"}
	requests := [][]byte{claudeToolContractRequest(t, names[:1], "auto"), claudeToolContractRequest(t, names[1:], "auto")}
	states := make([]any, 2)
	for _, frame := range []string{
		`data: {"type":"message_start","message":{"id":"fixture"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_0","name":"fixture_name"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	} {
		for index, input := range requests {
			out := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture-claude", input, nil, []byte(frame), &states[index])
			for _, body := range out {
				if call := gjson.GetBytes(body, "choices.0.delta.tool_calls.0"); call.Exists() && call.Get("function.name").String() != names[index] {
					t.Fatalf("stream=%d request-specific name not restored: %s", index, body)
				}
			}
		}
	}
}
