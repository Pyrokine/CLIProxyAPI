package executor

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func auditProtocolResponsesHistory(t *testing.T, payload []byte) []byte {
	t.Helper()
	var input []json.RawMessage
	for _, message := range gjson.GetBytes(payload, "messages").Array() {
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
	body, err := json.Marshal(map[string]any{"model": "gemini-3-flash", "input": input})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAuditProtocolReminderHistoryFinalWire(t *testing.T) {
	const call = `{"role":"assistant","tool_calls":[{"type":"function","id":"call-1","function":{"name":"` +
		`run_command","arguments":"{\"command\":\"echo test\"}"}}]}`
	const result = `{"role":"tool","tool_call_id":"call-1","content":"test"}`
	const reminder = `{"role":"developer","content":"<permissions instructions>\nApproved: ` +
		`echo\n</permissions ` +
		`instructions>"}`
	const wrapped = "<system-reminder>\n<permissions instructions>\nApproved: echo\n</" +
		"permissions instructions>\n</" +
		"system-reminder>"
	for _, sample := range []struct {
		name, messages string
		want           []string
	}{
		{
			"original directive and prior-turn prefix",
			`{"role":"user","content":"Turn 1 user"},{"role":"assistant","content":` +
				`"Turn 1 assistant"},{"role":"system","content":"Call tool now"},{"role` +
				`":"user","content":"Turn 2 user"}`,
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
			"original developer and intervening user",
			call + "," + reminder + `,{"role":"user","content":"Wait, also check this"},` + result,
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
				(`,{"role":"user","content":"Wait, also ` +
					`check this"},{"role":"tool",` +
					`"tool_call_id":"call-2","content":` +
					`"second"}`),
			[]string{
				"call:run_command", "call:run_second", "result:run_command:test", "result:run_second:second",
				"user:" + wrapped, "user:Wait, also check this",
			},
		},
		{
			"empty assistant between directive and result",
			call + "," + reminder + `,{"role":"assistant","content":""},` + result,
			[]string{"call:run_command", "result:run_command:test", "user:" + wrapped},
		},
		{
			"empty assistant keeps conversation boundary", `{"role":"assistant","content":""},` + reminder,
			[]string{"user:" + wrapped},
		},
		{
			"tool-only issuing prefix", call + "," + reminder + "," + result + `,{"role":"user","content":"Continue"}`,
			[]string{"call:run_command", "result:run_command:test", "user:" + wrapped, "user:Continue"},
		},
	} {
		for _, target := range []struct {
			name, provider, model, path string
			source                      translator.Format
		}{
			{"Gemini Chat", "gemini", "gemini-3-flash", "contents", translator.FormatOpenAI},
			{"Gemini Responses", "gemini", "gemini-3-flash", "contents", translator.FormatOpenAIResponse},
			{"Antigravity Gemini Chat", "antigravity", "gemini-3-flash", "request.contents", translator.FormatOpenAI},
			{
				"Antigravity Claude Chat", "antigravity", "claude-sonnet-4-6", "request.contents",
				translator.FormatOpenAI,
			},
		} {
			for _, stream := range []bool{false, true} {
				name := sample.name + "/" + target.name + map[bool]string{
					false: "/Execute", true: "/ExecuteStream",
				}[stream]
				t.Run(
					name, func(t *testing.T) {
						payload := []byte(`{"model":"gemini-3-flash","messages":[` + sample.messages + `]}`)
						if target.source == translator.FormatOpenAIResponse {
							payload = auditProtocolResponsesHistory(t, payload)
						}
						wire := auditProtocolFinalWire(
							t,
							target.provider,
							target.model,
							target.source,
							payload,
							stream,
						)
						var got []string
						for _, content := range gjson.GetBytes(wire, target.path).Array() {
							for _, part := range content.Get("parts").Array() {
								switch {
								case part.Get("functionCall").Exists():
									name := part.Get("functionCall.name").String()
									got = append(got, "call:"+name)
									want := `{"command":"echo test"}`
									if name == "run_second" {
										want = `{}`
									}
									if part.Get("functionCall.args").Raw != want {
										t.Fatalf(
											"argument bytes=%s want=%s",
											part.Get("functionCall.args").Raw,
											want,
										)
									}
								case part.Get("functionResponse").Exists():
									got = append(
										got,
										"result:"+part.Get("functionResponse.name").
											String()+
											":"+part.Get("functionResponse.response.result").
											String(),
									)
								case strings.TrimSpace(part.Get("text").String()) != "":
									got = append(
										got,
										content.Get("role").String()+":"+part.Get("text").String(),
									)
								}
							}
						}
						if !reflect.DeepEqual(got, sample.want) {
							t.Fatalf("final history=%q want=%q payload=%s", got, sample.want, wire)
						}
						prefix := ""
						if target.provider == "antigravity" {
							prefix = "request."
						}
						if gjson.GetBytes(wire, prefix+"systemInstruction").Exists() || gjson.GetBytes(
							wire, prefix+"generationConfig.temperature",
						).Float() != 0.25 {
							t.Fatalf("local directive or final payload rule changed: %s", wire)
						}
					},
				)
			}
		}
	}
}
