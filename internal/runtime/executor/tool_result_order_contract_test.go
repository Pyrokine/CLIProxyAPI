package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOriginalToolResultOrderReachesFinalOpenAICompatWire(t *testing.T) {
	const calls = `{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},{"type":"` +
		`function_call","call_id":"call_b","name":"tool_b","arguments":"{}"}`
	for _, tc := range []struct {
		name, body, roles, results string
		format                     sdk.Format
	}{
		{
			"Claude parallel results",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"Run analysis"}]},{"role":"` +
				`assistant","content":[{"type":"tool_use","id":"call_1","name":"fetch_data","input":{"id":` +
				`123}},{"type":"tool_use","id":"call_2","name":"calc_metric","input":{"scale":1.5}}]},{"role"` +
				`:"assistant","content":[{"type":"text","text":"Waiting for results to continue"},{"type":"` +
				`thinking","thinking":"Thinking about next steps","signature":"sig_abc"}]},{"role":"user","` +
				`content":[{"type":"text","text":"Reminder: keep timeout short"}]},{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"call_2","content":"metric_ok"},{"type":"tool_result","` +
				`tool_use_id":"call_1","content":"data_ok"}]}]}`,
			"user,assistant,tool,tool,assistant,user", "call_2,call_1", sdk.FormatClaude,
		},
		{
			"Claude orphan and incomplete",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"Start"}]},{"role":"assistant","` +
				`content":[{"type":"tool_use","id":"call_alpha","name":"do_a","input":{}},{"type":"tool_use",` +
				`"id":"call_beta","name":"do_b","input":{}}]},{"role":"user","content":[{"type":"text","text"` +
				`:"Waiting on results"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"` +
				`call_unmatched","content":"orphan_result"}]}]}`,
			"user,assistant,user,tool", "call_unmatched", sdk.FormatClaude,
		},
		{
			"Responses complete",
			`{"input":[` + calls + (`,{"role":"user","content":"reminder during execution"},{"type":"` +
				`function_call_output","call_id":"call_b","output":"result_b"},{"type":"function_call_output"` +
				`,"call_id":"call_a","output":"result_a"}]}`),
			"assistant,tool,tool,user", "call_b,call_a", sdk.FormatOpenAIResponse,
		},
		{
			"Responses incomplete",
			`{"input":[` + calls + (`,{"role":"user","content":"reminder before results"},{"type":"function` +
				`_call_output","call_id":"call_a","output":"result_a"}]}`),
			"assistant,user,tool", "call_a", sdk.FormatOpenAIResponse,
		},
		{
			"Responses mixed empty ID",
			`{"input":[{"type":"function_call","call_id":"","name":"unknown","arguments":"{}"},{"type":"` +
				`function_call","call_id":"a","name":"known","arguments":"{}"},{"role":"user","content":"` +
				`reminder"},{"type":"function_call_output","call_id":"a","output":"ok"}]}`,
			"assistant,user,tool", "a", sdk.FormatOpenAIResponse,
		},
		{
			"Responses duplicate call",
			`{"input":[{"type":"function_call","call_id":"dup","name":"tool_1","arguments":"{}"},{"type":"` +
				`function_call","call_id":"dup","name":"tool_2","arguments":"{}"},{"role":"user","content":"` +
				`reminder"},{"type":"function_call_output","call_id":"dup","output":"ok"}]}`,
			"assistant,user,tool", "dup", sdk.FormatOpenAIResponse,
		},
		{
			"Responses duplicate output",
			`{"input":[{"type":"function_call","call_id":"call_dup_out","name":"tool_a","arguments":"{}"},{"` +
				`role":"user","content":"reminder before results"},{"type":"function_call_output","call_id":"` +
				`call_dup_out","output":"first"},{"type":"function_call_output","call_id":"call_dup_out","` +
				`output":"second"}]}`,
			"assistant,user,tool,user", "call_dup_out", sdk.FormatOpenAIResponse,
		},
		{
			"Responses duplicate custom output",
			`{"input":[{"type":"custom_tool_call","call_id":"custom_dup","name":"custom_a","input":"{}"},{"` +
				`role":"user","content":"reminder before custom results"},{"type":"custom_tool_call_output","` +
				`call_id":"custom_dup","output":"output 1"},{"type":"custom_tool_call_output","call_id":"` +
				`custom_dup","output":"output 2"}]}`,
			"assistant,user,tool,user", "custom_dup", sdk.FormatOpenAIResponse,
		},
		{
			"Responses missing output IDs",
			`{"input":[` + calls + (`,{"role":"user","content":"reminder before results"},{"type":"function` +
				`_call_output","output":"output X"},{"type":"function_call_output","out` +
				`put":"output Y"}]}`),
			"assistant,user,user,user", "", sdk.FormatOpenAIResponse,
		},
		{
			"Responses missing and orphan output IDs",
			`{"input":[` + calls + (`,{"role":"user","content":"reminder before results"},{"type":"function` +
				`_call_output","output":"output X"},{"type":"function_call_output","out` +
				`put":"output Y"},{"type":"function_call_output","call_id":"orphan_id",` +
				`"output":"output Z"}]}`),
			"assistant,user,user,user,user", "", sdk.FormatOpenAIResponse,
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
					var wire []byte
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(request *http.Request) (*http.Response, error) {
							calls++
							if request.URL.Host != "payload-contract.invalid" ||
								request.URL.Path != "/v1/chat/completions" {
								return nil, fmt.Errorf("unexpected isolated route %s", request.URL)
							}
							var err error
							wire, err = io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							body := `{"id":"synthetic","choices":[{"message":{"role":"assistant","content":` +
								`"ok"},"finish_reason":` +
								`"stop"}]}`
							if stream {
								body = "data: {\"id\":\"synthetic\",\"choices\":[{\"delta\":{\"content\":\"ok" +
									"\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
							}
							return googlePayloadResponse(request, body), nil
						},
					}
					ctx := util.WithRoundTripper(sdk.WithPluginHooks(t.Context(), nil), transport)
					cfg := &config.Config{}
					credential := &auth.Auth{
						ID: t.Name(), Provider: "openai", Attributes: map[string]string{
							"base_url": "https://payload-contract.invalid/v1", "api_key": "synthetic",
						},
					}
					if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport {
						t.Fatal("actual OpenAI-compatible transport is not isolated")
					}
					payload := []byte(`{"model":"deepseek-v4.1-flash",` + tc.body[1:])
					original := bytes.Clone(payload)
					request := core.Request{Model: "deepseek-v4.1-flash", Payload: payload}
					opts := core.Options{
						SourceFormat:   tc.format,
						ResponseFormat: sdk.FormatOpenAI,
						Stream:         stream,
					}
					implementation := NewOpenAICompatExecutor("openai", cfg)
					var err error
					if stream {
						var result *core.StreamResult
						result, err = implementation.ExecuteStream(ctx, credential, request, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = implementation.Execute(ctx, credential, request, opts)
					}
					if err != nil || calls != 1 || !bytes.Equal(payload, original) {
						t.Fatalf(
							"final transport error=%v calls=%d input unchanged=%t", err, calls,
							bytes.Equal(payload, original),
						)
					}
					var roles, results []string
					for _, message := range gjson.GetBytes(wire, "messages").Array() {
						roles = append(roles, message.Get("role").String())
						if message.Get("role").String() == "tool" {
							results = append(results, message.Get("tool_call_id").String())
						}
					}
					if strings.Join(roles, ",") != tc.roles || strings.Join(results, ",") != tc.results {
						t.Fatalf(
							"final roles=%v results=%v want=%s / %s: %s",
							roles,
							results,
							tc.roles,
							tc.results,
							wire,
						)
					}
					if tc.name == "Claude parallel results" && (gjson.GetBytes(
						wire, "messages.2.content",
					).String() != "metric_ok" || gjson.GetBytes(wire, "messages.3.content").String() != "data_ok") {
						t.Fatalf("tool result content changed: %s", wire)
					}
				},
			)
		}
	}
}
