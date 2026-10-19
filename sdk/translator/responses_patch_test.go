package translator

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const patchRequest = `{"tools":[{"type":"custom","name":"apply_patch","description":"This is a FREEFORM tool, so do not wrap the patch in JSON."}],"tool_choice":{"type":"custom","name":"apply_patch"},"input":[]}`

func patchRegistry() *Registry {
	r := NewRegistry()
	r.Register(FormatOpenAIResponse, FormatOpenAI, func(_ string, body []byte, _ bool) []byte {
		return body
	}, ResponseTransform{
		Stream: func(_ context.Context, _ string, original, _ []byte, body []byte, _ *any) [][]byte {
			return [][]byte{body}
		},
		NonStream: func(_ context.Context, _ string, original, _ []byte, body []byte, _ *any) []byte {
			return body
		},
	})
	return r
}

func TestResponsesPatchRequestContract(t *testing.T) {
	r := patchRegistry()
	body, err := r.TranslateRequestChecked(context.Background(), FormatOpenAIResponse, FormatOpenAI, "model", []byte(patchRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "tools.0.type").String() != "function" ||
		gjson.GetBytes(body, "tools.0.parameters.additionalProperties").Bool() ||
		!gjson.GetBytes(body, "tools.0.parameters.additionalProperties").Exists() ||
		gjson.GetBytes(body, "tool_choice.type").String() != "function" {
		t.Fatalf("patch request was not normalized: %s", body)
	}
}

func TestResponsesPatchNonStreamContract(t *testing.T) {
	for _, input := range []string{"*** Begin Patch\n*** End Patch", "中文与🧪\n<>&", ""} {
		t.Run(input, func(t *testing.T) {
			arguments, _ := sjson.Set(`{}`, "input", input)
			response := []byte(`{"id":"resp_1","status":"completed","error":null,"output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"apply_patch","arguments":""}],"usage":{"input_tokens":2}}`)
			response, _ = sjson.SetBytes(response, "output.0.arguments", arguments)
			var param any
			got := patchRegistry().TranslateNonStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", []byte(patchRequest), nil, response, &param)
			if gjson.GetBytes(got, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(got, "output.0.input").String() != input {
				t.Fatalf("patch output was not restored: %s", got)
			}
			if gjson.GetBytes(got, "id").String() != "resp_1" || gjson.GetBytes(got, "output.0.id").String() != "fc_1" || gjson.GetBytes(got, "usage.input_tokens").Int() != 2 {
				t.Fatalf("response metadata changed: %s", got)
			}
		})
	}
}

func TestResponsesPatchIncrementalAndTerminalContract(t *testing.T) {
	request := []byte(patchRequest)
	registry := patchRegistry()
	var param any
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","opaque":{"trace":"kept"}}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"apply_patch","arguments":""}}`,
	}
	var delta string
	for _, frame := range frames {
		registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", request, nil, []byte(frame), &param)
	}
	arguments := `{"input":"中文🧪\n<>&"}`
	for _, fragment := range arguments {
		frame, _ := sjson.SetBytes([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0}`), "delta", string(fragment))
		for _, event := range registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", request, nil, frame, &param) {
			if gjson.GetBytes(event, "type").String() == "response.custom_tool_call_input.delta" {
				delta += gjson.GetBytes(event, "delta").String()
			}
		}
	}
	if delta != "中文🧪\n<>&" {
		t.Fatalf("incremental input = %q", delta)
	}
	if err := ResponseTranslationError(&param); err != nil {
		t.Fatal(err)
	}
	item := []byte(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"apply_patch","arguments":""}`)
	item, _ = sjson.SetBytes(item, "arguments", arguments)
	terminal, _ := sjson.SetRawBytes([]byte(`{"type":"response.done","response":{"id":"resp_1","output":[],"opaque":{"trace":"kept"}}}`), "response.output.0", item)
	out := registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", request, nil, terminal, &param)
	if err := ResponseTranslationError(&param); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || gjson.GetBytes(out[len(out)-1], "type").String() != "response.completed" || gjson.GetBytes(out[len(out)-1], "response.output.0.input").String() != delta {
		t.Fatalf("invalid terminal: %s", bytes.Join(out, nil))
	}
	state := param.(*responsesPatchTranslation)
	if !state.closed || state.bridge != nil || state.client != nil || state.original != nil || state.inner != nil {
		t.Fatal("request translation state retained after completion")
	}
	for _, frame := range [][]byte{[]byte("[DONE]"), terminal, []byte(frames[1])} {
		if out := registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", request, nil, frame, &param); len(out) != 0 {
			t.Fatalf("duplicate event delivered: %s", bytes.Join(out, nil))
		}
	}
	lateFailure := []byte(`{"type":"response.failed","response":{"error":{"message":"late upstream failure"}}}`)
	if out := registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", request, nil, lateFailure, &param); len(out) != 1 || !bytes.Equal(out[0], lateFailure) {
		t.Fatalf("late failure hidden: %s", bytes.Join(out, nil))
	}
}

func TestResponsesPatchInvalidArgumentsFail(t *testing.T) {
	for _, arguments := range []string{
		`{"input":42}`, `{"input":"one","input":"two"}`, `{"input":"unterminated`,
		`{"input":"\ud83e"}`, `{"other":"text"}`, `{"input":"text","extra":1}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			response := []byte(`{"id":"resp_1","status":"completed","error":null,"output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"apply_patch","arguments":""}]}`)
			response, _ = sjson.SetBytes(response, "output.0.arguments", arguments)
			var param any
			got := patchRegistry().TranslateNonStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", []byte(patchRequest), nil, response, &param)
			if ResponseTranslationError(&param) == nil || gjson.GetBytes(got, "type").String() != "response.failed" {
				t.Fatalf("invalid arguments accepted: %s", got)
			}
		})
	}
	var param any
	registry := patchRegistry()
	out := registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", []byte(patchRequest), nil, []byte("data: [DONE]\n\n"), &param)
	if ResponseTranslationError(&param) == nil || len(out) != 1 || !bytes.Contains(out[0], []byte("response.failed")) {
		t.Fatalf("premature sentinel accepted: %s", bytes.Join(out, nil))
	}
	if out := registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", []byte(patchRequest), nil, []byte("data: [DONE]\n\n"), &param); len(out) != 0 {
		t.Fatalf("failure emitted twice: %s", bytes.Join(out, nil))
	}
}

func TestResponsesPatchSSEFramingAndIncomplete(t *testing.T) {
	registry := patchRegistry()
	for _, separator := range []string{"\n", "\r\n"} {
		var param any
		frame := "event: response.incomplete" + separator + `data: {"type":"response.incomplete",` + separator + `data: "response":{"id":"resp_1","status":"incomplete","output":[]}}` + separator + separator
		out := registry.TranslateStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", []byte(patchRequest), nil, []byte(frame), &param)
		if err := ResponseTranslationError(&param); err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || !bytes.HasPrefix(out[0], []byte("event: response.incomplete\ndata: ")) || !bytes.HasSuffix(out[0], []byte("\n\n")) {
			t.Fatalf("incomplete framing changed: %s", bytes.Join(out, nil))
		}
	}
}

func TestResponsesPatchHistoryRequestContract(t *testing.T) {
	request := []byte(patchRequest)
	request, _ = sjson.SetRawBytes(request, "input", []byte(`[{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"中文🧪"},{"type":"custom_tool_call_output","call_id":"call_1","output":"done"}]`))
	got, err := patchRegistry().TranslateRequestChecked(context.Background(), FormatOpenAIResponse, FormatOpenAI, "model", request, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "input.0.type").String() != "function_call" || gjson.GetBytes(got, "input.1.type").String() != "function_call_output" || gjson.Parse(gjson.GetBytes(got, "input.0.arguments").String()).Get("input").String() != "中文🧪" {
		t.Fatalf("history conversion failed: %s", got)
	}
	request, _ = sjson.SetBytes(request, "input.0.input", 42)
	if got, err := patchRegistry().TranslateRequestChecked(context.Background(), FormatOpenAIResponse, FormatOpenAI, "model", request, false); err == nil || got != nil {
		t.Fatalf("invalid history accepted: %s, %v", got, err)
	}
}

func TestResponsesPatchNativeAndOrdinaryRequestsUnchanged(t *testing.T) {
	for _, from := range []Format{FormatCodex, FormatOpenAIResponse} {
		var param any
		response := []byte(`{"output":[{"type":"custom_tool_call","name":"apply_patch","input":"native"}]}`)
		got := patchRegistry().TranslateNonStream(context.Background(), from, FormatOpenAIResponse, "model", []byte(patchRequest), nil, response, &param)
		if !bytes.Equal(got, response) {
			t.Fatalf("native response changed: %s", got)
		}
	}
	request := []byte(`{"tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object"}}],"input":[]}`)
	registry := patchRegistry()
	translated, err := registry.TranslateRequestChecked(context.Background(), FormatOpenAIResponse, FormatOpenAI, "model", request, false)
	if err != nil || !bytes.Equal(translated, request) {
		t.Fatalf("ordinary request changed: %s, %v", translated, err)
	}
	response := []byte(`{"id":"resp_ordinary","status":"completed","output":[{"id":"fc_ordinary","type":"function_call","call_id":"call_ordinary","name":"apply_patch","arguments":"{\"input\":42}"}]}`)
	var param any
	if got := registry.TranslateNonStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "model", request, translated, response, &param); !bytes.Equal(got, response) || ResponseTranslationError(&param) != nil {
		t.Fatalf("ordinary function response changed: %s", got)
	}
}

type patchOriginalHooks struct {
	labeledPluginHooks
	originals   [][]byte
	afterBodies [][]byte
}

func (h *patchOriginalHooks) NormalizeResponseBefore(_ context.Context, _, _ Format, _ string, original, _, body []byte, _ bool) []byte {
	h.originals = append(h.originals, bytes.Clone(original))
	return body
}

func (h *patchOriginalHooks) TranslateResponse(_ context.Context, _, _ Format, _ string, original, _, _ []byte, _ bool) ([]byte, bool) {
	h.originals = append(h.originals, bytes.Clone(original))
	return nil, false
}

func (h *patchOriginalHooks) NormalizeResponseAfter(_ context.Context, _, _ Format, _ string, original, _, body []byte, _ bool) []byte {
	h.originals = append(h.originals, bytes.Clone(original))
	h.afterBodies = append(h.afterBodies, bytes.Clone(body))
	return body
}

type patchRecoveryHooks struct {
	labeledPluginHooks
	afterCalled     bool
	translateCalled bool
	recover         bool
}

func (h *patchRecoveryHooks) NormalizeResponseAfter(_ context.Context, _, _ Format, _ string, _, _, _ []byte, stream bool) []byte {
	h.afterCalled = true
	if stream {
		return []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)
	}
	return []byte(`{"status":"completed","output":[]}`)
}

func (h *patchRecoveryHooks) TranslateResponse(_ context.Context, _, _ Format, _ string, _, _, _ []byte, stream bool) ([]byte, bool) {
	h.translateCalled = true
	if !h.recover {
		return nil, false
	}
	if stream {
		return []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`), true
	}
	return []byte(`{"status":"completed","output":[]}`), true
}

func TestResponsesPatchInvalidArgumentsSkipPluginRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{{name: "nonstream"}, {name: "stream", stream: true}} {
		t.Run(tc.name, func(t *testing.T) {
			registry := patchRegistry()
			hooks := &patchRecoveryHooks{}
			ctx := WithPluginHooks(t.Context(), hooks)
			body := []byte(`{"id":"resp_failed","status":"completed","output":[{"id":"fc_failed","type":"function_call","call_id":"call_failed","name":"apply_patch","arguments":"{\"input\":42}"}]}`)
			var param any
			if tc.stream {
				frame, err := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", body)
				if err != nil {
					t.Fatal(err)
				}
				frames := registry.TranslateStream(ctx, FormatOpenAI, FormatOpenAIResponse, "fixture", []byte(patchRequest), nil, frame, &param)
				if len(frames) != 1 || !bytes.Contains(frames[0], []byte("response.failed")) {
					t.Fatalf("invalid patch became a successful stream: %s", bytes.Join(frames, nil))
				}
			} else {
				out := registry.TranslateNonStream(ctx, FormatOpenAI, FormatOpenAIResponse, "fixture", []byte(patchRequest), nil, body, &param)
				if gjson.GetBytes(out, "type").String() != "response.failed" {
					t.Fatalf("invalid patch became a successful response: %s", out)
				}
			}
			if ResponseTranslationError(&param) == nil || hooks.afterCalled {
				t.Fatalf("error=%v, response normalizer called=%v", ResponseTranslationError(&param), hooks.afterCalled)
			}
			hooks.recover = true
			hooks.translateCalled = false
			if tc.stream {
				if out := registry.TranslateStream(ctx, FormatOpenAI, FormatOpenAIResponse, "fixture", []byte(patchRequest), nil, []byte(`{}`), &param); len(out) != 0 {
					t.Fatalf("retained failure became a new stream: %s", bytes.Join(out, nil))
				}
			} else {
				out := registry.TranslateNonStream(ctx, FormatOpenAI, FormatOpenAIResponse, "fixture", []byte(patchRequest), nil, []byte(`{}`), &param)
				if gjson.GetBytes(out, "type").String() != "response.failed" {
					t.Fatalf("retained failure became a new response: %s", out)
				}
			}
			if hooks.translateCalled || ResponseTranslationError(&param) == nil {
				t.Fatalf("plugin translation called=%v, error=%v", hooks.translateCalled, ResponseTranslationError(&param))
			}
		})
	}
}

func TestResponsesPatchHooksReceiveOriginalRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		hooks := &patchOriginalHooks{}
		ctx := WithPluginHooks(context.Background(), hooks)
		request, err := sjson.SetRawBytes([]byte(patchRequest), "metadata", []byte(`{"sourceIndex":7,"authIndex":"offline-auth","opaque":{"marker":"retained"}}`))
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"id":"resp_hook","status":"completed","error":null,"output":[{"id":"fc_hook","type":"function_call","call_id":"call_hook","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}]}`)
		var param any
		registry := patchRegistry()
		expectedCalls := 3
		if stream {
			frame, err := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", body)
			if err != nil {
				t.Fatal(err)
			}
			frames := registry.TranslateStream(ctx, FormatOpenAI, FormatOpenAIResponse, "model", request, nil, frame, &param)
			expectedCalls = 2 + len(frames)
			if len(hooks.afterBodies) != len(frames) {
				t.Fatalf("normalized frames = %d, emitted frames = %d", len(hooks.afterBodies), len(frames))
			}
			for i, frame := range frames {
				if !bytes.Equal(hooks.afterBodies[i], frame) {
					t.Fatalf("response normalizer did not receive client frame %d", i)
				}
			}
		} else {
			out := registry.TranslateNonStream(ctx, FormatOpenAI, FormatOpenAIResponse, "model", request, nil, body, &param)
			if len(hooks.afterBodies) != 1 || !bytes.Equal(hooks.afterBodies[0], out) {
				t.Fatal("response normalizer did not receive the final client response")
			}
		}
		if err := ResponseTranslationError(&param); err != nil {
			t.Fatal(err)
		}
		if len(hooks.originals) != expectedCalls {
			t.Fatalf("hook calls = %d, want %d", len(hooks.originals), expectedCalls)
		}
		for _, original := range hooks.originals {
			if !bytes.Equal(original, request) {
				t.Fatalf("client request changed before hook: %s", original)
			}
		}
	}
}

func TestResponsesPatchEmptyNonStreamTranslation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
	} {
		for _, patch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/patch=%t", tc.name, patch), func(t *testing.T) {
				registry := NewRegistry()
				registry.Register(FormatOpenAIResponse, FormatOpenAI, nil, ResponseTransform{
					NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte {
						return tc.result
					},
				})
				original := []byte(`{"input":"ordinary","tools":[{"type":"function","name":"lookup"}]}`)
				if patch {
					original = []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)
				}
				var state any
				out := registry.TranslateNonStream(context.Background(), FormatOpenAI, FormatOpenAIResponse, "synthetic-model", original, nil, []byte(`{"choices":[]}`), &state)
				err := ResponseTranslationError(&state)
				if !patch {
					if err != nil || !bytes.Equal(out, tc.result) {
						t.Fatalf("ordinary empty translation changed: output=%s error=%v", out, err)
					}
					return
				}
				if tc.name == "nil" && out != nil {
					t.Fatalf("nil patch translation produced a payload: %s", out)
				}
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != http.StatusBadGateway {
					t.Fatalf("empty patch translation error=%v output=%s, want SDK 502", err, out)
				}
			})
		}
	}
}
