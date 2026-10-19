package responses

import (
	"context"
	"fmt"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiResponsesWaitsForSplitAndLateUsage(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, reason := range []string{"STOP", "MAX_TOKENS", "SAFETY"} {
			var state any
			frames := []string{
				`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":10}}`,
				`{"candidates":[{"finishReason":"` + reason + `"}],"usageMetadata":{"candidatesTokenCount":2}}`,
				`{"usageMetadata":{"cachedContentTokenCount":4,"thoughtsTokenCount":3,"totalTokenCount":15}}`,
			}
			for _, frame := range frames {
				if wrapped {
					frame = `{"response":` + frame + `}`
				}
				for _, chunk := range ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-fixture", nil, nil, []byte("data: "+frame), &state) {
					event, _ := parseSSEEvent(t, chunk)
					if event == "response.completed" || event == "response.incomplete" || event == "response.failed" {
						t.Fatalf("terminal before usage tail: %s", event)
					}
				}
			}
			var terminal gjson.Result
			count := 0
			wantEvent := "response.completed"
			if reason != "STOP" {
				wantEvent = "response.incomplete"
			}
			for _, chunk := range ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-fixture", nil, nil, []byte("data: [DONE]"), &state) {
				event, data := parseSSEEvent(t, chunk)
				if event == wantEvent {
					terminal = data
					count++
				}
			}
			if count != 1 || terminal.Get("response.usage.input_tokens").Int() != 10 || terminal.Get("response.usage.input_tokens_details.cached_tokens").Int() != 4 || terminal.Get("response.usage.output_tokens").Int() != 5 || terminal.Get("response.usage.output_tokens_details.reasoning_tokens").Int() != 3 || terminal.Get("response.usage.total_tokens").Int() != 15 {
				t.Fatalf("wrapped=%t reason=%s terminal count=%d body=%s", wrapped, reason, count, terminal.Raw)
			}
			if reason == "MAX_TOKENS" && terminal.Get("response.incomplete_details.reason").String() != "max_output_tokens" || reason == "SAFETY" && terminal.Get("response.incomplete_details.reason").String() != "content_filter" {
				t.Fatalf("terminal reason changed: %s", terminal.Raw)
			}
			if st := state.(*geminiToResponsesState); !st.Completed || st.PendingTerminal != nil {
				t.Fatalf("terminal state retained pending finish: %#v", st.PendingTerminal)
			}
			for _, extra := range []string{`{"usageMetadata":{"promptTokenCount":999}}`, `[DONE]`} {
				if out := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-fixture", nil, nil, []byte(extra), &state); len(out) != 0 {
					t.Fatalf("events after terminal: %q", out)
				}
			}
		}
	}
}

func TestGeminiResponsesErrorOverridesPendingFinish(t *testing.T) {
	var state any
	ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-fixture", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`), &state)
	out := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-fixture", nil, nil,
		[]byte(`{"error":{"status":"UNAVAILABLE","message":"fixture failure"}}`), &state)
	failed := 0
	for _, chunk := range out {
		event, data := parseSSEEvent(t, chunk)
		if event == "response.completed" {
			t.Fatal("upstream error emitted success")
		}
		if event == "response.failed" {
			failed++
			if data.Get("response.error.code").String() != "UNAVAILABLE" {
				t.Fatalf("error changed: %s", data.Raw)
			}
		}
	}
	if failed != 1 || state.(*geminiToResponsesState).PendingTerminal != nil {
		t.Fatalf("failed events=%d pending=%#v", failed, state.(*geminiToResponsesState).PendingTerminal)
	}
}

const issue6258Usage = `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":50,"totalTokenCount":160,"cachedContentTokenCount":7}`

func issue6258Translate(t *testing.T, param *any, frame string) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for _, chunk := range ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.7-flash", nil, nil, []byte(frame), param) {
		_, event := parseSSEEvent(t, chunk)
		events = append(events, event)
	}
	return events
}

func issue6258Terminal(t *testing.T, events []gjson.Result) gjson.Result {
	t.Helper()
	var terminal gjson.Result
	terminals, created, inProgress := 0, 0, 0
	previous := int64(-1)
	added := map[string]gjson.Result{}
	done := map[string]int{}
	responseID := ""
	for _, event := range events {
		seq := event.Get("sequence_number").Int()
		if seq <= previous {
			t.Errorf("sequence_number=%d after %d", seq, previous)
		}
		previous = seq
		if id := event.Get("response.id").String(); id != "" {
			if responseID != "" && id != responseID {
				t.Errorf("response ID changed: %q -> %q", responseID, id)
			}
			responseID = id
		}
		switch event.Get("type").String() {
		case "response.created":
			created++
		case "response.in_progress":
			inProgress++
		case "response.output_item.added":
			added[event.Get("item.id").String()] = event
		case "response.output_item.done":
			id := event.Get("item.id").String()
			done[id]++
			start, ok := added[id]
			if !ok || start.Get("output_index").Int() != event.Get("output_index").Int() {
				t.Errorf("item identity/index changed: %s", event.Raw)
			}
		case "response.completed", "response.incomplete", "response.failed":
			terminals++
			terminal = event
		}
	}
	if terminals != 1 {
		t.Errorf("terminal count=%d, want exactly 1", terminals)
	}
	if created != 1 || inProgress != 1 {
		t.Errorf("created/in_progress counts=%d/%d, want 1/1", created, inProgress)
	}
	for id := range added {
		if done[id] != 1 {
			t.Errorf("item %s done count=%d, want exactly 1", id, done[id])
		}
	}
	for index, item := range terminal.Get("response.output").Array() {
		start, ok := added[item.Get("id").String()]
		if !ok || start.Get("output_index").Int() != int64(index) {
			t.Errorf("terminal output identity/index changed: %s", item.Raw)
		}
	}
	return terminal.Get("response")
}

func issue6258AssertUsage(t *testing.T, response gjson.Result, input, output, reasoning, total, cached int64) {
	t.Helper()
	for path, want := range map[string]int64{
		"input_tokens": input, "output_tokens": output, "output_tokens_details.reasoning_tokens": reasoning,
		"total_tokens": total, "input_tokens_details.cached_tokens": cached,
	} {
		got := response.Get("usage." + path)
		if !got.Exists() || got.Int() != want {
			t.Errorf("usage.%s=%s, want %d; usage=%s", path, got.Raw, want, response.Get("usage").Raw)
		}
	}
}

func TestIssue6258GeminiResponsesSplitStopUsage(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, usageKey := range []string{"usageMetadata", "cpaUsageMetadata"} {
			t.Run(fmt.Sprintf("wrapped=%t/%s", wrapped, usageKey), func(t *testing.T) {
				frame := func(body string) string {
					if wrapped {
						return `data: {"response":` + body + `}`
					}
					return "data: " + body
				}
				var param any
				events := issue6258Translate(t, &param, frame(`{"responseId":"split-stop","candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`))
				finish := frame(`{"candidates":[{"content":{"parts":[{"text":" tail"}]},"finishReason":"STOP"}]}`)
				beforeUsage := issue6258Translate(t, &param, finish)
				for _, event := range beforeUsage {
					if event.Get("type").String() == "response.completed" {
						t.Error("STOP without usage emitted a terminal before the usage tail")
					}
				}
				events = append(events, beforeUsage...)
				repeatedFinish := issue6258Translate(t, &param, frame(`{"candidates":[{"finishReason":"STOP"}]}`))
				for _, event := range repeatedFinish {
					if event.Get("type").String() == "response.completed" {
						t.Error("repeated pending STOP emitted a terminal before usage")
					}
				}
				events = append(events, repeatedFinish...)
				afterUsage := issue6258Translate(t, &param, frame(`{"`+usageKey+`":`+issue6258Usage+`}`))
				tailTerminals := 0
				for _, event := range afterUsage {
					if event.Get("type").String() == "response.completed" {
						tailTerminals++
					}
				}
				if tailTerminals != 1 {
					t.Errorf("usage tail emitted %d terminals, want 1", tailTerminals)
				}
				events = append(events, afterUsage...)
				for _, repeated := range []string{finish, "data: [DONE]", "data: [DONE]"} {
					events = append(events, issue6258Translate(t, &param, repeated)...)
				}
				response := issue6258Terminal(t, events)
				if response.Get("status").String() != "completed" || response.Get("output.0.content.0.text").String() != "answer tail" {
					t.Errorf("lost finish-frame content or wrong status: %s", response.Raw)
				}
				issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
			})
		}
	}
}

func TestIssue6258GeminiResponsesMaxTokens(t *testing.T) {
	for _, mode := range []string{"same_frame", "split_usage", "no_usage_clean_done"} {
		t.Run("stream/"+mode, func(t *testing.T) {
			var param any
			events := issue6258Translate(t, &param, `{"responseId":"max","candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)
			finish := `{"candidates":[{"finishReason":"MAX_TOKENS"}]`
			if mode == "same_frame" {
				finish += `,"usageMetadata":` + issue6258Usage
			}
			events = append(events, issue6258Translate(t, &param, finish+`}`)...)
			if mode == "split_usage" {
				events = append(events, issue6258Translate(t, &param, `{"usageMetadata":`+issue6258Usage+`}`)...)
			}
			for _, frame := range []string{"[DONE]", "[DONE]", finish + `}`} {
				events = append(events, issue6258Translate(t, &param, frame)...)
			}
			response := issue6258Terminal(t, events)
			for _, event := range events {
				kind := event.Get("type").String()
				if kind == "response.completed" || kind == "response.failed" {
					t.Errorf("MAX_TOKENS emitted %s, want response.incomplete", kind)
				}
			}
			issue6258AssertIncomplete(t, response)
			if mode != "no_usage_clean_done" {
				issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
			}
		})
	}
	t.Run("nonstream", func(t *testing.T) {
		raw := `{"responseId":"max","candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":` + issue6258Usage + `}`
		response := gjson.ParseBytes(ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-3.7-flash", nil, nil, []byte(raw), nil))
		issue6258AssertIncomplete(t, response)
		issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
	})
}

func issue6258AssertIncomplete(t *testing.T, response gjson.Result) {
	t.Helper()
	if response.Get("status").String() != "incomplete" || response.Get("incomplete_details.reason").String() != "max_output_tokens" {
		t.Errorf("MAX_TOKENS status=%q details=%s, want incomplete/max_output_tokens", response.Get("status").String(), response.Get("incomplete_details").Raw)
	}
	if response.Get("output.0.status").String() != "incomplete" || response.Get("output.0.content.0.text").String() != "partial" {
		t.Errorf("partial message must be preserved and incomplete: %s", response.Get("output").Raw)
	}
}

func TestIssue6258GeminiResponsesUsageSnapshots(t *testing.T) {
	for _, test := range []struct {
		name, finishUsage        string
		output, reasoning, total int64
	}{
		{"absent_preserves", `{"candidatesTokenCount":12,"totalTokenCount":182}`, 62, 50, 182},
		{"explicit_zero_replaces", `{"candidatesTokenCount":12,"thoughtsTokenCount":0,"totalTokenCount":0}`, 12, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var param any
			var events []gjson.Result
			for _, frame := range []string{
				`{"responseId":"snapshots","candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":` + issue6258Usage + `}`,
				`{"usageMetadata":` + issue6258Usage + `}`,
				`{"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":0},"cpaUsageMetadata":{"promptTokenCount":999}}`,
				`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":` + test.finishUsage + `}`,
				`[DONE]`,
			} {
				events = append(events, issue6258Translate(t, &param, frame)...)
			}
			issue6258AssertUsage(t, issue6258Terminal(t, events), 120, test.output, test.reasoning, test.total, 0)
		})
	}
}

func TestIssue6258GeminiResponsesMaxTokensOnlyActiveMessageIncomplete(t *testing.T) {
	var param any
	var events []gjson.Result
	for _, frame := range []string{
		`{"responseId":"mixed","candidates":[{"content":{"parts":[{"text":"preface"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"key":"value"}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":` + issue6258Usage + `}`,
		`[DONE]`,
	} {
		events = append(events, issue6258Translate(t, &param, frame)...)
	}
	response := issue6258Terminal(t, events)
	items := response.Get("output").Array()
	if len(items) != 3 {
		t.Fatalf("output count=%d, want preface/tool/active message: %s", len(items), response.Raw)
	}
	for index, want := range []string{"completed", "completed", "incomplete"} {
		if got := items[index].Get("status").String(); got != want {
			t.Errorf("output[%d].status=%q, want %q", index, got, want)
		}
		for _, event := range events {
			if event.Get("type").String() == "response.output_item.done" && event.Get("output_index").Int() == int64(index) && event.Get("item.status").String() != want {
				t.Errorf("output_item.done[%d].status=%q, want %q", index, event.Get("item.status").String(), want)
			}
		}
	}
}

func TestIssue6258GeminiResponsesDoneUsesUsageSnapshot(t *testing.T) {
	for _, usageKey := range []string{"usageMetadata", "cpaUsageMetadata"} {
		t.Run(usageKey, func(t *testing.T) {
			var param any
			var events []gjson.Result
			for _, frame := range []string{
				`{"responseId":"snapshot-done","candidates":[{"content":{"parts":[{"text":"answer"}]}}],"` + usageKey + `":` + issue6258Usage + `}`,
				`{"` + usageKey + `":` + issue6258Usage + `}`,
				`{"candidates":[{"finishReason":"STOP"}]}`,
				`[DONE]`, `[DONE]`,
			} {
				events = append(events, issue6258Translate(t, &param, frame)...)
			}
			issue6258AssertUsage(t, issue6258Terminal(t, events), 100, 60, 50, 160, 7)
		})
	}
}

func TestIssue6258GeminiResponsesSameFrameControl(t *testing.T) {
	var param any
	events := issue6258Translate(t, &param, `{"responseId":"control","candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":`+issue6258Usage+`}`)
	events = append(events, issue6258Translate(t, &param, "[DONE]")...)
	response := issue6258Terminal(t, events)
	issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
}

func TestGeminiResponsesUsageTailPreservesFinishAndContent(t *testing.T) {
	for _, reason := range []string{"STOP", "MAX_TOKENS", "SAFETY"} {
		t.Run(reason, func(t *testing.T) {
			var state any
			events := issue6258Translate(t, &state, `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"`+reason+`"}]}`)
			events = append(events, issue6258Translate(t, &state, `{"candidates":[{"content":{"parts":[{"text":" duplicate"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":`+issue6258Usage+`}`)...)
			response := issue6258Terminal(t, events)
			wantStatus := "completed"
			if reason != "STOP" {
				wantStatus = "incomplete"
			}
			if response.Get("status").String() != wantStatus || response.Get("output.0.status").String() != wantStatus || response.Get("output.0.content.0.text").String() != "answer" {
				t.Fatalf("用量尾帧改变了终态或正文：%s", response.Raw)
			}
			if reason == "SAFETY" && response.Get("incomplete_details.reason").String() != "content_filter" {
				t.Fatalf("原始拒绝原因丢失：%s", response.Raw)
			}
			issue6258AssertUsage(t, response, 100, 60, 50, 160, 7)
			if !state.(*geminiToResponsesState).Completed || state.(*geminiToResponsesState).PendingTerminal != nil {
				t.Fatal("完整用量尾帧未清除等待状态")
			}
			if extra := issue6258Translate(t, &state, "[DONE]"); len(extra) != 0 {
				t.Fatal("终态后重复发送事件")
			}
		})
	}
}

func TestGeminiResponsesRegisteredFunctionSnapshots(t *testing.T) {
	for _, identity := range []string{`"id":"call-p",`, `"call_id":"call-p",`, `"callId":"call-p",`, ""} {
		for _, custom := range []bool{false, true} {
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("identity=%s/custom=%t/wrapped=%t", identity, custom, wrapped), func(t *testing.T) {
					kind := "function"
					if custom {
						kind = "custom"
					}
					original := []byte(`{"model":"gemini-fixture","tools":[{"type":"` + kind + `","name":"apply_patch"}],"input":[]}`)
					request, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, "gemini-fixture", original, true)
					if err != nil {
						t.Fatal(err)
					}
					var state any
					var events []gjson.Result
					first := `{"candidates":[{"content":{"parts":[{"functionCall":{` + identity + `"name":"apply_patch","args":{"input":"patch"}}}]}}]}`
					second := `{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"input":"patch"},` + identity + `"name":"apply_patch"}}]}}]}`
					for _, frame := range []string{first, second, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":` + issue6258Usage + `}`, `[DONE]`, first} {
						if wrapped && frame != "[DONE]" {
							frame = `{"response":` + frame + `}`
						}
						for _, chunk := range sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse, "gemini-fixture", original, request, []byte(frame), &state) {
							_, event := parseSSEEvent(t, chunk)
							events = append(events, event)
						}
						if err := sdktranslator.ResponseTranslationError(&state); err != nil {
							t.Fatal(err)
						}
					}
					response := issue6258Terminal(t, events)
					want := 1
					if identity == "" {
						want = 2
					}
					items := response.Get("output").Array()
					if len(items) != want || response.Get("status").String() != "completed" {
						t.Fatalf("expected %d calls: %s", want, response.Raw)
					}
					ids := make(map[string]bool)
					for _, item := range items {
						id := item.Get("call_id").String()
						if id == "" || ids[id] || identity != "" && id != "call-p" {
							t.Fatalf("incorrect call identity: %s", item.Raw)
						}
						ids[id] = true
						if custom && (item.Get("type").String() != "custom_tool_call" || item.Get("input").String() != "patch") {
							t.Fatalf("incorrect custom input: %s", item.Raw)
						}
						if !custom && item.Get("type").String() != "function_call" {
							t.Fatalf("ordinary call changed type: %s", item.Raw)
						}
					}
				})
			}
		}
	}
}

func TestGeminiResponsesRegisteredSnapshotConflicts(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  string
		second string
	}{
		{"arguments", `{"input":"patch"}`, `{"input":"changed"}`},
		{"large integers", `{"n":9007199254740992}`, `{"n":9007199254740993}`},
		{"duplicate key", `{"input":"patch"}`, `{"input":"wrong","input":"patch"}`},
		{"invalid surrogate", `{"input":"�"}`, `{"input":"\ud800"}`},
	} {
		for _, custom := range []bool{false, true} {
			if custom && test.name == "large integers" {
				continue
			}
			t.Run(fmt.Sprintf("%s/custom=%t", test.name, custom), func(t *testing.T) {
				kind := "function"
				if custom {
					kind = "custom"
				}
				original := []byte(`{"model":"gemini-fixture","tools":[{"type":"` + kind + `","name":"apply_patch"}],"input":[]}`)
				request, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, "gemini-fixture", original, true)
				if err != nil {
					t.Fatal(err)
				}
				var state any
				for _, args := range []string{test.first, test.second} {
					frame := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-p","name":"apply_patch","args":` + args + `}}]}}]}`)
					out := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse, "gemini-fixture", original, request, frame, &state)
					if args == test.first {
						if err := sdktranslator.ResponseTranslationError(&state); err != nil {
							t.Fatal(err)
						}
						continue
					}
					err := sdktranslator.ResponseTranslationError(&state)
					status, ok := err.(interface{ StatusCode() int })
					if err == nil || !ok || status.StatusCode() != 502 {
						t.Fatalf("missing SDK failure: %v", err)
					}
					failed := 0
					for _, chunk := range out {
						event, _ := parseSSEEvent(t, chunk)
						if event == "response.failed" {
							failed++
						}
						if event == "response.completed" || event == "response.output_item.added" {
							t.Fatalf("conflict emitted success or another call: %s", chunk)
						}
					}
					if failed != 1 {
						t.Fatalf("failed terminal count=%d", failed)
					}
				}
			})
		}
	}
}

func TestGeminiResponsesSnapshotPreservesSignatureAndPartCursor(t *testing.T) {
	var state any
	part := `{"partIndex":3,"thoughtSignature":"` + testResponsesGeminiThoughtSignature + `","functionCall":{"name":"lookup","args":{"input":"patch"}}}`
	frame := `{"candidates":[{"content":{"parts":[` + part + `]}}]}`
	events := issue6258Translate(t, &state, frame)
	st := state.(*geminiToResponsesState)
	index := st.NextIndex
	if duplicate := issue6258Translate(t, &state, frame); len(duplicate) != 0 || st.NextIndex != index || st.CurrentPartIndex != 3 {
		t.Fatalf("duplicate changed signature/output/part state: %q", duplicate)
	}
	events = append(events, issue6258Translate(t, &state, `{"candidates":[{"content":{"parts":[{"text":"answer"}]} }]}`)...)
	if st.CurrentPartIndex != 4 {
		t.Fatalf("text part index=%d, want 4", st.CurrentPartIndex)
	}
	events = append(events, issue6258Translate(t, &state, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":`+issue6258Usage+`}`)...)
	response := issue6258Terminal(t, events)
	if response.Get("output.#").Int() != 3 || st.FuncSnapshots != nil {
		t.Fatalf("incorrect final output or retained snapshots: %s", response.Raw)
	}
	replay := []byte(`{"model":"gemini-fixture","tools":[{"type":"function","name":"lookup"}],"input":` + response.Get("output").Raw + `}`)
	translated, err := ConvertOpenAIResponsesRequestToGeminiChecked("gemini-fixture", replay, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, content := range gjson.GetBytes(translated, "contents").Array() {
		for _, item := range content.Get("parts").Array() {
			if item.Get("functionCall").Exists() {
				calls++
				if item.Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || item.Get("functionCall.args.input").String() != "patch" {
					t.Fatalf("signature or input changed on replay: %s", item.Raw)
				}
			}
		}
	}
	if calls != 1 {
		t.Fatalf("replayed function count=%d, want 1", calls)
	}
}

func TestGeminiResponsesRegisteredNonStreamFunctionSnapshots(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			for _, identity := range []string{`"id":"call-p",`, ""} {
				t.Run(fmt.Sprintf("custom=%t/wrapped=%t/identity=%s", custom, wrapped, identity), func(t *testing.T) {
					kind := "function"
					if custom {
						kind = "custom"
					}
					ctx := context.Background()
					original := []byte(`{"model":"gemini-fixture","tools":[{"type":"` + kind + `","name":"apply_patch"}],"input":[]}`)
					request, err := sdktranslator.TranslateRequestChecked(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, "gemini-fixture", original, false)
					if err != nil {
						t.Fatal(err)
					}
					part := `{"functionCall":{` + identity + `"name":"apply_patch","args":{"input":"patch"}}}`
					body := `{"candidates":[{"content":{"parts":[` + part + `,` + part + `]},"finishReason":"STOP"}],"usageMetadata":` + issue6258Usage + `}`
					if wrapped {
						body = `{"response":` + body + `}`
					}
					var state any
					translated := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse, "gemini-fixture", original, request, []byte(body), &state)
					if err := sdktranslator.ResponseTranslationError(&state); err != nil {
						t.Fatal(err)
					}
					want := int64(1)
					if identity == "" {
						want = 2
					}
					if gjson.GetBytes(translated, "status").String() != "completed" || gjson.GetBytes(translated, "output.#").Int() != want {
						t.Fatalf("expected %d calls: %s", want, translated)
					}
					for _, item := range gjson.GetBytes(translated, "output").Array() {
						if custom && item.Get("input").String() != "patch" {
							t.Fatalf("custom input changed: %s", item.Raw)
						}
					}
					issue6258AssertUsage(t, gjson.ParseBytes(translated), 100, 60, 50, 160, 7)
				})
			}
		}
	}
}

func TestGeminiResponsesRegisteredSnapshotIdentityConflicts(t *testing.T) {
	first := `{"partIndex":0,"functionCall":{"id":"call-p","name":"apply_patch","args":{"input":"patch"}}}`
	for _, second := range []string{
		`{"partIndex":0,"functionCall":{"id":"call-p","name":"other","args":{"input":"patch"}}}`,
		`{"partIndex":0,"functionCall":{"id":"other","name":"apply_patch","args":{"input":"patch"}}}`,
		`{"partIndex":1,"functionCall":{"id":"call-p","name":"apply_patch","args":{"input":"patch"}}}`,
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, second), func(t *testing.T) {
				ctx := context.Background()
				original := []byte(`{"model":"gemini-fixture","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"other"}],"input":[]}`)
				request, err := sdktranslator.TranslateRequestChecked(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, "gemini-fixture", original, stream)
				if err != nil {
					t.Fatal(err)
				}
				var state any
				if stream {
					for _, part := range []string{first, second} {
						sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse, "gemini-fixture", original, request, []byte(`{"candidates":[{"content":{"parts":[`+part+`]}}]}`), &state)
					}
				} else {
					sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse, "gemini-fixture", original, request, []byte(`{"candidates":[{"content":{"parts":[`+first+`,`+second+`]},"finishReason":"STOP"}]}`), &state)
				}
				err = sdktranslator.ResponseTranslationError(&state)
				status, ok := err.(interface{ StatusCode() int })
				if err == nil || !ok || status.StatusCode() != 502 {
					t.Fatalf("identity conflict was not propagated: %v", err)
				}
			})
		}
	}
}

func TestGeminiResponsesUsageMetadataPrecedence(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("wrapped=%t/stream=%t", wrapped, stream), func(t *testing.T) {
				body := `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5},"cpaUsageMetadata":` + issue6258Usage + `}`
				if wrapped {
					body = `{"response":` + body + `}`
				}
				var response gjson.Result
				if stream {
					var state any
					response = issue6258Terminal(t, issue6258Translate(t, &state, body))
				} else {
					response = gjson.ParseBytes(ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-fixture", nil, nil, []byte(body), nil))
				}
				issue6258AssertUsage(t, response, 2, 3, 0, 5, 0)
			})
		}
	}
}

func TestGeminiResponsesRejectsInvalidUsageTailCompletion(t *testing.T) {
	for _, usage := range []string{
		`{"promptTokenCount":10,"candidatesTokenCount":2}`,
		`{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":99}`,
		`{"promptTokenCount":-1,"candidatesTokenCount":2,"totalTokenCount":1}`,
		`{"promptTokenCount":1.5,"candidatesTokenCount":2,"totalTokenCount":3.5}`,
		`{"promptTokenCount":9223372036854775808,"candidatesTokenCount":2,"totalTokenCount":9223372036854775810}`,
	} {
		t.Run(usage, func(t *testing.T) {
			var state any
			events := issue6258Translate(t, &state, `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`)
			tail := issue6258Translate(t, &state, `{"usageMetadata":`+usage+`}`)
			if len(tail) != 0 || state.(*geminiToResponsesState).PendingTerminal == nil || state.(*geminiToResponsesState).Completed {
				t.Fatal("无效或不完整用量提前触发终态")
			}
			events = append(events, issue6258Translate(t, &state, "[DONE]")...)
			if response := issue6258Terminal(t, events); response.Get("status").String() != "completed" {
				t.Fatalf("显式结束信号未完成响应：%s", response.Raw)
			}
		})
	}
}
