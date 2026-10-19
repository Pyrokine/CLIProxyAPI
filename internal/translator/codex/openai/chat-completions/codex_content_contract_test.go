package chat_completions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexChatRegisteredAttachments(t *testing.T) {
	for _, tc := range []struct {
		name, part, kind, field, value string
	}{
		{"file ID", `{"type":"file","file":{"file_id":"file-fixture","filename":"fixture.pdf"}}`, "input_file", "file_id", "file-fixture"},
		{"file URL", `{"type":"file","file":{"file_url":"https://example.com/fixture.pdf","filename":"fixture.pdf"}}`, "input_file", "file_url", "https://example.com/fixture.pdf"},
		{"inline file", `{"type":"file","file":{"file_data":"data:application/pdf;base64,AA==","filename":"fixture.pdf"}}`, "input_file", "file_data", "data:application/pdf;base64,AA=="},
		{"audio", `{"type":"input_audio","input_audio":{"data":"SUQzBA==","format":"mp3"}}`, "input_audio", "data", "SUQzBA=="},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				raw := []byte(`{"messages":[{"role":"user","content":"history"},{"role":"assistant","content":"reply"},{"role":"user","content":[` + tc.part + `]}]}`)
				out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "fixture-model", raw, stream)
				if err != nil {
					t.Fatal(err)
				}
				part := gjson.GetBytes(out, "input.2.content.0")
				if part.Get("type").String() != tc.kind || part.Get(tc.field).String() != tc.value {
					t.Fatalf("attachment changed: %s", out)
				}
				if tc.kind == "input_file" && part.Get("filename").String() != "fixture.pdf" {
					t.Fatalf("filename missing: %s", out)
				}
				if tc.kind == "input_audio" && part.Get("format").String() != "mp3" {
					t.Fatalf("audio format missing: %s", out)
				}
			})
		}
	}
}

func TestCodexChatRegisteredEmptyTurnRejection(t *testing.T) {
	for _, part := range []string{
		`{"type":"file","file":{"filename":"empty.pdf"}}`,
		`{"type":"input_audio","input_audio":{"format":"mp3"}}`,
		`{"type":"input_audio","input_audio":{"data":123,"format":"mp3"}}`,
		`{"type":"input_audio","input_audio":{"data":"","format":"wav"}}`,
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", part, stream), func(t *testing.T) {
				raw := []byte(`{"messages":[{"role":"user","content":"history"},{"role":"assistant","content":"reply"},{"role":"user","content":[{"type":"text","text":""},` + part + `]},{"role":"user","content":"later"}]}`)
				body, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "fixture-model", raw, stream)
				if err == nil || body != nil {
					t.Fatalf("checked rejection: body=%s error=%v", body, err)
				}
				envelope := sdktranslator.TranslateRequestEnvelope(t.Context(), sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, sdktranslator.RequestEnvelope{Model: "fixture-model", Body: raw, Stream: stream})
				if envelope.Err == nil || envelope.Body != nil {
					t.Fatalf("envelope rejection: %+v", envelope)
				}
				if body := ConvertOpenAIRequestToCodex("fixture-model", raw, stream); body != nil {
					t.Fatalf("byte rejection: %s", body)
				}
			})
		}
	}
}

func TestCodexChatNameIdentityAcrossRegisteredRoundTrip(t *testing.T) {
	declared := "tool:search"
	history := "tool.search"
	for _, tools := range []string{
		`[{"type":"function","function":{"name":"tool:search"}},{"type":"function","function":{"name":"工具 run"}}]`,
		`[{"type":"function","function":{"name":"tool:search"}},{"type":"function","function":{"name":"工具 run"}},{"type":"function","function":{"name":"tool:search"}}]`,
	} {
		raw := []byte(`{"tools":` + tools + `,"tool_choice":{"type":"function","function":{"name":"tool:search"}},"messages":[{"role":"assistant","tool_calls":[{"id":"history-call","type":"function","function":{"name":"tool.search","arguments":"{}"}}]},{"role":"tool","tool_call_id":"history-call","content":"result"}]}`)
		body, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "fixture-model", raw, false)
		if err != nil {
			t.Fatal(err)
		}
		declaredName := gjson.GetBytes(body, "tools.0.name").String()
		historicalName := gjson.GetBytes(body, "input.0.name").String()
		if declaredName == historicalName || declaredName != "tool_search" || gjson.GetBytes(body, "tool_choice.name").String() != declaredName {
			t.Fatalf("declaration or history identity changed: %s", body)
		}
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			if len(tool.Get("name").String()) > 64 || strings.ContainsAny(tool.Get("name").String(), ".: ") {
				t.Fatalf("invalid upstream tool name: %s", tool.Raw)
			}
		}
		for mapped, want := range map[string]string{declaredName: declared, historicalName: history} {
			item := fmt.Sprintf(`{"type":"function_call","id":"item","call_id":"call","name":%q,"arguments":"{}"}`, mapped)
			nonStream := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", raw, body, []byte(`{"type":"response.completed","response":{"output":[`+item+`]}}`), nil)
			if got := gjson.GetBytes(nonStream, "choices.0.message.tool_calls.0.function.name").String(); got != want {
				t.Fatalf("non-stream name=%q want=%q", got, want)
			}
			var state any
			frames := sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", raw, body, []byte(`data: {"type":"response.output_item.added","output_index":0,"item":`+item+`}`), &state)
			if len(frames) != 1 || gjson.GetBytes(frames[0], "choices.0.delta.tool_calls.0.function.name").String() != want {
				t.Fatalf("stream name not restored: %s", frames)
			}
		}
	}
}

func TestCodexChatStringifiedImageToolOutput(t *testing.T) {
	for _, tc := range []struct {
		content, kind string
		array         bool
	}{
		{`[{"type":"input_text","text":"caption"},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"original"}]`, "input_image", true},
		{`[{"type":"text","text":"caption"},{"type":"image_url","image_url":{"url":"https://example.com/image.png","detail":"high"}}]`, "input_image", true},
		{`[{"type":"input_image","file_id":"image-fixture"}]`, "input_image", true},
		{`[{"type":"input_text","text":"caption"}]`, "", false},
		{`[{"type":"input_image","detail":"high"}]`, "", false},
		{`{"status":"ok"}`, "", false},
		{`[{"type":"input_image","image_url":"https://example.com/image.png"}] trailing`, "", false},
	} {
		t.Run(tc.content, func(t *testing.T) {
			encoded, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"type":"function","id":"call","function":{"name":"view_image","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call","content":` + string(encoded) + `}]}`)
			out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "fixture-model", raw, true)
			if err != nil {
				t.Fatal(err)
			}
			output := gjson.GetBytes(out, "input.1.output")
			if output.IsArray() != tc.array {
				t.Fatalf("tool output shape changed: %s", out)
			}
			if !tc.array {
				if output.String() != tc.content {
					t.Fatalf("text output changed: %s", out)
				}
				return
			}
			parts := output.Array()
			if parts[len(parts)-1].Get("type").String() != tc.kind {
				t.Fatalf("image tool output missing: %s", out)
			}
			if len(parts) == 2 && (parts[0].Get("type").String() != "input_text" || parts[0].Get("text").String() != "caption") {
				t.Fatalf("caption changed: %s", out)
			}
		})
	}
}

func TestCodexChatGeneratedImageRegisteredDelivery(t *testing.T) {
	var state any
	send := func(frame string) [][]byte {
		return sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", nil, nil, []byte("data: "+frame), &state)
	}
	for _, frame := range []string{
		`{"type":"response.image_generation_call.partial_image","item_id":"image-a","output_index":0,"output_format":"png","partial_image_b64":"AA=="}`,
		`{"type":"response.image_generation_call.partial_image","item_id":"image-b","output_index":1,"output_format":"png","partial_image_b64":"AA=="}`,
	} {
		out := send(frame)
		if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String() != "data:image/png;base64,AA==" {
			t.Fatalf("image delivery missing: %s", out)
		}
		if out := send(frame); len(out) != 0 {
			t.Fatalf("duplicate image: %s", out)
		}
	}
	if out := send(`{"type":"response.output_item.done","output_index":0,"item":{"id":"image-a","type":"image_generation_call","output_format":"png","result":"AA=="}}`); len(out) != 0 {
		t.Fatalf("image done duplicated partial: %s", out)
	}
	completed := `{"type":"response.completed","response":{"output":[{"id":"image-a","type":"image_generation_call","output_format":"png","result":"AA=="},{"id":"image-b","type":"image_generation_call","output_format":"webp","result":"AQ=="},{"type":"message","content":[{"type":"output_text","text":"caption"}]}]}}`
	out := send(completed)
	if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String() != "data:image/webp;base64,AQ==" || gjson.GetBytes(out[0], "choices.0.delta.content").String() != "caption" || gjson.GetBytes(out[0], "choices.0.finish_reason").String() != "stop" {
		t.Fatalf("terminal image, caption or finish reason missing: %s", out)
	}
	if out := send(`{"type":"response.image_generation_call.partial_image","item_id":"late","partial_image_b64":"AA=="}`); len(out) != 0 {
		t.Fatalf("image emitted after terminal: %s", out)
	}
	nonStream := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", nil, nil, []byte(completed), nil)
	if len(gjson.GetBytes(nonStream, "choices.0.message.images").Array()) != 2 || gjson.GetBytes(nonStream, "choices.0.message.content").String() != "caption" {
		t.Fatalf("non-stream image or caption missing: %s", nonStream)
	}
}

func TestCodexChatImageIdentityLearnedAtDone(t *testing.T) {
	var state any
	frames := []string{
		`{"type":"response.image_generation_call.partial_image","item_id":"image","partial_image_b64":"AA=="}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"image","result":"AA=="}}`,
		`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"AA=="}]}}`,
	}
	for index, frame := range frames {
		out := sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", nil, nil, []byte("data: "+frame), &state)
		if index == 1 {
			if len(out) != 0 {
				t.Fatalf("done image duplicated partial: %s", out)
			}
			continue
		}
		if len(out) != 1 {
			t.Fatalf("missing partial or terminal: %s", out)
		}
		if index == 2 && gjson.GetBytes(out[0], "choices.0.delta.images").Exists() {
			t.Fatalf("terminal index did not retain image identity: %s", out)
		}
	}
}

func TestCodexChatPatchHistoryWrapperBoundaries(t *testing.T) {
	for _, tc := range []struct{ call, tools, kind, want string }{
		{`{"type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"p\"}"}}`, `[{"type":"custom","name":"apply_patch"}]`, "custom_tool_call", "p"},
		{`{"type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"p\",\"extra\":1}"}}`, `[{"type":"custom","name":"apply_patch"}]`, "custom_tool_call", `{"input":"p","extra":1}`},
		{`{"type":"custom","custom":{"name":"apply_patch","input":"{\"input\":\"p\"}"}}`, `[{"type":"custom","name":"apply_patch"}]`, "custom_tool_call", `{"input":"p"}`},
		{`{"type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"p\"}"}}`, `[{"type":"custom","name":"apply_patch"},{"type":"function","function":{"name":"apply_patch"}}]`, "function_call", `{"input":"p"}`},
		{`{"type":"function","function":{"name":"apply_patch","arguments":"raw patch"}}`, `[{"type":"custom","name":"apply_patch"}]`, "custom_tool_call", "raw patch"},
	} {
		raw := []byte(`{"tools":` + tc.tools + `,"messages":[{"role":"assistant","tool_calls":[` + tc.call + `]}]}`)
		out, err := sdktranslator.TranslateRequestChecked(t.Context(), sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "fixture-model", raw, false)
		if err != nil {
			t.Fatal(err)
		}
		field := "input"
		if tc.kind == "function_call" {
			field = "arguments"
		}
		if gjson.GetBytes(out, "input.0.type").String() != tc.kind || gjson.GetBytes(out, "input.0."+field).String() != tc.want {
			t.Fatalf("patch history boundary changed: %s", out)
		}
	}
}

func TestCodexChatToolSnapshotTail(t *testing.T) {
	for _, tc := range []struct {
		kind, event, field, name, prefix, full string
	}{
		{"function_call", "response.function_call_arguments", "arguments", "search", `{"q":`, `{"q":"测试"}`},
		{"custom_tool_call", "response.custom_tool_call_input", "input", "apply_patch", "*** Begin Patch\n", "*** Begin Patch\n*** Add File: fixture.txt\n+测试\n*** End Patch"},
	} {
		for _, completion := range []string{"arguments done", "item done", "terminal"} {
			t.Run(tc.kind+"/"+completion, func(t *testing.T) {
				request := []byte(`{"messages":[{"role":"user","content":"fixture"}]}`)
				var state any
				var arguments strings.Builder
				send := func(event map[string]any) {
					t.Helper()
					payload, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					frames := sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", request, request, append([]byte("data: "), payload...), &state)
					for _, frame := range frames {
						for _, call := range gjson.GetBytes(frame, "choices.0.delta.tool_calls").Array() {
							arguments.WriteString(call.Get("function.arguments").String())
						}
					}
				}
				item := map[string]any{"type": tc.kind, "id": "item-fixture", "call_id": "call-fixture", "name": tc.name, tc.field: ""}
				send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
				send(map[string]any{"type": tc.event + ".delta", "item_id": "item-fixture", "output_index": 0, "delta": tc.prefix})
				item[tc.field] = tc.full
				switch completion {
				case "arguments done":
					send(map[string]any{"type": tc.event + ".done", "item_id": "item-fixture", "output_index": 0, tc.field: tc.full})
					send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				case "item done":
					send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				}
				send(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{item}}})
				if got := arguments.String(); got != tc.full {
					t.Fatalf("完整工具参数未交付：got=%q want=%q", got, tc.full)
				}
				if err := sdktranslator.ResponseTranslationError(&state); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCodexChatToolSnapshotConflicts(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		for _, completion := range []string{"arguments done", "item done", "terminal"} {
			for _, completed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/completed=%t", kind, completion, completed), func(t *testing.T) {
					field, event := "arguments", "response.function_call_arguments"
					if kind == "custom_tool_call" {
						field, event = "input", "response.custom_tool_call_input"
					}
					var state any
					send := func(value string) [][]byte {
						return sdktranslator.TranslateStream(t.Context(), sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "fixture-model", nil, nil, []byte("data: "+value), &state)
					}
					item := fmt.Sprintf(`{"type":%q,"id":"item","call_id":"call","name":"tool",%q:""}`, kind, field)
					send(`{"type":"response.output_item.added","output_index":0,"item":` + item + `}`)
					send(fmt.Sprintf(`{"type":%q,"item_id":"item","delta":"head"}`, event+".delta"))
					snapshot := "wrong"
					if completed {
						done := fmt.Sprintf(`{"type":%q,"item_id":"item",%q:"headtail"}`, event+".done", field)
						send(done)
						if frames := send(done); len(frames) != 0 || sdktranslator.ResponseTranslationError(&state) != nil {
							t.Fatalf("重复快照改变交付：frames=%s error=%v", frames, sdktranslator.ResponseTranslationError(&state))
						}
						snapshot = "headtailchanged"
					}
					item = fmt.Sprintf(`{"type":%q,"id":"item","call_id":"call","name":"tool",%q:%q}`, kind, field, snapshot)
					var value string
					switch completion {
					case "arguments done":
						value = fmt.Sprintf(`{"type":%q,"item_id":"item",%q:%q}`, event+".done", field, snapshot)
					case "item done":
						value = `{"type":"response.output_item.done","output_index":0,"item":` + item + `}`
					case "terminal":
						value = `{"type":"response.completed","response":{"output":[` + item + `]}}`
					}
					frames := send(value)
					err := sdktranslator.ResponseTranslationError(&state)
					status, ok := err.(interface{ StatusCode() int })
					if len(frames) != 0 || !ok || status.StatusCode() != 502 {
						t.Fatalf("冲突快照没有返回 502：frames=%s error=%v", frames, err)
					}
					if frames := send(`{"type":"response.completed","response":{"output":[]}}`); len(frames) != 0 {
						t.Fatalf("转换失败后交付了终态：%s", frames)
					}
				})
			}
		}
	}
}
