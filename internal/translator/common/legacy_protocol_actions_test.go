package common_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestLegacyStrictBooleanPrecedenceThroughSDK(t *testing.T) {
	for _, tc := range []struct {
		name, nested, outer, wantMode, wantStrict string
	}{
		{"nested false overrides outer true", `,"strict":false`, `,"strict":true`, "AUTO", "false"},
		{"nested true overrides outer false", `,"strict":true`, `,"strict":false`, "VALIDATED", "true"},
		{"outer true when nested absent", "", `,"strict":true`, "VALIDATED", "true"},
		{"nested false", `,"strict":false`, "", "AUTO", "false"},
		{"nested true", `,"strict":true`, "", "VALIDATED", "true"},
		{"both false", `,"strict":false`, `,"strict":false`, "AUTO", "false"},
		{"both absent", "", "", "AUTO", ""},
		{"nested null is not absent", `,"strict":null`, `,"strict":true`, "AUTO", ""},
		{"nested string is not boolean", `,"strict":"false"`, `,"strict":true`, "AUTO", ""},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				body := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"` +
					tc.outer + `,"function":{"name":"tool_a","parameters":{"type":"object","properties":{}}` +
					tc.nested + `}}],"tool_choice":"auto"}`)
				for _, target := range []sdk.Format{sdk.FormatGemini, sdk.FormatClaude} {
					ctx := sdk.WithPluginHooks(t.Context(), nil)
					out, err := sdk.TranslateRequestChecked(
						ctx, sdk.FormatOpenAI, target, "selected-model", body, false,
					)
					if err != nil {
						t.Fatal(err)
					}
					if target == sdk.FormatGemini {
						if got := gjson.GetBytes(
							out, "toolConfig.functionCallingConfig.mode",
						).String(); got != tc.wantMode {
							t.Errorf("Gemini strict mode = %q, want %q; %s", got, tc.wantMode, out)
						}
						if gjson.GetBytes(out, "tools.0.functionDeclarations.0.strict").Exists() {
							t.Errorf("Gemini declaration contains unsupported strict: %s", out)
						}
					} else if got := gjson.GetBytes(out, "tools.0.strict").Raw; got != tc.wantStrict {
						t.Errorf("Claude strict = %q, want %q; %s", got, tc.wantStrict, out)
					}
				}
			},
		)
	}
}

func TestLegacyToolOnlyResultPrefixKeepsDirectiveLocal(t *testing.T) {
	for _, entry := range []struct {
		from, to sdk.Format
		path     string
	}{
		{sdk.FormatOpenAI, sdk.FormatGemini, "contents"},
		{sdk.FormatOpenAI, sdk.FormatAntigravity, "request.contents"},
		{sdk.FormatOpenAIResponse, sdk.FormatGemini, "contents"},
	} {
		body := []byte(`{"messages":[{"role":"tool","tool_call_id":"orphan","content":"orphan-` +
			`result"},{"role":"developer","content":"keep-local-directive"}]}`)
		if entry.from == sdk.FormatOpenAIResponse {
			body = []byte((`{"input":[{"type":"function_call_output","call_id":"orphan","output":"orphan-` +
				`result"},{"type":"message","role":"developer","content":"keep-local-directive"}]}`))
		}
		ctx := sdk.WithPluginHooks(t.Context(), nil)
		out, err := sdk.TranslateRequestChecked(ctx, entry.from, entry.to, "gemini-3-flash", body, false)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, content := range gjson.GetBytes(out, entry.path).Array() {
			for _, part := range content.Get("parts").Array() {
				texts = append(texts, part.Get("text").String())
			}
		}
		joined := strings.Join(texts, "\n")
		resultIndex := strings.Index(joined, "orphan-result")
		reminderIndex := strings.Index(joined, "<system-reminder>\nkeep-local-directive\n</system-reminder>")
		if resultIndex < 0 || reminderIndex <= resultIndex ||
			gjson.GetBytes(out, "systemInstruction").Exists() || gjson.GetBytes(
			out, "request.systemInstruction",
		).Exists() {
			t.Errorf("%s -> %s lost the tool-only prefix or directive boundary: %s", entry.from, entry.to, out)
		}
	}
}

func TestLegacyReminderHistoryThroughThreeSDKEntries(t *testing.T) {
	const call = `{"role":"assistant","tool_calls":[{"type":"function","id":"call-1","function":{"name":"` +
		`run_command","arguments":"{\"command\":\"echo test\"}"}}]}`
	const result = `{"role":"tool","tool_call_id":"call-1","content":"test"}`
	const reminder = `{"role":"developer","content":"<permissions instructions>\nApproved: ` +
		`echo\n</permissions ` +
		`instructions>"}`
	const wrapped = "<system-reminder>\n<permissions instructions>\nApproved: echo\n</" +
		"permissions instructions>\n</" +
		"system-reminder>"
	for _, tc := range []struct {
		name, messages string
		want           []string
	}{
		{
			"original directive and prior-turn prefix",
			`{"role":"user","content":"Turn 1 user"},{"role":"assistant","content":"Turn 1 assistant"},` +
				`{"role":"system","content":"Call tool now"},{"role":"user","content":"Turn 2 user"}`,
			[]string{
				"user:Turn 1 user", "model:Turn 1 assistant",
				"user:<system-reminder>\nCall tool now\n</system-reminder>", "user:Turn 2 user",
			},
		},
		{
			"original paired counterexample", call + "," + reminder + "," + result,
			[]string{"call:run_command", "result:run_command:test", "user:" + wrapped},
		},
		{
			"original developer and intervening user", call + "," + reminder +
				`,{"role":"user","content":"Wait, also check this"},` + result,
			[]string{"call:run_command", "result:run_command:test", "user:" + wrapped, "user:Wait, also check this"},
		},
		{
			"parallel results keep the deferred directive and user together",
			(`{"role":"assistant","tool_calls":[{"type":"function","id":"call-1","function":{"name":"` +
				`run_command","arguments":"{\"command\":\"echo test\"}"}},{"type":"function","id":"call-2","` +
				`function":{"name":"run_second","arguments":"{}"}}]},`) +
				result +
				"," +
				reminder +
				(`,{"role":"user","content":"Wait, also check this"},{"role":"tool",` +
					`"tool_call_id":"call-2","content":` +
					`"second"}`),
			[]string{
				"call:run_command", "call:run_second", "result:run_command:test", "result:run_second:second",
				"user:" + wrapped, "user:Wait, also check this",
			},
		},
		{
			"empty assistant between directive and result", call + "," + reminder +
				`,{"role":"assistant","content":""},` + result,
			[]string{"call:run_command", "result:run_command:test", "user:" + wrapped},
		},
		{
			"empty assistant keeps conversation boundary", `{"role":"assistant","content":""},` + reminder,
			[]string{"user:" + wrapped},
		},
		{
			"tool-only issuing prefix", call + "," + reminder + "," + result +
				`,{"role":"user","content":"Continue"}`,
			[]string{"call:run_command", "result:run_command:test", "user:" + wrapped, "user:Continue"},
		},
	} {
		for _, entry := range []struct {
			name         string
			from, to     sdk.Format
			contentsPath string
		}{
			{"Gemini Chat", sdk.FormatOpenAI, sdk.FormatGemini, "contents"},
			{"Antigravity Chat", sdk.FormatOpenAI, sdk.FormatAntigravity, "request.contents"},
			{"Gemini Responses", sdk.FormatOpenAIResponse, sdk.FormatGemini, "contents"},
		} {
			t.Run(
				tc.name+"/"+entry.name, func(t *testing.T) {
					body := []byte(`{"model":"gemini-3-flash","messages":[` + tc.messages + `]}`)
					if entry.from == sdk.FormatOpenAIResponse {
						var input []json.RawMessage
						for _, message := range gjson.GetBytes(body, "messages").Array() {
							item := message.Raw
							switch message.Get("role").String() {
							case "assistant":
								if calls := message.Get("tool_calls"); calls.IsArray() {
									for _, call := range calls.Array() {
										encoded, err := json.Marshal(
											map[string]string{
												"type": "function_call", "call_id": call.Get("id").String(),
												"name":      call.Get("function.name").String(),
												"arguments": call.Get("function.arguments").String(),
											},
										)
										if err != nil {
											t.Fatal(err)
										}
										input = append(input, encoded)
									}
									continue
								}
							case "tool":
								encoded, err := json.Marshal(
									map[string]string{
										"type": "function_call_output", "call_id": message.Get("tool_call_id").String(),
										"output": message.Get("content").String(),
									},
								)
								if err != nil {
									t.Fatal(err)
								}
								item = string(encoded)
							}
							input = append(input, json.RawMessage(item))
						}
						var err error
						body, err = json.Marshal(map[string]any{"model": "gemini-3-flash", "input": input})
						if err != nil {
							t.Fatal(err)
						}
					}
					ctx := sdk.WithPluginHooks(t.Context(), nil)
					out, err := sdk.TranslateRequestChecked(
						ctx,
						entry.from,
						entry.to,
						"gemini-3-flash",
						body,
						false,
					)
					if err != nil {
						t.Fatal(err)
					}
					var got []string
					for _, content := range gjson.GetBytes(out, entry.contentsPath).Array() {
						for _, part := range content.Get("parts").Array() {
							switch {
							case part.Get("functionCall").Exists():
								got = append(got, "call:"+part.Get("functionCall.name").String())
							case part.Get("functionResponse").Exists():
								got = append(
									got, "result:"+part.Get("functionResponse.name").String()+":"+
										part.Get("functionResponse.response.result").String(),
								)
							case strings.TrimSpace(part.Get("text").String()) != "":
								got = append(got, content.Get("role").String()+":"+part.Get("text").String())
							default:
								t.Errorf("empty or unexpected history part: %s", part.Raw)
							}
						}
					}
					if !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("history = %q, want %q; %s", got, tc.want, out)
					}
					if gjson.GetBytes(out, "systemInstruction").Exists() ||
						gjson.GetBytes(out, "request.systemInstruction").Exists() {
						t.Errorf("mid-session directive became global: %s", out)
					}
				},
			)
		}
	}
}
