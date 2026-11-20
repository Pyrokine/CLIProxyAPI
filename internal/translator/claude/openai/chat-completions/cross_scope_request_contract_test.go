package chat_completions_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/claude/openai/chat-completions"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiPairingAllowsUserReminderBeforeResponse(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, role := range []string{"user", "model"} {
			body := []byte(fmt.Sprintf(
				`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-a","name":"lookup","args":`+
					`{}}}]},{"role":%q,"parts":[{"text":"reminder"}]},{"role":"user","parts":[{"`+
					`functionResponse":{"id":"call-a","name":"lookup","response":{"result":"done"}}}]}]}`,
				role,
			))
			if wrapped {
				body = []byte(`{"request":` + string(body) + `}`)
			}
			err := signature.ValidateGeminiFunctionCallPairing(body)
			if role == "user" && err != nil || role == "model" && err == nil {
				t.Fatalf("wrapped=%v role=%s pairing error=%v", wrapped, role, err)
			}
		}
	}
}

func TestClaudeChatSDKRejectsUnpairedToolResults(t *testing.T) {
	call := `{"role":"assistant","tool_calls":[{"id":"call.a","type":"function",` +
		`"function":{"name":"lookup","arguments":` +
		`"{}"}}]}`
	for _, test := range []struct {
		name, messages string
		valid          bool
	}{
		{name: "orphan", messages: `{"role":"tool","tool_call_id":"orphan","content":"result"}`},
		{name: "missing call id", messages: `{"role":"tool","content":"result"}`},
		{name: "different call", messages: call + `,{"role":"tool","tool_call_id":"different","content":"result"}`},
		{name: "sanitized collision", messages: call + `,{"role":"tool","tool_call_id":"call_a","content":"result"}`},
		{
			name: "previous assistant turn",
			messages: call + (`,{"role":"user","content":"another user turn"},{"role":"assistant","content":"` +
				`another turn"},{"role":"tool","tool_call_id":"call.a","content":"result"}`),
		},
		{name: "paired", messages: call + `,{"role":"tool","tool_call_id":"call.a","content":"result"}`, valid: true},
		{
			name: "adjacent assistant continuation",
			messages: call +
				(`,{"role":"assistant","content":"continuation"},{"role":"tool",` +
					`"tool_call_id":"call.a","content":` +
					`"result"}`),
			valid: true,
		},
		{
			name: "paired through reminder",
			messages: call + (`,{"role":"user","content":"<system-reminder>\nkeep reminder\n</system-` +
				`reminder>"},{"role":"tool","tool_call_id":"call.a","content":"result"}`),
			valid: true,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				body := []byte(`{"messages":[` + test.messages + `]}`)
				for _, stream := range []bool{false, true} {
					out, err := sdk.TranslateRequestChecked(
						t.Context(), sdk.FormatOpenAI, sdk.FormatClaude, "claude-test", body, stream,
					)
					if !test.valid {
						if err == nil || len(out) != 0 {
							t.Fatalf("unpaired tool result accepted: body=%s error=%v", out, err)
						}
						if status, ok := err.(interface {
							StatusCode() int
							IsRequestScoped() bool
						}); !ok || status.StatusCode() != 400 || !status.IsRequestScoped() {
							t.Fatalf("tool pairing error is not request-scoped HTTP 400: %v", err)
						}
						if legacy := sdk.TranslateRequest(
							sdk.FormatOpenAI, sdk.FormatClaude, "claude-test", body, stream,
						); len(legacy) != 0 {
							t.Fatalf("byte entry accepted unpaired tool result: %s", legacy)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					results, reminders := 0, 0
					for _, message := range gjson.GetBytes(out, "messages").Array() {
						for _, part := range message.Get("content").Array() {
							if part.Get("text").
								String() ==
								"<system-reminder>\nkeep reminder\n</system-reminder>" {
								reminders++
							}
							if part.Get("type").String() == "tool_result" {
								results++
								if part.Get("tool_use_id").String() != "call_a" ||
									part.Get("content").String() != "result" {
									t.Fatalf("paired tool result changed: %s", out)
								}
							}
						}
					}
					if results != 1 {
						t.Fatalf("paired tool result count=%d, want one: %s", results, out)
					}
					if test.name == "paired through reminder" && reminders != 1 {
						t.Fatalf("reminder was discarded: %s", out)
					}
				}
			},
		)
	}
}

func TestClaudeChatSDKAllowedToolsAcceptMappedNames(t *testing.T) {
	for _, mode := range []string{"auto", "required"} {
		for _, name := range []string{"mcp.server:get_time", "mcp_server_get_time"} {
			body := []byte(fmt.Sprintf(
				`{"messages":[{"role":"user","content":"Use the tool"}],"tools":[{"type`+
					`":"function","function":{"name":"mcp.server:get_time","parameters":{"t`+
					`ype":"object"}}}],"tool_choice":{"type":"allowed_tools","allowed_tools`+
					`":{"mode":%q,"tools":[{"type":"function","function":{"name":%q}}]}}}`,
				mode, name,
			))
			out, err := sdk.TranslateRequestChecked(
				context.Background(), sdk.FormatOpenAI, sdk.FormatClaude, "claude-test", body, false,
			)
			want := "auto"
			if mode == "required" {
				want = "any"
			}
			if err != nil || gjson.GetBytes(out, "tools.#").Int() != 1 || gjson.GetBytes(
				out, "tools.0.name",
			).String() != "mcp_server_get_time" || gjson.GetBytes(out, "tool_choice.type").String() != want {
				t.Fatalf("mode=%s name=%s error=%v body=%s", mode, name, err, out)
			}
		}
	}
}
