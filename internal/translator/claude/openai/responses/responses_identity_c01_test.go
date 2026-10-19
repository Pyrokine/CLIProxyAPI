package responses

import (
	"context"
	"fmt"
	"strings"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeResponsesNativeMessageJSON(t *testing.T) {
	request := []byte(`{"model":"client-model","tools":[{"type":"function","name":"lookup"}]}`)
	native := []byte(`{"type":"message","id":"msg_native","model":"claude-native","content":[{"type":"thinking","thinking":"plan\n","signature":"native-signature"},{"type":"redacted_thinking","data":"encrypted-thinking"},{"type":"text","text":"中文\t<>&\b","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"source"}]},{"type":"tool_use","id":"call_native","name":"lookup","input":{"count":9007199254740993,"command":"2>&1 <work>"}},{"type":"server_tool_use","id":"search_native","name":"web_search","input":{"query":"native query"}},{"type":"web_search_tool_result","tool_use_id":"search_native","content":[{"type":"web_search_result","url":"https://example.com","title":"source"}]},{"type":"text","text":"last"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":4,"cache_read_input_tokens":5,"output_tokens":7}}`)
	for _, reason := range []string{"end_turn", "max_tokens", "pause_turn"} {
		t.Run(reason, func(t *testing.T) {
			raw, err := sjson.SetBytes(native, "stop_reason", reason)
			if err != nil {
				t.Fatal(err)
			}
			var state any
			body := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatClaude,
				sdktranslator.FormatOpenAIResponse, "fallback-model", request, request, raw, &state)
			response := gjson.ParseBytes(body)
			if sdktranslator.ResponseTranslationError(&state) != nil || !gjson.ValidBytes(body) ||
				response.Get("id").String() != "msg_native" || response.Get("model").String() != "claude-native" {
				t.Fatalf("native response metadata = %s", body)
			}
			wantTypes := []string{"reasoning", "reasoning", "message", "function_call", "web_search_call", "message"}
			items := response.Get("output").Array()
			if len(items) != len(wantTypes) {
				t.Fatalf("native output = %s", response.Get("output").Raw)
			}
			for index, kind := range wantTypes {
				if items[index].Get("type").String() != kind {
					t.Fatalf("native item %d = %s, want %s", index, items[index].Raw, kind)
				}
			}
			if items[0].Get("summary.0.text").String() != "plan\n" || items[0].Get("encrypted_content").String() != "native-signature" ||
				items[1].Get("encrypted_content").String() != claudeResponsesRedactedThinkingPrefix+"encrypted-thinking" ||
				items[2].Get("content.0.text").String() != "中文\t<>&\b" || items[2].Get("content.0.annotations.#").Int() != 1 {
				t.Fatalf("native reasoning or cited text changed: %s", body)
			}
			arguments := items[3].Get("arguments").String()
			if items[3].Get("call_id").String() != "call_native" ||
				gjson.Get(arguments, "count").Raw != "9007199254740993" || gjson.Get(arguments, "command").String() != "2>&1 <work>" ||
				items[4].Get("action.query").String() != "native query" || items[5].Get("content.0.text").String() != "last" {
				t.Fatalf("native tools or final text changed: %s", body)
			}
			_, status, details := claudeResponsesTerminalState(reason)
			if response.Get("status").String() != status || items[2].Get("status").String() != "completed" ||
				items[5].Get("status").String() != status || len(details) > 0 && response.Get("incomplete_details").Raw != string(details) ||
				response.Get("usage.input_tokens").Int() != 12 || response.Get("usage.output_tokens").Int() != 7 ||
				response.Get("usage.input_tokens_details.cached_tokens").Int() != 5 || response.Get("usage.total_tokens").Int() != 19 {
				t.Fatalf("native completion or usage changed: %s", body)
			}
		})
	}
}

func TestClaudeResponsesPatchSnapshotContract(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	for _, tc := range []struct {
		name, fragment, snapshot, want string
		invalid                        bool
	}{
		{name: "partial extension", fragment: `{"input":"pre`, snapshot: `{"input":"prefix"}`, want: "prefix"},
		{name: "partial conflict", fragment: `{"input":"wrong`, snapshot: `{"input":"prefix"}`, invalid: true},
		{name: "complete conflict", fragment: `{"input":"pre"}`, snapshot: `{"input":"prefix"}`, invalid: true},
		{name: "invalid snapshot", fragment: `{"input":"pre`, snapshot: `{"input":7}`, invalid: true},
		{name: "equivalent snapshot", fragment: `{"input":"x\n"}`, snapshot: `{"input":"x\u000a"}`, want: "x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fragment, err := sjson.SetBytes([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta"}}`), "delta.partial_json", tc.fragment)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte(strings.Join([]string{
				`data: {"type":"message_start","message":{"id":"msg_snapshot"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"apply_patch","input":{}}}`,
				"data: " + string(fragment),
				fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"apply_patch","input":%s}}`, tc.snapshot),
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_stop"}`,
			}, "\n"))
			var state any
			body := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatClaude,
				sdktranslator.FormatOpenAIResponse, "claude-test", request, nil, raw, &state)
			response := gjson.ParseBytes(body)
			err = sdktranslator.ResponseTranslationError(&state)
			if tc.invalid {
				status, ok := err.(interface{ StatusCode() int })
				if err == nil || !ok || status.StatusCode() != 502 || response.Get("type").String() != "response.failed" ||
					response.Get("response.status").String() != "failed" || response.Get("response.error.code").String() != "invalid_tool_arguments" {
					t.Fatalf("snapshot failure = %s, error = %v", body, err)
				}
			} else if err != nil || response.Get("status").String() != "completed" || response.Get("output.0.input").String() != tc.want {
				t.Fatalf("snapshot input = %s, error = %v", body, err)
			}
		})
	}
}

func TestClaudeResponsesBuffersToolArgumentsBeforeIdentity(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	chunks := []string{
		`data: {"type":"message_start","message":{"id":"msg_deferred"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"retained\"}"}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"c2","name":"apply_patch","input":{}}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"message_stop"}`,
	}
	t.Run("stream", func(t *testing.T) {
		var state any
		var input strings.Builder
		var completed gjson.Result
		for index, chunk := range chunks {
			frames := sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatClaude,
				sdktranslator.FormatOpenAIResponse, "claude-test", request, nil, []byte(chunk), &state)
			if index == 1 && len(frames) > 0 {
				t.Fatalf("unnamed input emitted events: %q", frames)
			}
			for _, frame := range frames {
				event, data := parseClaudeResponsesSSEEvent(t, frame)
				if event == "response.custom_tool_call_input.delta" {
					input.WriteString(data.Get("delta").String())
				}
				if event == "response.completed" {
					completed = data.Get("response")
				}
			}
		}
		if sdktranslator.ResponseTranslationError(&state) != nil || input.String() != "retained" ||
			completed.Get("output.0.input").String() != "retained" || completed.Get("output.0.call_id").String() != "c2" {
			t.Fatalf("deferred input = %s, deltas = %q", completed.Raw, input.String())
		}
	})
	t.Run("buffered", func(t *testing.T) {
		var state any
		body := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatClaude,
			sdktranslator.FormatOpenAIResponse, "claude-test", request, nil, []byte(strings.Join(chunks, "\n")), &state)
		if sdktranslator.ResponseTranslationError(&state) != nil || gjson.GetBytes(body, "output.0.input").String() != "retained" ||
			gjson.GetBytes(body, "output.0.call_id").String() != "c2" {
			t.Fatalf("deferred buffered input = %s", body)
		}
	})
}

func TestResponsesIdentityClaudeRequestCustomHistoryRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}],
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]},
			{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","namespace":"mcp__editor","input":{"patch":"x"}},
			{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_text","text":"applied"}]}
		]
	}`)

	output := convertOpenAIResponsesRequestToClaudeDefault("claude-test", request, false)
	if got := gjson.GetBytes(output, "tools.#").Int(); got != 1 {
		t.Fatalf("tool count = %d, want 1; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "tools.0.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("tool name = %q, want qualified name", got)
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("history tool name = %q, want qualified name", got)
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.input.input.patch").String(); got != "x" {
		t.Fatalf("history custom input = %q, want x; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "messages.1.content.0.content").String(); got != "applied" {
		t.Fatalf("history custom output = %q, want applied", got)
	}
}

func TestResponsesIdentityClaudeStreamCustomItemDeltaDoneRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"mcp__editor__apply_patch","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** patch\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var events [][]byte
	for _, chunk := range chunks {
		events = append(
			events, sdktranslator.TranslateStream(
				context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse,
				"claude-test", request, request, chunk, &param,
			)...,
		)
	}

	var added, delta, done, completed gjson.Result
	for _, chunk := range events {
		event, data := parseClaudeResponsesSSEEvent(t, chunk)
		switch event {
		case "response.output_item.added":
			if data.Get("item.type").String() == "custom_tool_call" {
				added = data
			}
		case "response.custom_tool_call_input.delta":
			delta = data
		case "response.output_item.done":
			if data.Get("item.type").String() == "custom_tool_call" {
				done = data
			}
		case "response.completed":
			completed = data
		}
	}

	if got := added.Get("item.namespace").String(); got != "mcp__editor" {
		t.Fatalf("added namespace = %q, want mcp__editor; events=%q", got, events)
	}
	if got := delta.Get("delta").String(); got != "*** patch" {
		t.Fatalf("custom delta = %q, want patch", got)
	}
	if got := done.Get("item.name").String(); got != "apply_patch" {
		t.Fatalf("done name = %q, want apply_patch", got)
	}
	if got := completed.Get("response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("completed type = %q, want custom_tool_call; completed=%s", got, completed.Raw)
	}
	if got := completed.Get("response.output.0.namespace").String(); got != "mcp__editor" {
		t.Fatalf("completed namespace = %q, want mcp__editor", got)
	}
}

func TestResponsesIdentityClaudeNonStreamCustomRoundTrip(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"mcp__editor","tools":[{"type":"custom","name":"apply_patch"}]}]
	}`)
	raw := []byte(strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"mcp__editor__apply_patch","input":{}}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** patch\"}"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	))

	output := sdktranslator.TranslateNonStream(
		context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse,
		"claude-test", request, request, raw, nil,
	)
	if got := gjson.GetBytes(output, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("non-stream type = %q, want custom_tool_call; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "output.0.name").String(); got != "apply_patch" {
		t.Fatalf("non-stream name = %q, want apply_patch", got)
	}
	if got := gjson.GetBytes(output, "output.0.namespace").String(); got != "mcp__editor" {
		t.Fatalf("non-stream namespace = %q, want mcp__editor", got)
	}
	if got := gjson.GetBytes(output, "output.0.input").String(); got != "*** patch" {
		t.Fatalf("non-stream input = %q, want patch", got)
	}
}

func TestResponsesIdentityClaudeRegisteredNamesAreReversible(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools string
	}{
		{name: "long namespace", tools: `[{"type":"namespace","name":"mcp__example_apps__acme_inventory_service","tools":[{"type":"function","name":"acme_inventory_service_get_item_prices"},{"type":"function","name":"acme_inventory_service_get_item_metrics"}]}]`},
		{name: "sanitization collision", tools: `[{"type":"function","name":"a_b"},{"type":"function","name":"a.b"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := []byte(`{"tools":` + tc.tools + `,"input":[{"role":"user","content":"use the tool"},{"type":"function_call","call_id":"history","name":"","arguments":"{}"},{"type":"function_call_output","call_id":"history","output":"ok"}],"tool_choice":{"type":"function","name":""}}`)
			declarations := translatorcommon.NewResponsesToolIdentity(request).Declarations()
			var qualifiedNames []string
			for _, declaration := range declarations {
				qualifiedNames = append(qualifiedNames, declaration.QualifiedName)
			}
			request, _ = sjson.SetBytes(request, "input.1.name", declarations[0].Name)
			request, _ = sjson.SetBytes(request, "input.1.namespace", declarations[0].Namespace)
			request, _ = sjson.SetBytes(request, "tool_choice.name", declarations[0].Name)
			request, _ = sjson.SetBytes(request, "tool_choice.namespace", declarations[0].Namespace)
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, "claude-test", request, false)
			if err != nil {
				t.Fatal(err)
			}
			expectedNames := util.ChatFunctionNameMap(qualifiedNames)
			tools := gjson.GetBytes(out, "tools").Array()
			if len(tools) != len(declarations) {
				t.Fatalf("tool count = %d, want %d; output=%s", len(tools), len(declarations), out)
			}
			seen := make(map[string]bool)
			for index, declaration := range declarations {
				name := tools[index].Get("name").String()
				if name == "" || len(name) > 64 || util.SanitizeClaudeFunctionName(name) != name || seen[name] || name != expectedNames[declaration.QualifiedName] {
					t.Fatalf("invalid or inconsistent name %q for %q; output=%s", name, declaration.QualifiedName, out)
				}
				seen[name] = true
				if index == 0 && (gjson.GetBytes(out, "tool_choice.name").String() != name || gjson.GetBytes(out, "messages.1.content.0.name").String() != name) {
					t.Fatalf("declaration, choice and history names differ: %s", out)
				}
				chunks := [][]byte{
					[]byte(`data: {"type":"message_start","message":{"id":"msg_contract","usage":{"input_tokens":1,"output_tokens":1}}}`),
					[]byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_response","name":%q,"input":{}}}`, name)),
					[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"value\"}"}}`),
					[]byte(`data: {"type":"content_block_stop","index":0}`),
					[]byte(`data: {"type":"message_stop"}`),
				}
				var combined strings.Builder
				for _, chunk := range chunks {
					combined.Write(chunk)
					combined.WriteByte('\n')
				}
				var param any
				nonStream := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatClaude,
					sdktranslator.FormatOpenAIResponse, "claude-test", request, out, []byte(combined.String()), &param)
				item := gjson.GetBytes(nonStream, "output.0")
				if sdktranslator.ResponseTranslationError(&param) != nil || item.Get("name").String() != declaration.Name || item.Get("namespace").String() != declaration.Namespace || item.Get("arguments").String() != `{"query":"value"}` {
					t.Fatalf("non-stream identity was not restored: %s", nonStream)
				}
				param = nil
				added, done, completed := false, false, false
				for _, chunk := range chunks {
					for _, frame := range sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatClaude,
						sdktranslator.FormatOpenAIResponse, "claude-test", request, out, chunk, &param) {
						event, data := parseClaudeResponsesSSEEvent(t, frame)
						var toolItem gjson.Result
						switch event {
						case "response.output_item.added":
							added = true
							toolItem = data.Get("item")
						case "response.output_item.done":
							done = true
							toolItem = data.Get("item")
						case "response.completed":
							completed = true
							toolItem = data.Get("response.output.0")
						}
						if toolItem.Exists() && (toolItem.Get("name").String() != declaration.Name || toolItem.Get("namespace").String() != declaration.Namespace) {
							t.Fatalf("stream identity was not restored: %s", frame)
						}
					}
				}
				if sdktranslator.ResponseTranslationError(&param) != nil || !added || !done || !completed {
					t.Fatal("stream lifecycle did not finish")
				}
			}
		})
	}
}

func TestResponsesIdentityClaudeRegisteredHistoryNamesAreReversible(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
	}{
		{name: "long undeclared history", request: `{"input":[{"type":"function_call","call_id":"only","name":"history_only_tool_name_that_is_also_very_long_exceeding_sixty_four_characters","arguments":"{}"}]}`},
		{name: "declaration precedes history", request: `{"tools":[{"type":"function","name":"tool_x"}],"input":[{"type":"function_call","call_id":"history","name":"tool.x","arguments":"{}"}]}`},
		{name: "colliding history names", request: `{"input":[{"type":"function_call","call_id":"a","name":"a.b","arguments":"{}"},{"type":"function_call","call_id":"b","name":"a_b","arguments":"{}"}]}`},
		{name: "duplicate history identities", request: `{"input":[{"type":"function_call","call_id":"a","name":"a.b","arguments":"{}"},{"type":"function_call","call_id":"b","name":"a.b","arguments":"{}"}]}`},
		{name: "qualified history", request: `{"input":[{"type":"function_call","call_id":"a","namespace":"retired_server","name":"tool.with.dots","arguments":"{}"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := []byte(tc.request)
			identity := translatorcommon.NewResponsesToolIdentity(request)
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAIResponse,
				sdktranslator.FormatClaude, "claude-test", request, false)
			if err != nil {
				t.Fatal(err)
			}
			var toolNames []string
			for _, message := range gjson.GetBytes(out, "messages").Array() {
				for _, part := range message.Get("content").Array() {
					if part.Get("type").String() == "tool_use" {
						toolNames = append(toolNames, part.Get("name").String())
					}
				}
			}
			inputs := gjson.GetBytes(request, "input").Array()
			if len(toolNames) != len(inputs) {
				t.Fatalf("history calls changed: %s", out)
			}
			if tc.name == "declaration precedes history" && (gjson.GetBytes(out, "tools.0.name").String() != "tool_x" || toolNames[0] == "tool_x") {
				t.Fatalf("history stole declaration name: %s", out)
			}
			seen := make(map[string]string)
			for index, mapped := range toolNames {
				original := identity.ResolveName(inputs[index].Get("name").String(), inputs[index].Get("namespace").String())
				if previous := seen[mapped]; mapped == "" || len(mapped) > 64 || util.SanitizeClaudeFunctionName(mapped) != mapped || previous != "" && previous != original {
					t.Fatalf("invalid or colliding history name: %q -> %q", original, mapped)
				}
				seen[mapped] = original
				chunks := [][]byte{
					[]byte(`data: {"type":"message_start","message":{"id":"msg_history","usage":{"input_tokens":1,"output_tokens":1}}}`),
					[]byte(fmt.Sprintf(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_response","name":%q,"input":{}}}`, mapped)),
					[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"value\"}"}}`),
					[]byte(`data: {"type":"content_block_stop","index":0}`),
					[]byte(`data: {"type":"message_stop"}`),
				}
				var combined strings.Builder
				for _, chunk := range chunks {
					combined.Write(chunk)
					combined.WriteByte('\n')
				}
				var param any
				nonStream := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatClaude,
					sdktranslator.FormatOpenAIResponse, "claude-test", request, out, []byte(combined.String()), &param)
				item := gjson.GetBytes(nonStream, "output.0")
				if sdktranslator.ResponseTranslationError(&param) != nil || item.Get("name").String() != original || item.Get("arguments").String() != `{"query":"value"}` {
					t.Fatalf("non-stream history identity was not restored: %s", nonStream)
				}
				param = nil
				added, done, completed := false, false, false
				for _, chunk := range chunks {
					for _, frame := range sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatClaude,
						sdktranslator.FormatOpenAIResponse, "claude-test", request, out, chunk, &param) {
						event, data := parseClaudeResponsesSSEEvent(t, frame)
						var toolItem gjson.Result
						switch event {
						case "response.output_item.added":
							added = true
							toolItem = data.Get("item")
						case "response.output_item.done":
							done = true
							toolItem = data.Get("item")
						case "response.completed":
							completed = true
							toolItem = data.Get("response.output.0")
						}
						if toolItem.Exists() && toolItem.Get("name").String() != original {
							t.Fatalf("stream history identity was not restored: %s", frame)
						}
					}
				}
				if sdktranslator.ResponseTranslationError(&param) != nil || !added || !done || !completed {
					t.Fatal("history stream lifecycle did not finish")
				}
			}
		})
	}
}
